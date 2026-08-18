package homecontroller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHomeEndpointRendersEscapedDiagnosticDashboard(t *testing.T) {
	setTestEnvironment(t, "SHOW_SENSITIVE_DATA", "false")
	req := httptest.NewRequest(http.MethodGet, "http://simpleapp.test/?view=full&token=page-secret", nil)
	req.RemoteAddr = "192.0.2.10:43210"
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("X-Demo", `<img src=x onerror="alert(1)">`)
	recorder := httptest.NewRecorder()

	homeEndpoint(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "connect-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", got)
	}

	body := recorder.Body.String()
	for _, want := range []string{
		"Kubernetes traffic laboratory",
		"Incoming request",
		"Port checker",
		"198.51.100.9",
		"[REDACTED]",
		"&lt;img src=x onerror=&#34;alert(1)&#34;&gt;",
		"</html>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not contain %q", want)
		}
	}
	for _, secret := range []string{"page-secret", "header-secret", `<img src=x onerror="alert(1)">`} {
		if strings.Contains(body, secret) {
			t.Errorf("dashboard leaked unsafe value %q", secret)
		}
	}
}

func TestHomeEndpointCanReturnJSON(t *testing.T) {
	setTestEnvironment(t, "SHOW_SENSITIVE_DATA", "false")
	setTestEnvironment(t, "PORT", "18080")
	req := httptest.NewRequest(http.MethodPost, "http://simpleapp.test/?format=json&pretty=1", strings.NewReader("demo body"))
	req.Header.Set("Accept", "application/json")
	recorder := httptest.NewRecorder()

	homeEndpoint(recorder, req)

	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	var response diagnosticEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode JSON response: %v\n%s", err, recorder.Body.String())
	}
	if response.Request.Method != http.MethodPost || response.Request.BodyPreview != "demo body" {
		t.Fatalf("unexpected request snapshot: %#v", response.Request)
	}
	if len(response.DemoListeners) != 4 || response.DemoListeners[0] != "18080/tcp (HTTP)" || len(response.Notes) < 3 {
		t.Fatalf("missing diagnostic metadata: %#v", response)
	}
	if !strings.Contains(recorder.Body.String(), "\n  \"request\"") {
		t.Fatal("pretty=1 did not indent JSON")
	}
}

func TestWantsJSON(t *testing.T) {
	tests := []struct {
		name   string
		target string
		accept string
		want   bool
	}{
		{name: "format", target: "http://example.test/?format=JSON", want: true},
		{name: "accept", target: "http://example.test/", accept: "text/html, application/json; q=0.8", want: true},
		{name: "quality zero", target: "http://example.test/", accept: "application/json;q=0", want: false},
		{name: "html", target: "http://example.test/", accept: "text/html", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			req.Header.Set("Accept", tt.accept)
			if got := wantsJSON(req); got != tt.want {
				t.Fatalf("wantsJSON() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatBytesAndIdentityHue(t *testing.T) {
	if got := formatBytes(1536); got != "1.5 KiB" {
		t.Fatalf("formatBytes(1536) = %q", got)
	}
	first := identityHue("pod-uid")
	if first < 0 || first > 359 || first != identityHue("pod-uid") {
		t.Fatalf("identityHue is invalid or unstable: %d", first)
	}
}

func TestDiagnosticHTTPPort(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  string
	}{
		{value: "18080", want: "18080"},
		{value: "5060", want: "8080"},
		{value: "5061", want: "8080"},
		{value: "invalid", want: "8080"},
	} {
		setTestEnvironment(t, "PORT", tt.value)
		if got := diagnosticHTTPPort(); got != tt.want {
			t.Fatalf("PORT=%q: diagnosticHTTPPort() = %q, want %q", tt.value, got, tt.want)
		}
	}
}
