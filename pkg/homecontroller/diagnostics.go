package homecontroller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhoelzel/simpleapp/pkg/version"
)

// BodyPreviewLimit is the maximum number of request-body bytes retained for
// diagnostics. CollectRequestDetails restores every byte it reads to r.Body.
const BodyPreviewLimit = 64 * 1024

const redactedDiagnosticValue = "[REDACTED]"

var diagnosticProcessStartedAt = time.Now()

// DiagnosticValues represents every value associated with a header, query
// parameter, or trailer. Collections of DiagnosticValues are sorted by Name;
// Values retain their original order because order can be meaningful to a
// proxy chain.
type DiagnosticValues struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// SocketAddress exposes both the address as supplied by net/http and a parsed
// host and port. net.SplitHostPort is used so IPv6 addresses remain intact.
type SocketAddress struct {
	Network string `json:"network,omitempty"`
	Address string `json:"address,omitempty"`
	Host    string `json:"host,omitempty"`
	Port    string `json:"port,omitempty"`
}

// ClientClaim is an address asserted by a forwarding header. It is deliberately
// separate from RequestDetails.RemoteAddr and is never marked as trusted: only
// the deployment knows which upstream proxy CIDRs are authoritative.
type ClientClaim struct {
	Source  string `json:"source,omitempty"`
	Value   string `json:"value,omitempty"`
	Trusted bool   `json:"trusted"`
}

// TLSDetails contains a display-safe summary of the connection's TLS state.
// Certificate contents and other credential material are intentionally not
// copied into diagnostics.
type TLSDetails struct {
	Enabled              bool   `json:"enabled"`
	Version              string `json:"version,omitempty"`
	CipherSuite          string `json:"cipherSuite,omitempty"`
	ServerName           string `json:"serverName,omitempty"`
	NegotiatedProtocol   string `json:"negotiatedProtocol,omitempty"`
	DidResume            bool   `json:"didResume"`
	PeerCertificateCount int    `json:"peerCertificateCount"`
	VerifiedChainCount   int    `json:"verifiedChainCount"`
}

// RequestDetails is a template- and JSON-friendly snapshot of the request data
// visible to net/http after any load balancer or reverse proxy has processed it.
// Header and query credentials are redacted unless SHOW_SENSITIVE_DATA=true.
type RequestDetails struct {
	CollectedAt         time.Time          `json:"collectedAt"`
	Method              string             `json:"method"`
	Scheme              string             `json:"scheme"`
	Proto               string             `json:"proto"`
	Host                string             `json:"host"`
	RequestURI          string             `json:"requestURI"`
	Path                string             `json:"path"`
	RawQuery            string             `json:"rawQuery"`
	RemoteAddr          string             `json:"remoteAddr"`
	LocalAddr           string             `json:"localAddr"`
	RemoteSocket        SocketAddress      `json:"remoteSocket"`
	LocalSocket         SocketAddress      `json:"localSocket"`
	ReportedClient      ClientClaim        `json:"reportedClient"`
	ContentLength       int64              `json:"contentLength"`
	TransferEncoding    []string           `json:"transferEncoding"`
	Headers             []DiagnosticValues `json:"headers"`
	Query               []DiagnosticValues `json:"query"`
	Trailers            []DiagnosticValues `json:"trailers"`
	TrailersComplete    bool               `json:"trailersComplete"`
	LoadBalancerHeaders []DiagnosticValues `json:"loadBalancerHeaders"`
	BodyPreview         string             `json:"bodyPreview"`
	BodyPreviewEncoding string             `json:"bodyPreviewEncoding"`
	BodyPreviewBytes    int                `json:"bodyPreviewBytes"`
	BodyPreviewLimit    int                `json:"bodyPreviewLimit"`
	BodyTruncated       bool               `json:"bodyTruncated"`
	TLS                 TLSDetails         `json:"tls"`
	RawRequest          string             `json:"rawRequest"`
	SensitiveDataShown  bool               `json:"sensitiveDataShown"`
	CollectionErrors    []string           `json:"collectionErrors,omitempty"`
}

// BuildDetails is the build metadata embedded by the project's linker flags.
type BuildDetails struct {
	Release   string `json:"release"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	Image     string `json:"image,omitempty"`
}

// KubernetesResourceDetails contains only explicitly selected Downward API
// resource fields. Environment variables are never enumerated wholesale.
type KubernetesResourceDetails struct {
	CPURequestMillicores    string `json:"cpuRequestMillicores,omitempty"`
	CPULimitMillicores      string `json:"cpuLimitMillicores,omitempty"`
	MemoryRequestBytes      string `json:"memoryRequestBytes,omitempty"`
	MemoryLimitBytes        string `json:"memoryLimitBytes,omitempty"`
	EphemeralStorageRequest string `json:"ephemeralStorageRequest,omitempty"`
	EphemeralStorageLimit   string `json:"ephemeralStorageLimit,omitempty"`
}

// KubernetesDetails contains a conservative allowlist of values commonly
// injected with the Kubernetes Downward API.
type KubernetesDetails struct {
	PodName        string                    `json:"podName,omitempty"`
	PodNamespace   string                    `json:"podNamespace,omitempty"`
	PodIP          string                    `json:"podIP,omitempty"`
	PodIPs         string                    `json:"podIPs,omitempty"`
	PodUID         string                    `json:"podUID,omitempty"`
	HostIP         string                    `json:"hostIP,omitempty"`
	NodeName       string                    `json:"nodeName,omitempty"`
	ServiceAccount string                    `json:"serviceAccount,omitempty"`
	ContainerName  string                    `json:"containerName,omitempty"`
	Resources      KubernetesResourceDetails `json:"resources"`
}

// NetworkInterfaceDetails is a stable, display-ready view of a local network
// interface. Interfaces and their addresses are sorted before being returned.
type NetworkInterfaceDetails struct {
	Index           int      `json:"index"`
	Name            string   `json:"name"`
	MTU             int      `json:"mtu"`
	HardwareAddress string   `json:"hardwareAddress,omitempty"`
	Flags           string   `json:"flags"`
	Addresses       []string `json:"addresses"`
}

// MemoryDetails provides lightweight Go runtime memory statistics useful in a
// demonstration pod.
type MemoryDetails struct {
	AllocBytes      uint64 `json:"allocBytes"`
	TotalAllocBytes uint64 `json:"totalAllocBytes"`
	SysBytes        uint64 `json:"sysBytes"`
	HeapAllocBytes  uint64 `json:"heapAllocBytes"`
	HeapObjects     uint64 `json:"heapObjects"`
	NumGC           uint32 `json:"numGC"`
}

// RuntimeDetails contains process, build, Kubernetes and network diagnostics.
type RuntimeDetails struct {
	Build            BuildDetails              `json:"build"`
	Hostname         string                    `json:"hostname"`
	CurrentTime      time.Time                 `json:"currentTime"`
	ProcessStartedAt time.Time                 `json:"processStartedAt"`
	Uptime           string                    `json:"uptime"`
	UptimeSeconds    float64                   `json:"uptimeSeconds"`
	GoVersion        string                    `json:"goVersion"`
	OS               string                    `json:"os"`
	Arch             string                    `json:"arch"`
	PID              int                       `json:"pid"`
	UID              int                       `json:"uid"`
	GID              int                       `json:"gid"`
	Goroutines       int                       `json:"goroutines"`
	CPUs             int                       `json:"cpus"`
	Memory           MemoryDetails             `json:"memory"`
	Kubernetes       KubernetesDetails         `json:"kubernetes"`
	Interfaces       []NetworkInterfaceDetails `json:"interfaces"`
	LocalIPs         []string                  `json:"localIPs"`
	OutboundIP       string                    `json:"outboundIP,omitempty"`
	CollectionErrors []string                  `json:"collectionErrors,omitempty"`
}

// CollectRequestDetails takes a bounded snapshot of an HTTP request. It reads
// at most BodyPreviewLimit+1 bytes and reconstructs r.Body from those bytes and
// the unread tail, so downstream handlers still receive the complete body.
func CollectRequestDetails(r *http.Request) RequestDetails {
	showSensitive := showSensitiveDiagnosticData()
	details := RequestDetails{
		CollectedAt:         time.Now().UTC(),
		ContentLength:       -1,
		BodyPreviewLimit:    BodyPreviewLimit,
		SensitiveDataShown:  showSensitive,
		TransferEncoding:    []string{},
		Headers:             []DiagnosticValues{},
		Query:               []DiagnosticValues{},
		Trailers:            []DiagnosticValues{},
		LoadBalancerHeaders: []DiagnosticValues{},
		CollectionErrors:    []string{},
	}
	if r == nil {
		details.CollectionErrors = append(details.CollectionErrors, "request is nil")
		return details
	}

	details.Method = r.Method
	details.Proto = r.Proto
	details.Host = r.Host
	details.RemoteAddr = r.RemoteAddr
	details.RemoteSocket = parseSocketAddress("tcp", r.RemoteAddr)
	details.ContentLength = r.ContentLength
	details.TransferEncoding = append([]string{}, r.TransferEncoding...)
	details.Scheme = "http"
	if r.TLS != nil {
		details.Scheme = "https"
	}

	if localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && localAddr != nil {
		details.LocalAddr = localAddr.String()
		details.LocalSocket = parseSocketAddress(localAddr.Network(), details.LocalAddr)
	}

	safeURL, malformedQuery := diagnosticURL(r.URL, showSensitive)
	if malformedQuery {
		details.CollectionErrors = append(details.CollectionErrors, "query contains malformed escaping; unsafe segments were omitted")
	}
	if safeURL != nil {
		details.Path = safeURL.Path
		details.RawQuery = safeURL.RawQuery
		details.Query = sortedDiagnosticValues(safeURL.Query())
	}
	details.RequestURI = diagnosticRequestURI(r, safeURL, showSensitive)

	safeHeaders := sanitizedHeader(r.Header, showSensitive)
	details.Headers = sortedDiagnosticValues(safeHeaders)
	details.LoadBalancerHeaders = highlightedLoadBalancerHeaders(details.Headers)
	details.ReportedClient = reportedClientClaim(safeHeaders)

	preview, previewEncoding, previewBytes, truncated, bodyErr := previewAndRestoreBody(r)
	details.BodyPreview = preview
	details.BodyPreviewEncoding = previewEncoding
	details.BodyPreviewBytes = previewBytes
	details.BodyTruncated = truncated
	if bodyErr != nil {
		details.CollectionErrors = append(details.CollectionErrors, "body preview: "+bodyErr.Error())
	}

	details.Trailers = sortedDiagnosticValues(sanitizedHeader(r.Trailer, showSensitive))
	details.TrailersComplete = !truncated && bodyErr == nil
	details.TLS = summarizeTLS(r.TLS)

	rawRequest, err := dumpDiagnosticRequest(r, safeURL, safeHeaders, details.RequestURI, showSensitive)
	if err != nil {
		details.CollectionErrors = append(details.CollectionErrors, "raw request: "+err.Error())
	} else {
		details.RawRequest = rawRequest
	}

	return details
}

// CollectRuntimeDetails gathers local process and network information without
// contacting an external HTTP service and without terminating the process when
// discovery is unavailable.
func CollectRuntimeDetails() RuntimeDetails {
	collectedAt := time.Now()
	now := collectedAt.UTC()
	startedAt := diagnosticProcessStartedAt.UTC()
	uptime := collectedAt.Sub(diagnosticProcessStartedAt)
	if uptime < 0 {
		uptime = 0
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	details := RuntimeDetails{
		Build: BuildDetails{
			Release:   version.Release,
			Commit:    version.Commit,
			BuildTime: version.BuildTime,
			Image:     firstEnvironmentValue("BUILD_IMAGE", "CONTAINER_IMAGE"),
		},
		CurrentTime:      now,
		ProcessStartedAt: startedAt,
		Uptime:           uptime.Round(time.Millisecond).String(),
		UptimeSeconds:    uptime.Seconds(),
		GoVersion:        runtime.Version(),
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		PID:              os.Getpid(),
		UID:              os.Getuid(),
		GID:              os.Getgid(),
		Goroutines:       runtime.NumGoroutine(),
		CPUs:             runtime.NumCPU(),
		Memory: MemoryDetails{
			AllocBytes:      mem.Alloc,
			TotalAllocBytes: mem.TotalAlloc,
			SysBytes:        mem.Sys,
			HeapAllocBytes:  mem.HeapAlloc,
			HeapObjects:     mem.HeapObjects,
			NumGC:           mem.NumGC,
		},
		Kubernetes: KubernetesDetails{
			PodName:        firstEnvironmentValue("POD_NAME", "K8S_POD_NAME"),
			PodNamespace:   firstEnvironmentValue("POD_NAMESPACE", "K8S_POD_NAMESPACE"),
			PodIP:          firstEnvironmentValue("POD_IP", "K8S_POD_IP"),
			PodIPs:         firstEnvironmentValue("POD_IPS", "K8S_POD_IPS"),
			PodUID:         firstEnvironmentValue("POD_UID", "K8S_POD_UID"),
			HostIP:         firstEnvironmentValue("HOST_IP", "K8S_HOST_IP"),
			NodeName:       firstEnvironmentValue("NODE_NAME", "K8S_NODE_NAME"),
			ServiceAccount: firstEnvironmentValue("SERVICE_ACCOUNT", "SERVICE_ACCOUNT_NAME", "K8S_SERVICE_ACCOUNT_NAME"),
			ContainerName:  firstEnvironmentValue("CONTAINER_NAME", "K8S_CONTAINER_NAME"),
			Resources: KubernetesResourceDetails{
				CPURequestMillicores:    firstEnvironmentValue("CPU_REQUEST_MILLICORES", "CPU_REQUEST"),
				CPULimitMillicores:      firstEnvironmentValue("CPU_LIMIT_MILLICORES", "CPU_LIMIT"),
				MemoryRequestBytes:      firstEnvironmentValue("MEMORY_REQUEST_BYTES", "MEMORY_REQUEST"),
				MemoryLimitBytes:        firstEnvironmentValue("MEMORY_LIMIT_BYTES", "MEMORY_LIMIT"),
				EphemeralStorageRequest: os.Getenv("EPHEMERAL_STORAGE_REQUEST"),
				EphemeralStorageLimit:   os.Getenv("EPHEMERAL_STORAGE_LIMIT"),
			},
		},
		Interfaces:       []NetworkInterfaceDetails{},
		LocalIPs:         []string{},
		CollectionErrors: []string{},
	}

	hostname, err := os.Hostname()
	if err != nil {
		details.CollectionErrors = append(details.CollectionErrors, "hostname: "+err.Error())
	} else {
		details.Hostname = hostname
	}

	interfaces, localIPs, interfaceErrors := collectNetworkInterfaces()
	details.Interfaces = interfaces
	details.LocalIPs = localIPs
	details.CollectionErrors = append(details.CollectionErrors, interfaceErrors...)

	outboundIP, err := preferredOutboundIP()
	if err != nil {
		details.CollectionErrors = append(details.CollectionErrors, "outbound IP: "+err.Error())
	} else {
		details.OutboundIP = outboundIP
	}

	return details
}

// SetDiagnosticResponseHeaders prevents reflected request credentials and
// topology data from being cached or embedded, and gives callers a safe default
// Content Security Policy. The route remains free to choose HTML or JSON as its
// Content-Type after calling this helper.
func SetDiagnosticResponseHeaders(w http.ResponseWriter) {
	if w == nil {
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'")
}

func showSensitiveDiagnosticData() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SHOW_SENSITIVE_DATA")), "true")
}

func sanitizedHeader(header http.Header, showSensitive bool) http.Header {
	result := make(http.Header, len(header))
	for name, values := range header {
		copied := append([]string(nil), values...)
		if !showSensitive && isSensitiveDiagnosticName(name) {
			for i := range copied {
				copied[i] = redactedDiagnosticValue
			}
		}
		result[name] = copied
	}
	return result
}

func diagnosticURL(original *url.URL, showSensitive bool) (*url.URL, bool) {
	if original == nil {
		return nil, false
	}
	cloned := *original
	if showSensitive {
		return &cloned, false
	}

	values, err := url.ParseQuery(original.RawQuery)
	if err == nil {
		redactedRawQuery, redacted := redactRawQuery(original.RawQuery, values)
		if redacted {
			cloned.RawQuery = redactedRawQuery
		}
	} else {
		// Fail closed for malformed escaping or separators: keep only the
		// portions ParseQuery could validate, and normalize those values.
		cloned.RawQuery = sanitizedQueryValues(values).Encode()
	}
	if cloned.User != nil {
		cloned.User = url.User(redactedDiagnosticValue)
	}
	return &cloned, err != nil
}

func redactRawQuery(rawQuery string, values url.Values) (string, bool) {
	needsRedaction := false
	for name := range values {
		if isSensitiveDiagnosticName(name) {
			needsRedaction = true
			break
		}
	}
	if !needsRedaction {
		return rawQuery, false
	}

	parts := strings.Split(rawQuery, "&")
	for index, part := range parts {
		rawName := part
		if equals := strings.IndexByte(part, '='); equals >= 0 {
			rawName = part[:equals]
		}
		name, err := url.QueryUnescape(rawName)
		if err != nil || !isSensitiveDiagnosticName(name) {
			continue
		}
		parts[index] = rawName + "=" + url.QueryEscape(redactedDiagnosticValue)
	}
	return strings.Join(parts, "&"), true
}

func sanitizedQueryValues(values url.Values) url.Values {
	sanitized := make(url.Values, len(values))
	for name, entries := range values {
		entries = append([]string(nil), entries...)
		if isSensitiveDiagnosticName(name) {
			for i := range entries {
				entries[i] = redactedDiagnosticValue
			}
		}
		sanitized[name] = entries
	}
	return sanitized
}

func diagnosticRequestURI(r *http.Request, safeURL *url.URL, showSensitive bool) string {
	if showSensitive && r.RequestURI != "" {
		return r.RequestURI
	}
	if r.RequestURI == "*" {
		return "*"
	}
	if safeURL != nil {
		return safeURL.RequestURI()
	}
	return ""
}

func isSensitiveDiagnosticName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	compact := strings.NewReplacer("-", "", ".", "", "[", "", "]", "").Replace(normalized)
	for _, configured := range strings.Split(os.Getenv("DIAGNOSTIC_REDACT_NAMES"), ",") {
		configured = strings.ToLower(strings.TrimSpace(configured))
		configured = strings.NewReplacer("-", "", "_", "", ".", "", "[", "", "]", "").Replace(configured)
		if configured != "" && configured == compact {
			return true
		}
	}

	switch compact {
	case "authorization", "proxyauthorization", "authentication", "xauth", "xauthorization", "cookie", "setcookie",
		"apikey", "xapikey", "accesskey", "secretkey", "password", "passwd", "pwd",
		"token", "accesstoken", "refreshtoken", "idtoken", "authtoken", "xauthtoken",
		"secret", "clientsecret", "credential", "credentials", "signature", "sig", "code",
		"xamzcredential", "xamzsignature", "xamzsecuritytoken", "xgoogcredential", "xgoogsignature",
		"xforwardedclientcert", "xsslclientcert", "sslclientcert", "xclientcert", "xclientcertificate",
		"cfaccessjwtassertion", "xgoogiapjwtassertion", "xamznoidcdata", "xamznoidcaccesstoken",
		"samlrequest", "samlresponse":
		return true
	}

	for _, part := range strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '[' || r == ']'
	}) {
		switch part {
		case "auth", "authorization", "authentication", "password", "passwd", "token", "secret", "credential", "signature", "apikey", "jwt", "assertion":
			return true
		}
	}
	return false
}

func sortedDiagnosticValues(values map[string][]string) []DiagnosticValues {
	result := make([]DiagnosticValues, 0, len(values))
	for name, entries := range values {
		result = append(result, DiagnosticValues{
			Name:   name,
			Values: append([]string(nil), entries...),
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		left := strings.ToLower(result[i].Name)
		right := strings.ToLower(result[j].Name)
		if left == right {
			return result[i].Name < result[j].Name
		}
		return left < right
	})
	return result
}

func parseSocketAddress(network, address string) SocketAddress {
	result := SocketAddress{Network: network, Address: address}
	if address == "" {
		return result
	}
	host, port, err := net.SplitHostPort(address)
	if err == nil {
		result.Host = host
		result.Port = port
		return result
	}

	trimmed := strings.TrimPrefix(strings.TrimSuffix(address, "]"), "[")
	if zoneIndex := strings.LastIndex(trimmed, "%"); zoneIndex >= 0 {
		if net.ParseIP(trimmed[:zoneIndex]) != nil {
			result.Host = trimmed
			return result
		}
	}
	if net.ParseIP(trimmed) != nil {
		result.Host = trimmed
	}
	return result
}

func previewAndRestoreBody(r *http.Request) (string, string, int, bool, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return "", "utf-8", 0, false, nil
	}

	original := r.Body
	limited := &io.LimitedReader{R: original, N: BodyPreviewLimit + 1}
	consumed, err := io.ReadAll(limited)
	r.Body = &replayReadCloser{
		Reader: io.MultiReader(bytes.NewReader(consumed), original),
		Closer: original,
	}

	truncated := len(consumed) > BodyPreviewLimit
	preview := consumed
	if truncated {
		preview = consumed[:BodyPreviewLimit]
	}
	if displayableUTF8(preview) {
		return string(preview), "utf-8", len(preview), truncated, err
	}
	return base64.StdEncoding.EncodeToString(preview), "base64", len(preview), truncated, err
}

type replayReadCloser struct {
	io.Reader
	io.Closer
}

func displayableUTF8(value []byte) bool {
	if !utf8.Valid(value) {
		return false
	}
	for len(value) > 0 {
		r, size := utf8.DecodeRune(value)
		value = value[size:]
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func summarizeTLS(state *tls.ConnectionState) TLSDetails {
	if state == nil {
		return TLSDetails{}
	}
	return TLSDetails{
		Enabled:              true,
		Version:              tlsVersionName(state.Version),
		CipherSuite:          tls.CipherSuiteName(state.CipherSuite),
		ServerName:           state.ServerName,
		NegotiatedProtocol:   state.NegotiatedProtocol,
		DidResume:            state.DidResume,
		PeerCertificateCount: len(state.PeerCertificates),
		VerifiedChainCount:   len(state.VerifiedChains),
	}
}

func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("unknown (0x%04x)", version)
	}
}

func highlightedLoadBalancerHeaders(headers []DiagnosticValues) []DiagnosticValues {
	result := make([]DiagnosticValues, 0)
	for _, header := range headers {
		if isLoadBalancerHeader(header.Name) {
			result = append(result, DiagnosticValues{
				Name:   header.Name,
				Values: append([]string(nil), header.Values...),
			})
		}
	}
	return result
}

func isLoadBalancerHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "forwarded", "via", "x-real-ip", "true-client-ip", "fastly-client-ip", "fly-client-ip",
		"cf-connecting-ip", "cf-connecting-ipv6", "cf-ray", "x-request-id", "x-correlation-id":
		return true
	}
	for _, prefix := range []string{"x-forwarded-", "x-original-", "x-envoy-", "x-amzn-", "x-azure-", "x-vercel-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func reportedClientClaim(header http.Header) ClientClaim {
	if forwarded := header.Values("Forwarded"); len(forwarded) > 0 {
		if value := firstForwardedFor(forwarded); value != "" {
			return ClientClaim{Source: "Forwarded", Value: value}
		}
	}
	for _, name := range []string{"CF-Connecting-IP", "True-Client-IP", "X-Forwarded-For", "X-Real-IP", "Fastly-Client-IP", "Fly-Client-IP", "X-Envoy-External-Address"} {
		for _, rawValue := range header.Values(name) {
			value := strings.TrimSpace(strings.SplitN(rawValue, ",", 2)[0])
			if value != "" {
				return ClientClaim{Source: name, Value: value}
			}
		}
	}
	return ClientClaim{}
}

func firstForwardedFor(values []string) string {
	for _, value := range values {
		for _, element := range strings.Split(value, ",") {
			for _, parameter := range strings.Split(element, ";") {
				nameAndValue := strings.SplitN(parameter, "=", 2)
				if len(nameAndValue) != 2 || !strings.EqualFold(strings.TrimSpace(nameAndValue[0]), "for") {
					continue
				}
				claim := strings.TrimSpace(nameAndValue[1])
				if len(claim) >= 2 && claim[0] == '"' && claim[len(claim)-1] == '"' {
					if unquoted, err := strconv.Unquote(claim); err == nil {
						claim = unquoted
					}
				}
				return claim
			}
		}
	}
	return ""
}

func dumpDiagnosticRequest(r *http.Request, safeURL *url.URL, safeHeader http.Header, safeRequestURI string, showSensitive bool) (string, error) {
	cloned := new(http.Request)
	*cloned = *r
	cloned.Header = sanitizedHeader(safeHeader, showSensitive)
	cloned.Trailer = sanitizedHeader(r.Trailer, showSensitive)
	cloned.URL = safeURL
	cloned.RequestURI = safeRequestURI
	raw, err := httputil.DumpRequest(cloned, false)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func firstEnvironmentValue(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func collectNetworkInterfaces() ([]NetworkInterfaceDetails, []string, []string) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return []NetworkInterfaceDetails{}, []string{}, []string{"network interfaces: " + err.Error()}
	}

	result := make([]NetworkInterfaceDetails, 0, len(interfaces))
	localIPSet := make(map[string]struct{})
	errors := make([]string, 0)
	for _, iface := range interfaces {
		item := NetworkInterfaceDetails{
			Index:           iface.Index,
			Name:            iface.Name,
			MTU:             iface.MTU,
			HardwareAddress: iface.HardwareAddr.String(),
			Flags:           iface.Flags.String(),
			Addresses:       []string{},
		}
		addresses, addressErr := iface.Addrs()
		if addressErr != nil {
			errors = append(errors, "interface "+iface.Name+": "+addressErr.Error())
		} else {
			for _, address := range addresses {
				item.Addresses = append(item.Addresses, address.String())
				if ip := ipFromAddress(address.String()); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
					localIPSet[ip.String()] = struct{}{}
				}
			}
		}
		sort.Strings(item.Addresses)
		result = append(result, item)
	}

	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Index < result[j].Index
		}
		return result[i].Name < result[j].Name
	})
	localIPs := make([]string, 0, len(localIPSet))
	for ip := range localIPSet {
		localIPs = append(localIPs, ip)
	}
	sort.Strings(localIPs)
	sort.Strings(errors)
	return result, localIPs, errors
}

func ipFromAddress(address string) net.IP {
	if ip, _, err := net.ParseCIDR(address); err == nil {
		return ip
	}
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		if zoneIndex := strings.LastIndex(host, "%"); zoneIndex >= 0 {
			host = host[:zoneIndex]
		}
		return net.ParseIP(host)
	}
	if zoneIndex := strings.LastIndex(address, "%"); zoneIndex >= 0 {
		address = address[:zoneIndex]
	}
	return net.ParseIP(strings.Trim(address, "[]"))
}

// preferredOutboundIP asks the kernel which local address it would use for an
// IPv4 UDP route. Dialing UDP does not send application data; the connection is
// immediately closed. Failure is returned as diagnostics rather than fatal.
func preferredOutboundIP() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "udp4", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer connection.Close()
	host, _, err := net.SplitHostPort(connection.LocalAddr().String())
	if err != nil {
		return "", err
	}
	return host, nil
}
