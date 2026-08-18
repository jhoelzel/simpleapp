package router

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessLogMiddlewareAddsDiagnosticHeadersWithoutLoggingSecrets(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	handler := accessLogMiddleware("v1.2.3")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	req := httptest.NewRequest(http.MethodGet, "http://example.test/demo?token=do-not-log", nil)
	req.Header.Set("Authorization", "Bearer do-not-log")
	req.Header.Set("X-Request-ID", "demo-request-123")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}
	if got := recorder.Header().Get("X-Request-ID"); got != "demo-request-123" {
		t.Errorf("X-Request-ID = %q", got)
	}
	if got := recorder.Header().Get("X-Simpleapp-Version"); got != "v1.2.3" {
		t.Errorf("X-Simpleapp-Version = %q", got)
	}
	if strings.Contains(logs.String(), "do-not-log") || strings.Contains(logs.String(), "token=") {
		t.Fatalf("access log leaked sensitive request data: %s", logs.String())
	}
	for _, want := range []string{`method="GET"`, `path="/demo"`, "status=201", "bytes=2"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("access log %q does not contain %q", logs.String(), want)
		}
	}
}

func TestResponseRecorderPreservesOptionalWriterCapabilities(t *testing.T) {
	underlying := httptest.NewRecorder()
	recorder := &responseRecorder{ResponseWriter: underlying}

	if recorder.Unwrap() != underlying {
		t.Fatal("Unwrap did not return the underlying writer")
	}
	if _, ok := interface{}(recorder).(http.Flusher); !ok {
		t.Fatal("response recorder does not implement http.Flusher")
	}
	if _, ok := interface{}(recorder).(io.ReaderFrom); !ok {
		t.Fatal("response recorder does not implement io.ReaderFrom")
	}
	if _, err := recorder.ReadFrom(strings.NewReader("streamed")); err != nil {
		t.Fatal(err)
	}
	if recorder.status != http.StatusOK || recorder.bytes != len("streamed") || underlying.Body.String() != "streamed" {
		t.Fatalf("unexpected streamed response: status=%d bytes=%d body=%q", recorder.status, recorder.bytes, underlying.Body.String())
	}
}

func TestSafeRequestID(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "valid", value: "trace/abc:123-_.", want: "trace/abc:123-_."},
		{name: "empty"},
		{name: "space", value: "not safe"},
		{name: "newline", value: "not\nsafe"},
		{name: "too long", value: strings.Repeat("x", 129)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeRequestID(tt.value); got != tt.want {
				t.Fatalf("safeRequestID(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
