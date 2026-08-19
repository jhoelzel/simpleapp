package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

func TestConfigFromEnvironmentDefaultsToDisabled(t *testing.T) {
	clearTracingEnvironment(t)

	cfg, err := configFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.enabled || cfg.endpointConfigured || cfg.serviceName != defaultServiceName {
		t.Fatalf("unexpected default config: %#v", cfg)
	}

	runtime := Initialize(context.Background(), "test")
	if got := runtime.Status(); got.State != StateDisabled || got.Enabled {
		t.Fatalf("unexpected disabled status: %#v", got)
	}
}

func TestDisabledTracingSanitizesInvalidServiceName(t *testing.T) {
	clearTracingEnvironment(t)
	t.Setenv("OTEL_SERVICE_NAME", "service\nlog-injection")

	runtime := Initialize(context.Background(), "test")
	if got := runtime.Status(); got.State != StateDisabled || got.ServiceName != defaultServiceName {
		t.Fatalf("disabled status = %#v", got)
	}
}

func TestConfigFromEnvironmentAcceptsOTLPGRPC(t *testing.T) {
	clearTracingEnvironment(t)
	t.Setenv("SIMPLEAPP_TRACING_ENABLED", "true")
	t.Setenv("OTEL_SERVICE_NAME", "webrequesttest")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://tempo.monitoring.svc:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")
	t.Setenv("SIMPLEAPP_TRACE_PUBLIC_ENDPOINT", "false")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")

	cfg, err := configFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.enabled || !cfg.endpointConfigured || !cfg.insecure || cfg.publicEndpoint || cfg.serviceName != "webrequesttest" {
		t.Fatalf("unexpected OTLP config: %#v", cfg)
	}
}

func TestConfigFromEnvironmentRejectsInvalidEnabledConfiguration(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "invalid enabled", env: map[string]string{"SIMPLEAPP_TRACING_ENABLED": "sometimes"}},
		{name: "invalid service name", env: map[string]string{"SIMPLEAPP_TRACING_ENABLED": "true", "OTEL_SERVICE_NAME": "service\nsecret"}},
		{name: "missing endpoint", env: map[string]string{"SIMPLEAPP_TRACING_ENABLED": "true"}},
		{name: "wrong protocol", env: map[string]string{
			"SIMPLEAPP_TRACING_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		}},
		{name: "invalid insecure", env: map[string]string{
			"SIMPLEAPP_TRACING_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4317", "OTEL_EXPORTER_OTLP_INSECURE": "sometimes",
		}},
		{name: "unsupported exporter", env: map[string]string{
			"SIMPLEAPP_TRACING_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4317", "OTEL_TRACES_EXPORTER": "zipkin",
		}},
		{name: "invalid sampler ratio", env: map[string]string{
			"SIMPLEAPP_TRACING_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4317", "OTEL_TRACES_SAMPLER": "parentbased_traceidratio", "OTEL_TRACES_SAMPLER_ARG": "2",
		}},
		{name: "NaN sampler ratio", env: map[string]string{
			"SIMPLEAPP_TRACING_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4317", "OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "NaN",
		}},
		{name: "infinite sampler ratio", env: map[string]string{
			"SIMPLEAPP_TRACING_ENABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://tempo:4317", "OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "+Inf",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearTracingEnvironment(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			if _, err := configFromEnvironment(); err == nil {
				t.Fatal("expected invalid tracing configuration")
			}
			runtime := Initialize(context.Background(), "test")
			if got := runtime.Status(); got.State != StateInitFailed {
				t.Fatalf("status = %#v, want init_failed", got)
			}
		})
	}
}

func TestStandardControlsDisableTracing(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "SDK disabled", key: "OTEL_SDK_DISABLED", value: "true"},
		{name: "trace exporter none", key: "OTEL_TRACES_EXPORTER", value: "none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearTracingEnvironment(t)
			t.Setenv("SIMPLEAPP_TRACING_ENABLED", "true")
			t.Setenv(test.key, test.value)
			t.Setenv("OTEL_SERVICE_NAME", "disabled\nconfiguration")

			runtime := Initialize(context.Background(), "test")
			if got := runtime.Status(); got.State != StateDisabled || got.Enabled || got.EndpointConfigured {
				t.Fatalf("status = %#v, want disabled", got)
			}
		})
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, value := range []string{
		"http://tempo.monitoring.svc:4317",
		"HTTP://tempo.monitoring.svc:4317",
		"https://tempo.example.test",
		"http://[::1]:4317/collector",
	} {
		if err := validateEndpoint(value); err != nil {
			t.Errorf("valid endpoint %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"tempo.monitoring.svc:4317",
		"ftp://tempo:4317",
		"http://user:secret@tempo:4317",
		"http://tempo:4317?token=secret",
		"http://tempo:4317#secret",
		"http://tempo:0",
		"http://tempo:65536",
		"http://bad host:4317",
		"://invalid",
	} {
		if err := validateEndpoint(value); err == nil {
			t.Errorf("invalid endpoint %q accepted", value)
		}
	}
}

func TestEffectiveOTLPTransportSecurityMatchesExporterPrecedence(t *testing.T) {
	for _, test := range []struct {
		name            string
		endpoint        string
		generalInsecure string
		traceInsecure   string
		tlsCredentials  bool
		wantInsecure    bool
	}{
		{name: "HTTP scheme", endpoint: "http://collector:4317", wantInsecure: true},
		{name: "HTTPS scheme", endpoint: "https://collector:4317", wantInsecure: false},
		{name: "general true overrides HTTPS", endpoint: "https://collector:4317", generalInsecure: "true", wantInsecure: true},
		{name: "general false overrides HTTP", endpoint: "http://collector:4317", generalInsecure: "false", wantInsecure: false},
		{name: "trace setting overrides general", endpoint: "https://collector:4317", generalInsecure: "false", traceInsecure: "TRUE", wantInsecure: true},
		{name: "trace false overrides HTTP and general", endpoint: "http://collector:4317", generalInsecure: "true", traceInsecure: "false", wantInsecure: false},
		{name: "CA or client credentials force TLS", endpoint: "http://collector:4317", generalInsecure: "true", traceInsecure: "true", tlsCredentials: true, wantInsecure: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := effectiveOTLPInsecure(test.endpoint, test.generalInsecure, test.traceInsecure, test.tlsCredentials); got != test.wantInsecure {
				t.Fatalf("effective insecure = %t, want %t", got, test.wantInsecure)
			}
		})
	}
}

func TestConfigReportsEffectiveOTLPTransportSecurity(t *testing.T) {
	for _, test := range []struct {
		name            string
		endpoint        string
		generalInsecure string
		traceInsecure   string
		wantInsecure    bool
	}{
		{name: "trace true overrides HTTPS", endpoint: "https://collector:4317", traceInsecure: "true", wantInsecure: true},
		{name: "trace false overrides HTTP", endpoint: "http://collector:4317", traceInsecure: "false", wantInsecure: false},
		{name: "trace wins over general", endpoint: "http://collector:4317", generalInsecure: "true", traceInsecure: "false", wantInsecure: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearTracingEnvironment(t)
			t.Setenv("SIMPLEAPP_TRACING_ENABLED", "true")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", test.endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", test.generalInsecure)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", test.traceInsecure)

			cfg, err := configFromEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.insecure != test.wantInsecure {
				t.Fatalf("configured insecure = %t, want %t", cfg.insecure, test.wantInsecure)
			}
		})
	}
}

func TestStandardBooleanSyntaxMatchesOTel(t *testing.T) {
	for _, value := range []string{"true", "TRUE", "false", "False"} {
		if _, err := parseStandardBoolean(value); err != nil {
			t.Errorf("standard boolean %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"1", "t", "yes", "sometimes"} {
		if _, err := parseStandardBoolean(value); err == nil {
			t.Errorf("non-standard boolean %q accepted", value)
		}
	}
}

func TestInvalidConfigurationDoesNotLeakToLogs(t *testing.T) {
	clearTracingEnvironment(t)
	var logs bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalWriter) })

	tests := []struct {
		name     string
		variable string
		value    string
	}{
		{name: "endpoint", variable: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", value: "http://user:endpoint-secret@tempo:4317"},
		{name: "header", variable: "OTEL_EXPORTER_OTLP_TRACES_HEADERS", value: "authorization=header-secret%ZZ"},
		{name: "timeout", variable: "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", value: "timeout-secret"},
		{name: "certificate", variable: "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", value: "/tmp/certificate-secret-does-not-exist.pem"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearTracingEnvironment(t)
			t.Setenv("SIMPLEAPP_TRACING_ENABLED", "true")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://tempo:4317")
			t.Setenv(test.variable, test.value)
			logs.Reset()

			runtime := Initialize(context.Background(), "test")
			if got := runtime.Status().State; got != StateInitFailed {
				t.Fatalf("state = %q, want init_failed", got)
			}
			if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), test.value) {
				t.Fatalf("configuration secret leaked to logs: %q", logs.String())
			}
		})
	}
}

func TestInitializeDoesNotReplaceGlobalProviderOrPropagator(t *testing.T) {
	clearTracingEnvironment(t)
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	provider := sdktrace.NewTracerProvider()
	propagator := propagation.TraceContext{}
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagator)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		_ = provider.Shutdown(context.Background())
	})

	for _, test := range []struct {
		name string
		env  map[string]string
	}{
		{name: "disabled", env: map[string]string{}},
		{name: "failed", env: map[string]string{"SIMPLEAPP_TRACING_ENABLED": "true"}},
		{name: "configured", env: map[string]string{
			"SIMPLEAPP_TRACING_ENABLED":          "true",
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:4317",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearTracingEnvironment(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			runtime := Initialize(context.Background(), "test")
			if otel.GetTracerProvider() != provider || otel.GetTextMapPropagator() != propagator {
				t.Fatal("Initialize replaced process-global OpenTelemetry state")
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = runtime.Shutdown(shutdownCtx)
			cancel()
		})
	}
}

func TestMiddlewareCreatesRouteSpanAndExposesContext(t *testing.T) {
	for _, publicEndpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "trusted parent", true: "public endpoint"}[publicEndpoint], func(t *testing.T) {
			spanRecorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(
				sdktrace.WithSampler(sdktrace.AlwaysSample()),
				sdktrace.WithSpanProcessor(spanRecorder),
			)
			runtime := testRuntime(provider, publicEndpoint)
			t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

			router := mux.NewRouter()
			router.HandleFunc("/items/{id}", func(w http.ResponseWriter, r *http.Request) {
				traceContext := CurrentTrace(r.Context())
				status := CurrentStatus(r.Context())
				if !traceContext.Active || !traceContext.Recording || !traceContext.Sampled {
					t.Errorf("trace context is not active: %#v", traceContext)
				}
				if status.State != StateConfigured || status.PublicEndpoint != publicEndpoint {
					t.Errorf("unexpected runtime status: %#v", status)
				}
				_, _ = w.Write([]byte(traceContext.TraceID))
			}).Methods(http.MethodGet)
			router.Use(runtime.Middleware())

			request := httptest.NewRequest(http.MethodGet, "http://example.test/items/42", nil)
			const parentTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
			request.Header.Set("Traceparent", "00-"+parentTraceID+"-00f067aa0ba902b7-01")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			spans := spanRecorder.Ended()
			if len(spans) != 1 {
				t.Fatalf("ended spans = %d, want 1", len(spans))
			}
			if spans[0].Name() != "GET /items/{id}" {
				t.Errorf("span name = %q, want route template", spans[0].Name())
			}
			if publicEndpoint {
				if spans[0].SpanContext().TraceID().String() == parentTraceID || len(spans[0].Links()) != 1 {
					t.Errorf("public endpoint should start a linked trace: trace=%s links=%d", spans[0].SpanContext().TraceID(), len(spans[0].Links()))
				}
			} else if got := spans[0].SpanContext().TraceID().String(); got != parentTraceID {
				t.Errorf("trusted endpoint trace ID = %s, want %s", got, parentTraceID)
			}
			if response.Body.String() != spans[0].SpanContext().TraceID().String() {
				t.Errorf("handler trace ID %q does not match span %s", response.Body.String(), spans[0].SpanContext().TraceID())
			}
		})
	}
}

func TestMiddlewareExcludesKubernetesProbes(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	runtime := testRuntime(provider, false)
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	router := mux.NewRouter()
	for _, path := range []string{"/status/healthz", "/status/readyz"} {
		router.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if CurrentStatus(r.Context()).State != StateConfigured {
				t.Error("safe tracing status was not attached to excluded request")
			}
			if CurrentTrace(r.Context()).Active {
				t.Error("probe unexpectedly has an active span")
			}
			w.WriteHeader(http.StatusOK)
		})
	}
	router.Use(runtime.Middleware())

	for _, path := range []string{"/status/healthz", "/status/readyz"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
	if spans := spanRecorder.Ended(); len(spans) != 0 {
		t.Fatalf("probe spans = %d, want 0", len(spans))
	}
}

func TestShutdownRunsProviderOnce(t *testing.T) {
	exporter := &countingExporter{}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	runtime := testRuntime(provider, false)

	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exporter.shutdowns.Load(); got != 1 {
		t.Fatalf("exporter shutdowns = %d, want 1", got)
	}
}

func TestOTLPGRPCExporterSendsRouteSpanAndResource(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	collector := &recordingTraceCollector{requests: make(chan *collectortrace.ExportTraceServiceRequest, 1)}
	server := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	clearTracingEnvironment(t)
	t.Setenv("SIMPLEAPP_TRACING_ENABLED", "true")
	t.Setenv("OTEL_SERVICE_NAME", "webrequesttest")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("POD_NAME", "webrequesttest-test")
	t.Setenv("POD_NAMESPACE", "fl-production")
	t.Setenv("POD_UID", "test-pod-uid")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=spoofed,k8s.pod.name=spoofed,service.namespace=formslogic")

	runtime := Initialize(context.Background(), "0.0.test")
	if status := runtime.Status(); status.State != StateConfigured {
		t.Fatalf("tracing status = %#v", status)
	}

	router := mux.NewRouter()
	router.HandleFunc("/demo/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(CurrentTrace(r.Context()).TraceID))
	}).Methods(http.MethodGet)
	router.Use(runtime.Middleware())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/demo/42", nil))

	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}

	var exportRequest *collectortrace.ExportTraceServiceRequest
	select {
	case exportRequest = <-collector.requests:
	case <-time.After(3 * time.Second):
		t.Fatal("OTLP collector did not receive a trace export")
	}

	spanCount := 0
	foundResource := false
	foundRouteSpan := false
	for _, resourceSpans := range exportRequest.ResourceSpans {
		attributes := make(map[string]string)
		for _, item := range resourceSpans.Resource.Attributes {
			attributes[item.Key] = item.Value.GetStringValue()
		}
		if attributes["service.name"] == "webrequesttest" &&
			attributes["service.version"] == "0.0.test" &&
			attributes["k8s.pod.name"] == "webrequesttest-test" &&
			attributes["k8s.namespace.name"] == "fl-production" &&
			attributes["service.namespace"] == "formslogic" {
			foundResource = true
		}
		for _, scopeSpans := range resourceSpans.ScopeSpans {
			for _, span := range scopeSpans.Spans {
				spanCount++
				if span.Name == "GET /demo/{id}" && hex.EncodeToString(span.TraceId) == response.Body.String() {
					foundRouteSpan = true
				}
			}
		}
	}
	if spanCount != 1 || !foundRouteSpan || !foundResource {
		t.Fatalf("exported spans=%d route=%t resource=%t", spanCount, foundRouteSpan, foundResource)
	}
}

type countingExporter struct {
	shutdowns atomic.Int32
}

type recordingTraceCollector struct {
	collectortrace.UnimplementedTraceServiceServer
	requests chan *collectortrace.ExportTraceServiceRequest
}

func (collector *recordingTraceCollector) Export(_ context.Context, request *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	collector.requests <- request
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

func (exporter *countingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (exporter *countingExporter) Shutdown(context.Context) error {
	exporter.shutdowns.Add(1)
	return nil
}

func testRuntime(provider *sdktrace.TracerProvider, publicEndpoint bool) *Runtime {
	return &Runtime{
		status: Status{
			Enabled:            true,
			State:              StateConfigured,
			Exporter:           "otlp/grpc",
			ServiceName:        "webrequesttest",
			EndpointConfigured: true,
			PublicEndpoint:     publicEndpoint,
		},
		provider:    provider,
		propagators: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		sdkProvider: provider,
	}
}

func clearTracingEnvironment(t *testing.T) {
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
