package homecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	portCheckBodyLimit        = 64 << 10
	portCheckDefaultTimeout   = 500 * time.Millisecond
	portCheckMaximumTimeout   = 5 * time.Second
	portCheckOverallTimeout   = 20 * time.Second
	portCheckMaximumWorkers   = 128
	portCheckMaximumExplicit  = 4096
	portCheckEchoPayload      = "simpleapp-port-check"
	portCheckAllowedTargetEnv = "PORT_CHECK_ALLOWED_TARGETS"
)

var (
	errInvalidTarget    = errors.New("invalid target")
	errTargetNotAllowed = errors.New("target not allowed")
	errTargetResolution = errors.New("target resolution failed")
	errTooManyPorts     = errors.New("too many explicit ports")

	// A scan can create many sockets. Keep that load bounded across the process,
	// not merely within one request.
	portCheckScanSlot = make(chan struct{}, 1)
)

// PortCheckRequest is the JSON payload accepted by PortCheckEndpoint.
type PortCheckRequest struct {
	Target    string `json:"target"`
	Ports     string `json:"ports"`
	Protocol  string `json:"protocol"`
	TimeoutMS int    `json:"timeout_ms"`
}

// PortCheckTiming describes both the request-wide and per-port limits.
type PortCheckTiming struct {
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	DurationMS       int64     `json:"duration_ms"`
	PerPortTimeoutMS int       `json:"per_port_timeout_ms"`
	OverallTimeoutMS int       `json:"overall_timeout_ms"`
}

// PortCheckResult is one completed protocol/port probe. TCP can report open;
// UDP reports responded only after receiving a datagram from the target.
type PortCheckResult struct {
	Port       int    `json:"port"`
	Protocol   string `json:"protocol"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// PortCheckResponse is returned by PortCheckEndpoint and can also be consumed
// directly by the dashboard template.
type PortCheckResponse struct {
	Target          string            `json:"target"`
	ResolvedTarget  string            `json:"resolved_target"`
	NormalizedPorts string            `json:"normalized_ports"`
	AllPorts        bool              `json:"all_ports"`
	Ports           []int             `json:"ports,omitempty"`
	Protocols       []string          `json:"protocols"`
	Timing          PortCheckTiming   `json:"timing"`
	Total           int               `json:"total"`
	Completed       int               `json:"completed"`
	StatusCounts    map[string]int    `json:"status_counts"`
	Partial         bool              `json:"partial"`
	Cancelled       bool              `json:"cancelled"`
	Results         []PortCheckResult `json:"results"`
}

type portCheckErrorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type portCheckJob struct {
	port     int
	protocol string
}

type portCheckSummary struct {
	completed    int
	statusCounts map[string]int
	results      []PortCheckResult
}

type portCheckProbe func(context.Context, string, int, string, time.Duration) PortCheckResult

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type targetPolicy struct {
	allowAll     bool
	allowedIPs   map[string]struct{}
	allowedNames map[string]struct{}
	allowedCIDRs []*net.IPNet
}

// PortCheckEndpoint handles POST /api/port-check. Register it with:
//
//	r.HandleFunc("/api/port-check", homecontroller.PortCheckEndpoint)
//
// The method is checked here as well, so registration does not need to depend
// on router-specific method filtering.
func PortCheckEndpoint(w http.ResponseWriter, r *http.Request) {
	SetDiagnosticResponseHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePortCheckError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST with a JSON request body")
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writePortCheckError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}

	request, err := decodePortCheckRequest(w, r)
	if err != nil {
		writePortCheckError(w, http.StatusBadRequest, "invalid_json", "request body must be one valid JSON object with no unknown fields")
		return
	}

	ports, allPorts, err := parsePortCheckPorts(request.Ports)
	if err != nil {
		if errors.Is(err, errTooManyPorts) {
			writePortCheckError(w, http.StatusBadRequest, "too_many_ports", "explicit port lists are limited to 4096 ports; use all for a full scan")
			return
		}
		writePortCheckError(w, http.StatusBadRequest, "invalid_ports", "ports must be comma-separated ports or ranges, or the sole value all")
		return
	}

	protocols, err := parsePortCheckProtocols(request.Protocol)
	if err != nil {
		writePortCheckError(w, http.StatusBadRequest, "invalid_protocol", "protocol must be tcp, udp, or both")
		return
	}

	perPortTimeout, err := parsePortCheckTimeout(request.TimeoutMS)
	if err != nil {
		writePortCheckError(w, http.StatusBadRequest, "invalid_timeout", "timeout_ms must be between 1 and 5000, or 0 for the default")
		return
	}

	if !acquirePortCheckScan() {
		w.Header().Set("Retry-After", "1")
		writePortCheckError(w, http.StatusTooManyRequests, "scan_in_progress", "another port check is already running")
		return
	}
	defer releasePortCheckScan()

	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(r.Context(), portCheckOverallTimeout)
	defer cancel()

	policy, err := loadTargetPolicy()
	if err != nil {
		writePortCheckError(w, http.StatusInternalServerError, "invalid_target_policy", "port-check target policy is invalid")
		return
	}

	target, resolvedTarget, err := resolvePortCheckTarget(ctx, request.Target, policy, net.DefaultResolver)
	if err != nil {
		switch {
		case errors.Is(err, errInvalidTarget):
			writePortCheckError(w, http.StatusBadRequest, "invalid_target", "target must be an IP address, hostname, localhost, or self")
		case errors.Is(err, errTargetNotAllowed):
			writePortCheckError(w, http.StatusForbidden, "target_not_allowed", "target is not permitted by the port-check policy")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			writePortCheckError(w, http.StatusGatewayTimeout, "scan_timeout", "the port check exceeded its overall time limit")
		default:
			writePortCheckError(w, http.StatusUnprocessableEntity, "target_unresolvable", "target could not be resolved")
		}
		return
	}

	summary := executePortCheck(ctx, resolvedTarget, ports, protocols, perPortTimeout, allPorts, probePort)
	finishedAt := time.Now().UTC()
	total := len(ports) * len(protocols)
	responsePorts := ports
	if allPorts {
		responsePorts = nil
	}
	response := PortCheckResponse{
		Target:          target,
		ResolvedTarget:  resolvedTarget,
		NormalizedPorts: normalizePortCheckPorts(ports, allPorts),
		AllPorts:        allPorts,
		Ports:           responsePorts,
		Protocols:       protocols,
		Timing: PortCheckTiming{
			StartedAt:        startedAt,
			FinishedAt:       finishedAt,
			DurationMS:       finishedAt.Sub(startedAt).Milliseconds(),
			PerPortTimeoutMS: int(perPortTimeout / time.Millisecond),
			OverallTimeoutMS: int(portCheckOverallTimeout / time.Millisecond),
		},
		Total:        total,
		Completed:    summary.completed,
		StatusCounts: summary.statusCounts,
		Partial:      summary.completed < total,
		Cancelled:    ctx.Err() != nil,
		Results:      summary.results,
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

func decodePortCheckRequest(w http.ResponseWriter, r *http.Request) (PortCheckRequest, error) {
	var request *PortCheckRequest
	r.Body = http.MaxBytesReader(w, r.Body, portCheckBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return PortCheckRequest{}, err
	}
	if request == nil {
		return PortCheckRequest{}, errors.New("request must be a JSON object")
	}

	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return PortCheckRequest{}, errors.New("request must contain one JSON value")
	}
	return *request, nil
}

func parsePortCheckPorts(raw string) ([]int, bool, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "all" {
		ports := make([]int, 65535)
		for i := range ports {
			ports[i] = i + 1
		}
		return ports, true, nil
	}
	if raw == "" || strings.Contains(raw, "all") {
		return nil, false, errors.New("missing or mixed port specification")
	}

	selected := make([]bool, 65536)
	selectedCount := 0
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, false, errors.New("empty port item")
		}

		parts := strings.Split(item, "-")
		switch len(parts) {
		case 1:
			port, err := parsePortNumber(parts[0])
			if err != nil {
				return nil, false, err
			}
			if !selected[port] {
				selected[port] = true
				selectedCount++
			}
		case 2:
			first, err := parsePortNumber(strings.TrimSpace(parts[0]))
			if err != nil {
				return nil, false, err
			}
			last, err := parsePortNumber(strings.TrimSpace(parts[1]))
			if err != nil || first > last {
				return nil, false, errors.New("invalid port range")
			}
			for port := first; port <= last; port++ {
				if !selected[port] {
					selected[port] = true
					selectedCount++
					if selectedCount > portCheckMaximumExplicit {
						return nil, false, errTooManyPorts
					}
				}
			}
		default:
			return nil, false, errors.New("invalid port range")
		}
		if selectedCount > portCheckMaximumExplicit {
			return nil, false, errTooManyPorts
		}
	}

	ports := make([]int, 0)
	for port := 1; port <= 65535; port++ {
		if selected[port] {
			ports = append(ports, port)
		}
	}
	if len(ports) == 0 {
		return nil, false, errors.New("no ports selected")
	}
	return ports, false, nil
}

func normalizePortCheckPorts(ports []int, allPorts bool) string {
	if allPorts {
		return "all"
	}
	if len(ports) == 0 {
		return ""
	}

	parts := make([]string, 0, len(ports))
	for firstIndex := 0; firstIndex < len(ports); {
		lastIndex := firstIndex
		for lastIndex+1 < len(ports) && ports[lastIndex+1] == ports[lastIndex]+1 {
			lastIndex++
		}
		if firstIndex == lastIndex {
			parts = append(parts, strconv.Itoa(ports[firstIndex]))
		} else {
			parts = append(parts, strconv.Itoa(ports[firstIndex])+"-"+strconv.Itoa(ports[lastIndex]))
		}
		firstIndex = lastIndex + 1
	}
	return strings.Join(parts, ",")
}

func parsePortNumber(raw string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("port outside valid range")
	}
	return port, nil
}

func parsePortCheckProtocols(raw string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "tcp":
		return []string{"tcp"}, nil
	case "udp":
		return []string{"udp"}, nil
	case "both":
		return []string{"tcp", "udp"}, nil
	default:
		return nil, errors.New("unsupported protocol")
	}
}

func parsePortCheckTimeout(timeoutMS int) (time.Duration, error) {
	if timeoutMS == 0 {
		return portCheckDefaultTimeout, nil
	}
	if timeoutMS < 1 || timeoutMS > int(portCheckMaximumTimeout/time.Millisecond) {
		return 0, errors.New("timeout outside valid range")
	}
	return time.Duration(timeoutMS) * time.Millisecond, nil
}

func acquirePortCheckScan() bool {
	select {
	case portCheckScanSlot <- struct{}{}:
		return true
	default:
		return false
	}
}

func releasePortCheckScan() {
	<-portCheckScanSlot
}

func loadTargetPolicy() (targetPolicy, error) {
	policy := targetPolicy{
		allowedIPs:   make(map[string]struct{}),
		allowedNames: make(map[string]struct{}),
	}
	policy.allowIP(net.ParseIP("127.0.0.1"))
	policy.allowIP(net.ParseIP("::1"))

	if addresses, err := net.InterfaceAddrs(); err == nil {
		for _, address := range addresses {
			if network, ok := address.(*net.IPNet); ok {
				policy.allowIP(network.IP)
			}
		}
	}

	if podIP := net.ParseIP(strings.TrimSpace(os.Getenv("POD_IP"))); podIP != nil {
		policy.allowIP(podIP)
	}

	if err := policy.addAllowedTargets(os.Getenv(portCheckAllowedTargetEnv)); err != nil {
		return targetPolicy{}, err
	}
	return policy, nil
}

func (policy *targetPolicy) addAllowedTargets(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return errors.New("empty allowed target")
		}
		if item == "*" {
			policy.allowAll = true
			continue
		}
		if strings.Contains(item, "/") {
			_, network, err := net.ParseCIDR(item)
			if err != nil {
				return errors.New("invalid allowed CIDR")
			}
			policy.allowedCIDRs = append(policy.allowedCIDRs, network)
			continue
		}
		if ip := net.ParseIP(item); ip != nil {
			policy.allowIP(ip)
			continue
		}

		name, err := normalizeHostname(item)
		if err != nil {
			return errors.New("invalid allowed hostname")
		}
		policy.allowedNames[name] = struct{}{}
	}
	return nil
}

func (policy *targetPolicy) allowIP(ip net.IP) {
	if ip != nil {
		policy.allowedIPs[canonicalIP(ip)] = struct{}{}
	}
}

func (policy targetPolicy) permitsIP(ip net.IP) bool {
	if policy.allowAll {
		return true
	}
	if _, ok := policy.allowedIPs[canonicalIP(ip)]; ok {
		return true
	}
	for _, network := range policy.allowedCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func resolvePortCheckTarget(ctx context.Context, rawTarget string, policy targetPolicy, resolver ipResolver) (string, string, error) {
	target, hostname, err := normalizePortCheckTarget(rawTarget)
	if err != nil {
		return "", "", err
	}
	if target == "self" {
		return target, "127.0.0.1", nil
	}

	if ip := net.ParseIP(target); ip != nil {
		if !policy.permitsIP(ip) {
			return "", "", errTargetNotAllowed
		}
		return canonicalIP(ip), canonicalIP(ip), nil
	}

	addresses, err := resolver.LookupIPAddr(ctx, hostname)
	if err != nil || len(addresses) == 0 {
		return "", "", errTargetResolution
	}

	_, hostnameExplicitlyAllowed := policy.allowedNames[hostname]
	resolved := make([]net.IP, 0, len(addresses))
	seen := make(map[string]struct{})
	for _, address := range addresses {
		if address.IP == nil || (!hostnameExplicitlyAllowed && !policy.permitsIP(address.IP)) {
			continue
		}
		canonical := canonicalIP(address.IP)
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		resolved = append(resolved, address.IP)
	}
	if len(resolved) == 0 {
		return "", "", errTargetNotAllowed
	}

	// Resolve once and select one stable literal. Prefer IPv4 for predictable
	// behavior on dual-stack localhost and then sort lexically.
	sort.Slice(resolved, func(i, j int) bool {
		i4 := resolved[i].To4() != nil
		j4 := resolved[j].To4() != nil
		if i4 != j4 {
			return i4
		}
		return canonicalIP(resolved[i]) < canonicalIP(resolved[j])
	})
	return hostname, canonicalIP(resolved[0]), nil
}

func normalizePortCheckTarget(raw string) (string, string, error) {
	target := strings.TrimSpace(raw)
	if strings.EqualFold(target, "self") {
		return "self", "", nil
	}
	if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		target = strings.TrimSuffix(strings.TrimPrefix(target, "["), "]")
	}
	if ip := net.ParseIP(target); ip != nil {
		return canonicalIP(ip), "", nil
	}

	hostname, err := normalizeHostname(target)
	if err != nil {
		return "", "", errInvalidTarget
	}
	return hostname, hostname, nil
}

func normalizeHostname(raw string) (string, error) {
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if hostname == "" || len(hostname) > 253 {
		return "", errInvalidTarget
	}
	for _, label := range strings.Split(hostname, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errInvalidTarget
		}
		for _, character := range label {
			if (character >= 'a' && character <= 'z') ||
				(character >= '0' && character <= '9') || character == '-' {
				continue
			}
			return "", errInvalidTarget
		}
	}
	return hostname, nil
}

func canonicalIP(ip net.IP) string {
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}
	return ip.String()
}

func executePortCheck(
	ctx context.Context,
	target string,
	ports []int,
	protocols []string,
	perPortTimeout time.Duration,
	retainActionableOnly bool,
	probe portCheckProbe,
) portCheckSummary {
	total := len(ports) * len(protocols)
	summary := portCheckSummary{
		statusCounts: make(map[string]int),
		results:      make([]PortCheckResult, 0),
	}
	if total == 0 {
		return summary
	}

	workerCount := total
	if workerCount > portCheckMaximumWorkers {
		workerCount = portCheckMaximumWorkers
	}
	jobs := make(chan portCheckJob, workerCount)
	results := make(chan PortCheckResult, workerCount)

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					result := probe(ctx, target, job.port, job.protocol, perPortTimeout)
					select {
					case results <- result:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, port := range ports {
			for _, protocol := range protocols {
				select {
				case jobs <- portCheckJob{port: port, protocol: protocol}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	go func() {
		workers.Wait()
		close(results)
	}()

	for result := range results {
		summary.completed++
		summary.statusCounts[result.Status]++
		if !retainActionableOnly || result.Status == "open" || result.Status == "responded" {
			summary.results = append(summary.results, result)
		}
	}

	sort.Slice(summary.results, func(i, j int) bool {
		if summary.results[i].Port != summary.results[j].Port {
			return summary.results[i].Port < summary.results[j].Port
		}
		return protocolSortOrder(summary.results[i].Protocol) < protocolSortOrder(summary.results[j].Protocol)
	})
	return summary
}

func protocolSortOrder(protocol string) int {
	if protocol == "tcp" {
		return 0
	}
	return 1
}

func probePort(ctx context.Context, target string, port int, protocol string, timeout time.Duration) PortCheckResult {
	if protocol == "udp" {
		return probeUDPPort(ctx, target, port, timeout)
	}
	return probeTCPPort(ctx, target, port, timeout)
}

func probeTCPPort(ctx context.Context, target string, port int, timeout time.Duration) PortCheckResult {
	startedAt := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	address := net.JoinHostPort(target, strconv.Itoa(port))
	connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", address)
	result := PortCheckResult{Port: port, Protocol: "tcp", DurationMS: time.Since(startedAt).Milliseconds()}
	if err == nil {
		_ = connection.Close()
		result.Status = "open"
		return result
	}

	result.Status, result.Detail = classifyTCPError(probeCtx, err)
	return result
}

func classifyTCPError(ctx context.Context, err error) (string, string) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled", "cancelled"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout", "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "closed", "connection_refused"
	}
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return "unreachable", "unreachable"
	}
	if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
		return "timeout", "timeout"
	}
	return "error", "connection_failed"
}

func probeUDPPort(ctx context.Context, target string, port int, timeout time.Duration) PortCheckResult {
	startedAt := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := PortCheckResult{Port: port, Protocol: "udp"}
	address := net.JoinHostPort(target, strconv.Itoa(port))
	connection, err := (&net.Dialer{}).DialContext(probeCtx, "udp", address)
	if err != nil {
		result.Status = "unknown"
		result.Detail = udpErrorDetail(probeCtx, err)
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}
	defer connection.Close()

	if deadline, ok := probeCtx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	stopDeadlineWatcher := make(chan struct{})
	go func() {
		select {
		case <-probeCtx.Done():
			_ = connection.SetDeadline(time.Now())
		case <-stopDeadlineWatcher:
		}
	}()
	defer close(stopDeadlineWatcher)

	if _, err := connection.Write([]byte(portCheckEchoPayload)); err != nil {
		result.Status = "unknown"
		result.Detail = udpErrorDetail(probeCtx, err)
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}

	buffer := make([]byte, 512)
	if _, err := connection.Read(buffer); err != nil {
		result.Status = "unknown"
		result.Detail = udpErrorDetail(probeCtx, err)
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}

	result.Status = "responded"
	result.DurationMS = time.Since(startedAt).Milliseconds()
	return result
}

func udpErrorDetail(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "no_response"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "no_response"
	}
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return "unreachable"
	}
	if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
		return "no_response"
	}
	return "probe_failed"
}

func writePortCheckError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(portCheckErrorResponse{Code: code, Error: message})
}
