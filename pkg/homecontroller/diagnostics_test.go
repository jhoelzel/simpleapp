package homecontroller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhoelzel/simpleapp/pkg/version"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestCollectRequestDetails(t *testing.T) {
	setTestEnvironment(t, "SHOW_SENSITIVE_DATA", "false")
	body := "hello <world>"
	req := httptest.NewRequest(http.MethodPost, "https://origin.invalid/a%2Fb?z=last&token=very-secret&a=2&a=1", strings.NewReader(body))
	req.Host = "public.example"
	req.RemoteAddr = "[2001:db8::5]:54321"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{
		IP:   net.ParseIP("2001:db8::10"),
		Port: 8080,
		Zone: "eth0",
	}))
	req.TLS = &tls.ConnectionState{
		Version:            tls.VersionTLS13,
		CipherSuite:        tls.TLS_AES_128_GCM_SHA256,
		ServerName:         "public.example",
		NegotiatedProtocol: "h2",
		DidResume:          true,
	}
	req.TransferEncoding = []string{"chunked"}
	req.Header["X-Zeta"] = []string{"last"}
	req.Header["Authorization"] = []string{"Bearer request-secret"}
	req.Header["Cookie"] = []string{"session=cookie-secret", "other=second-secret"}
	req.Header["Cf-Access-Jwt-Assertion"] = []string{"cloudflare-assertion-secret"}
	req.Header["X-Goog-Iap-Jwt-Assertion"] = []string{"google-assertion-secret"}
	req.Header["X-Amzn-Oidc-Data"] = []string{"aws-assertion-secret"}
	req.Header["X-Multi"] = []string{"first", "second"}
	req.Header["X-Forwarded-For"] = []string{"198.51.100.20, 10.0.0.3"}
	req.Header["X-Forwarded-Proto"] = []string{"https"}
	req.Header["Traceparent"] = []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	req.Header["Tracestate"] = []string{"vendor=opaque-secret"}
	req.Header["Baggage"] = []string{"account=customer-secret"}
	req.Trailer = http.Header{
		"Authorization": []string{"trailer-secret"},
		"X-Trailer":     []string{"trailer-value"},
	}

	details := CollectRequestDetails(req)

	if details.Method != http.MethodPost || details.Scheme != "https" || details.Proto != req.Proto {
		t.Fatalf("unexpected request line details: %#v", details)
	}
	if details.Host != "public.example" || details.Path != "/a/b" {
		t.Fatalf("unexpected host/path: host=%q path=%q", details.Host, details.Path)
	}
	if details.RemoteSocket.Host != "2001:db8::5" || details.RemoteSocket.Port != "54321" {
		t.Fatalf("remote IPv6 socket was parsed incorrectly: %#v", details.RemoteSocket)
	}
	if details.LocalSocket.Host != "2001:db8::10%eth0" || details.LocalSocket.Port != "8080" {
		t.Fatalf("local IPv6 socket was parsed incorrectly: %#v", details.LocalSocket)
	}
	if details.ReportedClient.Source != "X-Forwarded-For" || details.ReportedClient.Value != "198.51.100.20" || details.ReportedClient.Trusted {
		t.Fatalf("unexpected reported client claim: %#v", details.ReportedClient)
	}
	if details.ContentLength != int64(len(body)) || !reflect.DeepEqual(details.TransferEncoding, []string{"chunked"}) {
		t.Fatalf("content metadata mismatch: length=%d transfer=%v", details.ContentLength, details.TransferEncoding)
	}
	if details.BodyPreview != body || details.BodyPreviewEncoding != "utf-8" || details.BodyPreviewBytes != len(body) || details.BodyTruncated {
		t.Fatalf("unexpected body preview: %#v", details)
	}
	if details.BodyPreviewLimit != BodyPreviewLimit {
		t.Fatalf("body preview limit = %d, want %d", details.BodyPreviewLimit, BodyPreviewLimit)
	}
	if !details.TrailersComplete {
		t.Fatal("trailers for a fully read body were not marked complete")
	}
	if !details.TLS.Enabled || details.TLS.Version != "TLS 1.3" || details.TLS.CipherSuite != "TLS_AES_128_GCM_SHA256" || details.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("unexpected TLS summary: %#v", details.TLS)
	}

	assertSortedDiagnosticNames(t, details.Headers)
	assertSortedDiagnosticNames(t, details.Query)
	assertSortedDiagnosticNames(t, details.Trailers)
	assertSortedDiagnosticNames(t, details.LoadBalancerHeaders)
	assertDiagnosticValues(t, details.Headers, "Authorization", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Headers, "Cookie", []string{redactedDiagnosticValue, redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Headers, "Cf-Access-Jwt-Assertion", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Headers, "X-Goog-Iap-Jwt-Assertion", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Headers, "X-Amzn-Oidc-Data", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Headers, "Traceparent", []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"})
	assertDiagnosticValues(t, details.Headers, "Tracestate", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Headers, "Baggage", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Headers, "X-Multi", []string{"first", "second"})
	assertDiagnosticValues(t, details.Query, "a", []string{"2", "1"})
	assertDiagnosticValues(t, details.Query, "token", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.Trailers, "Authorization", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.LoadBalancerHeaders, "X-Forwarded-For", []string{"198.51.100.20, 10.0.0.3"})
	assertDiagnosticValues(t, details.LoadBalancerHeaders, "Traceparent", []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"})
	assertDiagnosticValues(t, details.LoadBalancerHeaders, "Tracestate", []string{redactedDiagnosticValue})
	assertDiagnosticValues(t, details.LoadBalancerHeaders, "Baggage", []string{redactedDiagnosticValue})
	if details.RawQuery != "z=last&token=%5BREDACTED%5D&a=2&a=1" {
		t.Fatalf("redaction did not preserve safe query ordering/encoding: %q", details.RawQuery)
	}

	for _, value := range []string{details.RequestURI, details.RawQuery, details.RawRequest} {
		if strings.Contains(value, "very-secret") {
			t.Fatalf("sensitive query value leaked in %q", value)
		}
	}
	if strings.Contains(details.RawRequest, "request-secret") || strings.Contains(details.RawRequest, "cookie-secret") || strings.Contains(details.RawRequest, "assertion-secret") || strings.Contains(details.RawRequest, "opaque-secret") || strings.Contains(details.RawRequest, "customer-secret") {
		t.Fatalf("credential header leaked in raw request: %q", details.RawRequest)
	}
	if strings.Contains(details.RawRequest, body) {
		t.Fatalf("raw request unexpectedly contains its body: %q", details.RawRequest)
	}

	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("restored body = %q, want %q", restored, body)
	}
}

func TestCollectRequestDetailsIncludesActiveTraceContext(t *testing.T) {
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, span := provider.Tracer("diagnostics-test").Start(context.Background(), "request")
	defer span.End()
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil).WithContext(ctx)

	details := CollectRequestDetails(req)
	if !details.Trace.Active || !details.Trace.Recording || !details.Trace.Sampled {
		t.Fatalf("trace context = %#v", details.Trace)
	}
	if details.Trace.TraceID != span.SpanContext().TraceID().String() || details.Trace.SpanID != span.SpanContext().SpanID().String() {
		t.Fatalf("trace identifiers = %#v, want %s/%s", details.Trace, span.SpanContext().TraceID(), span.SpanContext().SpanID())
	}
}

func TestCollectRequestDetailsCapsAndRestoresBody(t *testing.T) {
	setTestEnvironment(t, "SHOW_SENSITIVE_DATA", "false")
	body := strings.Repeat("a", BodyPreviewLimit+137)
	req := httptest.NewRequest(http.MethodPost, "http://example.test/", strings.NewReader(body))

	details := CollectRequestDetails(req)
	if !details.BodyTruncated {
		t.Fatal("large body was not marked truncated")
	}
	if details.TrailersComplete {
		t.Fatal("trailers for a truncated body were marked complete")
	}
	if details.BodyPreviewBytes != BodyPreviewLimit || len(details.BodyPreview) != BodyPreviewLimit {
		t.Fatalf("preview size = %d/%d, want %d", details.BodyPreviewBytes, len(details.BodyPreview), BodyPreviewLimit)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("restored body has %d bytes, want %d", len(restored), len(body))
	}
}

func TestCollectRequestDetailsEncodesBinaryPreview(t *testing.T) {
	body := []byte{0xff, 0x00, 'a'}
	req := httptest.NewRequest(http.MethodPost, "http://example.test/", bytes.NewReader(body))
	details := CollectRequestDetails(req)

	if details.BodyPreviewEncoding != "base64" {
		t.Fatalf("preview encoding = %q, want base64", details.BodyPreviewEncoding)
	}
	if details.BodyPreview != base64.StdEncoding.EncodeToString(body) {
		t.Fatalf("binary preview = %q", details.BodyPreview)
	}
}

func TestCollectRequestDetailsSensitiveOverride(t *testing.T) {
	setTestEnvironment(t, "SHOW_SENSITIVE_DATA", "TrUe")
	req := httptest.NewRequest(http.MethodGet, "http://example.test/?password=query-secret", nil)
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("X-API-Key", "api-secret")

	details := CollectRequestDetails(req)
	if !details.SensitiveDataShown {
		t.Fatal("sensitive override was not reported")
	}
	assertDiagnosticValues(t, details.Headers, "Authorization", []string{"Bearer header-secret"})
	assertDiagnosticValues(t, details.Headers, "X-API-Key", []string{"api-secret"})
	assertDiagnosticValues(t, details.Query, "password", []string{"query-secret"})
	for _, want := range []string{"query-secret", "header-secret", "api-secret"} {
		combined := details.RequestURI + details.RawQuery + details.RawRequest
		if !strings.Contains(combined, want) {
			t.Fatalf("sensitive override did not retain %q in diagnostics", want)
		}
	}
}

func TestCollectRequestDetailsMalformedQueryFailsClosed(t *testing.T) {
	setTestEnvironment(t, "SHOW_SENSITIVE_DATA", "false")
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	req.URL.RawQuery = "token=query-secret&broken=%ZZ"
	req.RequestURI = "/?token=query-secret&broken=%ZZ"

	details := CollectRequestDetails(req)
	if len(details.CollectionErrors) == 0 {
		t.Fatal("malformed query was not reported")
	}
	combined := details.RequestURI + details.RawQuery + details.RawRequest
	if strings.Contains(combined, "query-secret") || strings.Contains(combined, "%ZZ") {
		t.Fatalf("malformed query was not reconstructed safely: %q", combined)
	}
}

func TestCollectRequestDetailsPreservesSafeRawQuery(t *testing.T) {
	setTestEnvironment(t, "SHOW_SENSITIVE_DATA", "false")
	rawQuery := "z=1&a=%2f&a=hello+world&empty"
	req := httptest.NewRequest(http.MethodGet, "http://example.test/path?"+rawQuery, nil)

	details := CollectRequestDetails(req)

	if details.RawQuery != rawQuery {
		t.Fatalf("RawQuery = %q, want exact input %q", details.RawQuery, rawQuery)
	}
	if details.RequestURI != "/path?"+rawQuery {
		t.Fatalf("RequestURI = %q", details.RequestURI)
	}
	if !strings.Contains(details.RawRequest, "GET /path?"+rawQuery+" HTTP/1.1") {
		t.Fatalf("raw request changed safe query bytes: %q", details.RawRequest)
	}
}

func TestCollectRequestDetailsForwardedIPv6Claim(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	req.Header.Set("Forwarded", `for="[2001:db8:cafe::17]:4711";proto=https;host=example.test`)
	req.Header.Set("X-Forwarded-For", "198.51.100.12")

	details := CollectRequestDetails(req)
	if details.ReportedClient.Source != "Forwarded" || details.ReportedClient.Value != "[2001:db8:cafe::17]:4711" {
		t.Fatalf("forwarded IPv6 claim = %#v", details.ReportedClient)
	}
	if details.Scheme != "http" {
		t.Fatalf("transport scheme = %q; forwarding claims must not change it", details.Scheme)
	}
}

func TestRequestDetailsAreHTMLAndJSONFriendly(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/?ordinary=%3Cscript%3Equery%3C%2Fscript%3E", nil)
	req.Header.Set("X-Attack", `<img src=x onerror="alert(1)">`)
	details := CollectRequestDetails(req)

	view := template.Must(template.New("diagnostics").Parse(`{{range .Headers}}{{range .Values}}{{.}}{{end}}{{end}}{{range .Query}}{{range .Values}}{{.}}{{end}}{{end}}`))
	var rendered bytes.Buffer
	if err := view.Execute(&rendered, details); err != nil {
		t.Fatalf("render request details: %v", err)
	}
	if strings.Contains(rendered.String(), "<img") || strings.Contains(rendered.String(), "<script>") {
		t.Fatalf("html/template output contains executable markup: %q", rendered.String())
	}
	if !strings.Contains(rendered.String(), "&lt;img") || !strings.Contains(rendered.String(), "&lt;script&gt;") {
		t.Fatalf("html/template did not escape diagnostic values: %q", rendered.String())
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("marshal request details: %v", err)
	}
	if bytes.Contains(encoded, []byte("<img")) || bytes.Contains(encoded, []byte("<script>")) {
		t.Fatalf("JSON encoder did not HTML-escape markup: %s", encoded)
	}
}

func TestCollectRequestDetailsNilRequest(t *testing.T) {
	details := CollectRequestDetails(nil)
	if len(details.CollectionErrors) != 1 || details.CollectionErrors[0] != "request is nil" {
		t.Fatalf("nil request errors = %v", details.CollectionErrors)
	}
	if details.BodyPreviewLimit != BodyPreviewLimit {
		t.Fatalf("nil request preview limit = %d", details.BodyPreviewLimit)
	}
}

func TestCollectRuntimeDetails(t *testing.T) {
	setTestEnvironment(t, "POD_NAME", "simpleapp-abc")
	setTestEnvironment(t, "POD_NAMESPACE", "demo")
	setTestEnvironment(t, "POD_IP", "10.42.0.17")
	setTestEnvironment(t, "POD_UID", "pod-uid")
	setTestEnvironment(t, "HOST_IP", "192.0.2.50")
	setTestEnvironment(t, "NODE_NAME", "worker-1")
	setTestEnvironment(t, "SERVICE_ACCOUNT", "simpleapp")
	setTestEnvironment(t, "CONTAINER_NAME", "web")
	setTestEnvironment(t, "CPU_REQUEST_MILLICORES", "25")
	setTestEnvironment(t, "CPU_LIMIT_MILLICORES", "100")
	setTestEnvironment(t, "MEMORY_REQUEST_BYTES", "33554432")
	setTestEnvironment(t, "MEMORY_LIMIT_BYTES", "67108864")
	setTestEnvironment(t, "BUILD_IMAGE", "example/simpleapp:test")

	originalRelease, originalCommit, originalBuildTime := version.Release, version.Commit, version.BuildTime
	version.Release, version.Commit, version.BuildTime = "v-test", "abc123", "2026-08-18T00:00:00Z"
	t.Cleanup(func() {
		version.Release, version.Commit, version.BuildTime = originalRelease, originalCommit, originalBuildTime
	})

	details := CollectRuntimeDetails()
	if details.Build.Release != "v-test" || details.Build.Commit != "abc123" || details.Build.BuildTime != "2026-08-18T00:00:00Z" || details.Build.Image != "example/simpleapp:test" {
		t.Fatalf("build details = %#v", details.Build)
	}
	if details.Hostname == "" || details.CurrentTime.IsZero() || details.ProcessStartedAt.IsZero() || details.UptimeSeconds < 0 {
		t.Fatalf("invalid host/time details: %#v", details)
	}
	if details.GoVersion == "" || details.OS == "" || details.Arch == "" || details.PID <= 0 || details.CPUs <= 0 || details.Goroutines <= 0 {
		t.Fatalf("invalid process details: %#v", details)
	}
	if details.Memory.SysBytes == 0 {
		t.Fatalf("runtime memory details were not collected: %#v", details.Memory)
	}
	if details.Tracing.State != "disabled" || details.Tracing.Enabled {
		t.Fatalf("default tracing status = %#v", details.Tracing)
	}
	if details.Kubernetes.PodName != "simpleapp-abc" || details.Kubernetes.PodNamespace != "demo" || details.Kubernetes.PodIP != "10.42.0.17" || details.Kubernetes.HostIP != "192.0.2.50" || details.Kubernetes.NodeName != "worker-1" || details.Kubernetes.ServiceAccount != "simpleapp" {
		t.Fatalf("Kubernetes fields = %#v", details.Kubernetes)
	}
	if details.Kubernetes.Resources.CPURequestMillicores != "25" || details.Kubernetes.Resources.CPULimitMillicores != "100" || details.Kubernetes.Resources.MemoryRequestBytes != "33554432" || details.Kubernetes.Resources.MemoryLimitBytes != "67108864" {
		t.Fatalf("Kubernetes resources = %#v", details.Kubernetes.Resources)
	}
	if !sort.StringsAreSorted(details.LocalIPs) {
		t.Fatalf("local IPs are not sorted: %v", details.LocalIPs)
	}
	interfaceNames := make([]string, 0, len(details.Interfaces))
	for _, iface := range details.Interfaces {
		interfaceNames = append(interfaceNames, iface.Name)
		if !sort.StringsAreSorted(iface.Addresses) {
			t.Fatalf("addresses for %s are not sorted: %v", iface.Name, iface.Addresses)
		}
	}
	if !sort.StringsAreSorted(interfaceNames) {
		t.Fatalf("network interfaces are not sorted: %v", interfaceNames)
	}
	if _, err := json.Marshal(details); err != nil {
		t.Fatalf("marshal runtime details: %v", err)
	}
}

func TestCollectRuntimeDetailsHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := preferredOutboundIP(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("preferred outbound IP error = %v, want context.Canceled", err)
	}

	details := CollectRuntimeDetailsContext(ctx)
	if details.OutboundIP != "" {
		t.Fatalf("canceled runtime collection discovered outbound IP %q", details.OutboundIP)
	}
	foundCancellation := false
	for _, collectionErr := range details.CollectionErrors {
		if strings.HasPrefix(collectionErr, "outbound IP:") && strings.Contains(strings.ToLower(collectionErr), "cancel") {
			foundCancellation = true
			break
		}
	}
	if !foundCancellation {
		t.Fatalf("canceled runtime collection errors = %v", details.CollectionErrors)
	}
}

func TestSetDiagnosticResponseHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	SetDiagnosticResponseHeaders(recorder)

	wants := map[string]string{
		"Cache-Control":           "no-store, max-age=0",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	}
	for name, want := range wants {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := recorder.Header().Get("Content-Type"); got != "" {
		t.Fatalf("helper unexpectedly selected response content type %q", got)
	}
}

func TestSensitiveDiagnosticNames(t *testing.T) {
	setTestEnvironment(t, "DIAGNOSTIC_REDACT_NAMES", "X-Workshop-Identity")
	tests := map[string]bool{
		"Authorization":            true,
		"Proxy-Authorization":      true,
		"Cookie":                   true,
		"X-API-Key":                true,
		"X-Amz-Security-Token":     true,
		"Cf-Access-Jwt-Assertion":  true,
		"X-Goog-Iap-Jwt-Assertion": true,
		"X-Amzn-Oidc-Data":         true,
		"X-SSL-Client-Cert":        true,
		"X-Workshop-Identity":      true,
		"client_secret":            true,
		"access_token":             true,
		"password":                 true,
		"Baggage":                  true,
		"Tracestate":               true,
		"X-Request-ID":             false,
		"secretIp":                 false,
		"monkey":                   false,
		"Content-Type":             false,
	}
	for name, want := range tests {
		if got := isSensitiveDiagnosticName(name); got != want {
			t.Errorf("isSensitiveDiagnosticName(%q) = %t, want %t", name, got, want)
		}
	}
}

func assertSortedDiagnosticNames(t *testing.T, values []DiagnosticValues) {
	t.Helper()
	names := make([]string, 0, len(values))
	for _, value := range values {
		names = append(names, strings.ToLower(value.Name))
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("diagnostic names are not sorted: %v", names)
	}
}

func assertDiagnosticValues(t *testing.T, values []DiagnosticValues, name string, want []string) {
	t.Helper()
	for _, value := range values {
		if strings.EqualFold(value.Name, name) {
			if !reflect.DeepEqual(value.Values, want) {
				t.Fatalf("%s values = %v, want %v", name, value.Values, want)
			}
			return
		}
	}
	t.Fatalf("diagnostic value %q not found in %v", name, values)
}

func setTestEnvironment(t *testing.T, name, value string) {
	t.Helper()
	original, existed := os.LookupEnv(name)
	if err := os.Setenv(name, value); err != nil {
		t.Fatalf("set %s: %v", name, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, original)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func TestDiagnosticUptimeAdvances(t *testing.T) {
	first := CollectRuntimeDetails()
	time.Sleep(time.Millisecond)
	second := CollectRuntimeDetails()
	if second.UptimeSeconds <= first.UptimeSeconds {
		t.Fatalf("uptime did not advance: first=%f second=%f", first.UptimeSeconds, second.UptimeSeconds)
	}
}
