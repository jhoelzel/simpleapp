package router

import (
	"bytes"
	"context"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhoelzel/simpleapp/pkg/telemetry"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

func TestConfiguredTracingCorrelatesDashboardAPIAndAccessLogs(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	collector := &integrationTraceCollector{}
	server := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	clearIntegrationTracingEnvironment(t)
	t.Setenv("SIMPLEAPP_TRACING_ENABLED", "true")
	t.Setenv("OTEL_SERVICE_NAME", "webrequesttest")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "authorization=integration-header-secret")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=formslogic,demo.value=integration-resource-secret")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")

	var logs bytes.Buffer
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogOutput) })

	runtime := telemetry.Initialize(context.Background(), "0.0.test")
	if status := runtime.Status(); status.State != telemetry.StateConfigured {
		t.Fatalf("tracing status = %#v", status)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = runtime.Shutdown(shutdownCtx)
		cancel()
	})
	router := Get("0.0.test", runtime.Middleware())

	type response struct {
		path string
		body string
	}
	responses := make([]response, 0, 2)
	for _, path := range []string{"/", "/api/request"} {
		request := httptest.NewRequest(http.MethodGet, "http://example.test"+path, nil)
		request.Header.Set("X-Request-ID", "integration"+strings.ReplaceAll(path, "/", "-"))
		if path == "/api/request" {
			request.Header.Set("Accept", "application/json")
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d; body=%s", path, recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		for _, secret := range []string{"integration-header-secret", "integration-resource-secret", listener.Addr().String()} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s response exposed tracing configuration %q", path, secret)
			}
		}
		if !strings.Contains(strings.ToLower(body), "configured") || !strings.Contains(body, "webrequesttest") {
			t.Fatalf("%s response does not show safe configured status", path)
		}
		responses = append(responses, response{path: path, body: body})
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()

	spans := collector.spans()
	if len(spans) != 2 {
		t.Fatalf("exported server spans = %d, want 2", len(spans))
	}
	byName := make(map[string]*tracepb.Span, len(spans))
	for _, span := range spans {
		byName[span.Name] = span
	}
	for _, response := range responses {
		span := byName[http.MethodGet+" "+response.path]
		if span == nil {
			t.Fatalf("missing exported span for %s; names=%v", response.path, spanNames(spans))
		}
		traceID := hex.EncodeToString(span.TraceId)
		spanID := hex.EncodeToString(span.SpanId)
		if !strings.Contains(response.body, traceID) || !strings.Contains(response.body, spanID) {
			t.Fatalf("%s response is not correlated to exported trace=%s span=%s", response.path, traceID, spanID)
		}
		if !strings.Contains(logs.String(), `trace_id="`+traceID+`"`) || !strings.Contains(logs.String(), `span_id="`+spanID+`"`) {
			t.Fatalf("access log is not correlated to exported trace=%s span=%s", traceID, spanID)
		}
	}
	if strings.Contains(logs.String(), "integration-header-secret") || strings.Contains(logs.String(), "integration-resource-secret") {
		t.Fatalf("access logs exposed tracing secrets: %s", logs.String())
	}
}

type integrationTraceCollector struct {
	collectortrace.UnimplementedTraceServiceServer
	mu       sync.Mutex
	received []*tracepb.Span
}

func (collector *integrationTraceCollector) Export(_ context.Context, request *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	for _, resourceSpans := range request.ResourceSpans {
		for _, scopeSpans := range resourceSpans.ScopeSpans {
			collector.received = append(collector.received, scopeSpans.Spans...)
		}
	}
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

func (collector *integrationTraceCollector) spans() []*tracepb.Span {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]*tracepb.Span(nil), collector.received...)
}

func spanNames(spans []*tracepb.Span) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	return names
}

func clearIntegrationTracingEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SIMPLEAPP_TRACING_ENABLED",
		"SIMPLEAPP_TRACE_PUBLIC_ENDPOINT",
		"OTEL_SDK_DISABLED",
		"OTEL_SERVICE_NAME",
		"OTEL_TRACES_EXPORTER",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_EXPORTER_OTLP_TIMEOUT",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		"OTEL_EXPORTER_OTLP_COMPRESSION",
		"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
		"OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
		"OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG",
	} {
		t.Setenv(name, "")
	}
}
