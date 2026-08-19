package homecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type recordingResolver struct {
	addresses map[string][]net.IPAddr
	err       error
	calls     int
}

func (resolver *recordingResolver) LookupIPAddr(_ context.Context, hostname string) ([]net.IPAddr, error) {
	resolver.calls++
	if resolver.err != nil {
		return nil, resolver.err
	}
	return resolver.addresses[hostname], nil
}

func TestParsePortCheckPorts(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []int
		wantAll bool
		wantErr bool
	}{
		{name: "single values ranges and duplicates", raw: "443, 80, 81-83,443", want: []int{80, 81, 82, 83, 443}},
		{name: "all", raw: " ALL ", wantAll: true},
		{name: "lowest and highest", raw: "1,65535", want: []int{1, 65535}},
		{name: "empty", raw: "", wantErr: true},
		{name: "empty item", raw: "80,,443", wantErr: true},
		{name: "all must be sole value", raw: "all,443", wantErr: true},
		{name: "descending range", raw: "100-90", wantErr: true},
		{name: "zero", raw: "0", wantErr: true},
		{name: "too high", raw: "65536", wantErr: true},
		{name: "non numeric", raw: "http", wantErr: true},
		{name: "too many separators", raw: "1-2-3", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, gotAll, err := parsePortCheckPorts(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parsePortCheckPorts(%q) unexpectedly succeeded", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortCheckPorts(%q): %v", test.raw, err)
			}
			if gotAll != test.wantAll {
				t.Fatalf("all flag = %v, want %v", gotAll, test.wantAll)
			}
			if test.wantAll {
				if len(got) != 65535 || got[0] != 1 || got[len(got)-1] != 65535 {
					t.Fatalf("all expanded to an invalid set: length=%d first=%d last=%d", len(got), got[0], got[len(got)-1])
				}
				return
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ports = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParsePortCheckProtocols(t *testing.T) {
	tests := []struct {
		raw     string
		want    []string
		wantErr bool
	}{
		{raw: "TCP", want: []string{"tcp"}},
		{raw: " udp ", want: []string{"udp"}},
		{raw: "both", want: []string{"tcp", "udp"}},
		{raw: ""},
		{raw: "icmp"},
	}
	for index := range tests {
		test := tests[index]
		if test.want == nil {
			test.wantErr = true
		}
		got, err := parsePortCheckProtocols(test.raw)
		if test.wantErr {
			if err == nil {
				t.Errorf("parsePortCheckProtocols(%q) unexpectedly succeeded", test.raw)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parsePortCheckProtocols(%q) = %v, %v; want %v", test.raw, got, err, test.want)
		}
	}
}

func TestExplicitPortLimitAndNormalization(t *testing.T) {
	_, _, err := parsePortCheckPorts("1-4097")
	if !errors.Is(err, errTooManyPorts) {
		t.Fatalf("explicit expansion error = %v, want too-many-ports", err)
	}

	ports, allPorts, err := parsePortCheckPorts("443,80-82,81,1000-1002")
	if err != nil {
		t.Fatal(err)
	}
	if got := normalizePortCheckPorts(ports, allPorts); got != "80-82,443,1000-1002" {
		t.Fatalf("normalized ports = %q", got)
	}
	if got := normalizePortCheckPorts(nil, true); got != "all" {
		t.Fatalf("normalized all = %q", got)
	}
}

func TestTargetPolicyAndSingleResolution(t *testing.T) {
	policy := targetPolicy{allowedIPs: make(map[string]struct{}), allowedNames: make(map[string]struct{})}
	if err := policy.addAllowedTargets("10.0.0.0/8,service.example"); err != nil {
		t.Fatal(err)
	}
	resolver := &recordingResolver{addresses: map[string][]net.IPAddr{
		"inside.example":  {{IP: net.ParseIP("10.20.30.40")}, {IP: net.ParseIP("10.20.30.41")}},
		"service.example": {{IP: net.ParseIP("192.0.2.50")}},
		"outside.example": {{IP: net.ParseIP("192.0.2.60")}},
	}}

	target, resolved, err := resolvePortCheckTarget(context.Background(), "INSIDE.EXAMPLE.", policy, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if target != "inside.example" || resolved != "10.20.30.40" {
		t.Fatalf("resolved target = %q / %q", target, resolved)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver called %d times, want exactly once", resolver.calls)
	}

	_, resolved, err = resolvePortCheckTarget(context.Background(), "service.example", policy, resolver)
	if err != nil || resolved != "192.0.2.50" {
		t.Fatalf("explicit hostname was not permitted: resolved=%q err=%v", resolved, err)
	}

	_, _, err = resolvePortCheckTarget(context.Background(), "outside.example", policy, resolver)
	if !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("outside hostname error = %v, want target-not-allowed", err)
	}

	beforeLiteral := resolver.calls
	_, _, err = resolvePortCheckTarget(context.Background(), "192.0.2.60", policy, resolver)
	if !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("outside literal error = %v, want target-not-allowed", err)
	}
	if resolver.calls != beforeLiteral {
		t.Fatal("literal IP unexpectedly used DNS")
	}

	self, resolved, err := resolvePortCheckTarget(context.Background(), "self", policy, resolver)
	if err != nil || self != "self" || resolved != "127.0.0.1" {
		t.Fatalf("self resolution = %q / %q / %v", self, resolved, err)
	}
}

func TestTargetPolicyWildcard(t *testing.T) {
	policy := targetPolicy{allowedIPs: make(map[string]struct{}), allowedNames: make(map[string]struct{})}
	if err := policy.addAllowedTargets("*"); err != nil {
		t.Fatal(err)
	}
	resolver := &recordingResolver{addresses: map[string][]net.IPAddr{
		"public.example": {{IP: net.ParseIP("203.0.113.10")}},
	}}
	_, resolved, err := resolvePortCheckTarget(context.Background(), "public.example", policy, resolver)
	if err != nil || resolved != "203.0.113.10" {
		t.Fatalf("wildcard resolution = %q / %v", resolved, err)
	}
}

func TestExecutePortCheckSortsAndFiltersAllResults(t *testing.T) {
	probe := func(_ context.Context, _ string, port int, protocol string, _ time.Duration) PortCheckResult {
		status := "closed"
		if port == 443 && protocol == "tcp" {
			status = "open"
		}
		if port == 53 && protocol == "udp" {
			status = "responded"
		}
		return PortCheckResult{Port: port, Protocol: protocol, Status: status}
	}

	summary := executePortCheck(
		context.Background(),
		"127.0.0.1",
		[]int{443, 53, 80},
		[]string{"tcp", "udp"},
		50*time.Millisecond,
		true,
		probe,
	)
	if summary.completed != 6 {
		t.Fatalf("completed = %d, want 6", summary.completed)
	}
	if summary.statusCounts["open"] != 1 || summary.statusCounts["responded"] != 1 || summary.statusCounts["closed"] != 4 {
		t.Fatalf("unexpected status counts: %#v", summary.statusCounts)
	}
	want := []PortCheckResult{
		{Port: 53, Protocol: "udp", Status: "responded"},
		{Port: 443, Protocol: "tcp", Status: "open"},
	}
	if !reflect.DeepEqual(summary.results, want) {
		t.Fatalf("retained results = %#v, want %#v", summary.results, want)
	}
}

func TestExecutePortCheckHonorsWorkerBound(t *testing.T) {
	ports := make([]int, portCheckMaximumWorkers+50)
	for index := range ports {
		ports[index] = index + 1
	}

	var mutex sync.Mutex
	active := 0
	maximumActive := 0
	probe := func(_ context.Context, _ string, port int, protocol string, _ time.Duration) PortCheckResult {
		mutex.Lock()
		active++
		if active > maximumActive {
			maximumActive = active
		}
		mutex.Unlock()
		time.Sleep(2 * time.Millisecond)
		mutex.Lock()
		active--
		mutex.Unlock()
		return PortCheckResult{Port: port, Protocol: protocol, Status: "closed"}
	}

	summary := executePortCheck(context.Background(), "127.0.0.1", ports, []string{"tcp"}, time.Second, false, probe)
	if summary.completed != len(ports) {
		t.Fatalf("completed = %d, want %d", summary.completed, len(ports))
	}
	if maximumActive > portCheckMaximumWorkers {
		t.Fatalf("maximum active workers = %d, limit %d", maximumActive, portCheckMaximumWorkers)
	}
}

func TestProbeTCPAndUDP(t *testing.T) {
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	go func() {
		connection, acceptErr := tcpListener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
	}()
	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
	tcpResult := probeTCPPort(context.Background(), "127.0.0.1", tcpPort, time.Second)
	if tcpResult.Status != "open" {
		t.Fatalf("TCP status = %q (%s), want open", tcpResult.Status, tcpResult.Detail)
	}

	udpListener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpListener.Close()
	go func() {
		buffer := make([]byte, 512)
		count, address, readErr := udpListener.ReadFrom(buffer)
		if readErr == nil {
			_, _ = udpListener.WriteTo(buffer[:count], address)
		}
	}()
	udpPort := udpListener.LocalAddr().(*net.UDPAddr).Port
	udpResult := probeUDPPort(context.Background(), "127.0.0.1", udpPort, time.Second)
	if udpResult.Status != "responded" {
		t.Fatalf("UDP status = %q (%s), want responded", udpResult.Status, udpResult.Detail)
	}

	closedUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedUDPPort := closedUDP.LocalAddr().(*net.UDPAddr).Port
	_ = closedUDP.Close()
	unknownResult := probeUDPPort(context.Background(), "127.0.0.1", closedUDPPort, 50*time.Millisecond)
	if unknownResult.Status != "unknown" {
		t.Fatalf("closed UDP status = %q, want unknown (never open)", unknownResult.Status)
	}
}

func TestPortCheckEndpointTCPResponse(t *testing.T) {
	clearPortCheckTargetEnvironment(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	body := `{"target":"127.0.0.1","ports":"` + strconv.Itoa(port) + `","protocol":"tcp","timeout_ms":500}`
	recorder := performPortCheckRequest(http.MethodPost, "application/json", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response PortCheckResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Target != "127.0.0.1" || response.ResolvedTarget != "127.0.0.1" {
		t.Fatalf("target = %q / %q", response.Target, response.ResolvedTarget)
	}
	if response.AllPorts || response.NormalizedPorts != strconv.Itoa(port) || !reflect.DeepEqual(response.Ports, []int{port}) {
		t.Fatalf("unexpected normalized ports: all=%v normalized=%q ports=%v", response.AllPorts, response.NormalizedPorts, response.Ports)
	}
	if response.Total != 1 || response.Completed != 1 || response.Partial || response.Cancelled {
		t.Fatalf("unexpected completion fields: total=%d completed=%d partial=%v cancelled=%v", response.Total, response.Completed, response.Partial, response.Cancelled)
	}
	if len(response.Results) != 1 || response.Results[0].Status != "open" || response.StatusCounts["open"] != 1 {
		t.Fatalf("unexpected scan response: %#v", response)
	}
	if recorder.Header().Get("Content-Security-Policy") == "" || recorder.Header().Get("Cache-Control") == "" {
		t.Fatal("diagnostic security headers are missing")
	}
}

func TestPortCheckAllResponseIsCompact(t *testing.T) {
	clearPortCheckTargetEnvironment(t)
	spanRecorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(spanRecorder),
	)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	parentContext, parentSpan := provider.Tracer("portcheck-test").Start(context.Background(), "http.server")
	defer parentSpan.End()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/port-check",
		bytes.NewBufferString(`{"target":"self","ports":"all","protocol":"tcp","timeout_ms":10}`),
	)
	request.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(parentContext)
	cancel()
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	PortCheckEndpoint(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["ports"]; exists {
		t.Fatal("all-port response unexpectedly contains the 65,535-element ports list")
	}
	var response PortCheckResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.AllPorts || response.NormalizedPorts != "all" || response.Total != 65535 {
		t.Fatalf("unexpected all-port metadata: %#v", response)
	}
	if !response.Partial || !response.Cancelled {
		t.Fatalf("cancelled scan should be partial and cancelled: %#v", response)
	}
	spans := spanRecorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "portcheck.scan" {
		t.Fatalf("full scan emitted %d completed spans, want one aggregate portcheck.scan span", len(spans))
	}
}

func TestPortCheckEndpointRejectsUnsafeRequests(t *testing.T) {
	clearPortCheckTargetEnvironment(t)
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{name: "POST only", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed"},
		{name: "JSON only", method: http.MethodPost, contentType: "text/plain", body: `{}`, wantStatus: http.StatusUnsupportedMediaType, wantCode: "unsupported_media_type"},
		{name: "unknown field", method: http.MethodPost, contentType: "application/json", body: `{"target":"self","ports":"80","protocol":"tcp","extra":true}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_json"},
		{name: "trailing JSON", method: http.MethodPost, contentType: "application/json", body: `{"target":"self","ports":"80","protocol":"tcp"} {}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_json"},
		{name: "null JSON", method: http.MethodPost, contentType: "application/json", body: `null`, wantStatus: http.StatusBadRequest, wantCode: "invalid_json"},
		{name: "bad ports", method: http.MethodPost, contentType: "application/json", body: `{"target":"self","ports":"0","protocol":"tcp"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_ports"},
		{name: "explicit scan too large", method: http.MethodPost, contentType: "application/json", body: `{"target":"self","ports":"1-4097","protocol":"tcp"}`, wantStatus: http.StatusBadRequest, wantCode: "too_many_ports"},
		{name: "bad protocol", method: http.MethodPost, contentType: "application/json", body: `{"target":"self","ports":"80","protocol":"sctp"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_protocol"},
		{name: "bad timeout", method: http.MethodPost, contentType: "application/json", body: `{"target":"self","ports":"80","protocol":"tcp","timeout_ms":5001}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_timeout"},
		{name: "oversized", method: http.MethodPost, contentType: "application/json", body: `{"target":"` + strings.Repeat("a", portCheckBodyLimit) + `","ports":"80","protocol":"tcp"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_json"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performPortCheckRequest(test.method, test.contentType, test.body)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			var response portCheckErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.wantCode {
				t.Fatalf("code = %q, want %q", response.Code, test.wantCode)
			}
			if strings.Contains(strings.ToLower(response.Error), "connection refused") {
				t.Fatalf("error leaks an internal network error: %q", response.Error)
			}
		})
	}
}

func TestPortCheckEndpointAllowsOnlyOneScan(t *testing.T) {
	clearPortCheckTargetEnvironment(t)
	if !acquirePortCheckScan() {
		t.Fatal("scan slot unexpectedly occupied")
	}
	defer releasePortCheckScan()

	recorder := performPortCheckRequest(
		http.MethodPost,
		"application/json",
		`{"target":"self","ports":"80","protocol":"tcp","timeout_ms":10}`,
	)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("429 response is missing Retry-After")
	}
}

func performPortCheckRequest(method, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/api/port-check", bytes.NewBufferString(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	PortCheckEndpoint(recorder, request)
	return recorder
}

func clearPortCheckTargetEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{portCheckAllowedTargetEnv, "POD_IP"} {
		oldValue, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		cleanupName := name
		cleanupValue := oldValue
		cleanupExisted := existed
		t.Cleanup(func() {
			if cleanupExisted {
				_ = os.Setenv(cleanupName, cleanupValue)
			} else {
				_ = os.Unsetenv(cleanupName)
			}
		})
	}
}
