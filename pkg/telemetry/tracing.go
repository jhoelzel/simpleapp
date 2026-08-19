// Package telemetry configures optional OpenTelemetry tracing for simpleapp.
package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	StateDisabled   = "disabled"
	StateConfigured = "configured"
	StateInitFailed = "init_failed"

	defaultServiceName = "simpleapp"
)

// Status is safe to expose in the diagnostics API. It deliberately omits the
// collector address, exporter headers, TLS file paths, resource attributes,
// and raw initialization errors.
type Status struct {
	Enabled            bool   `json:"enabled"`
	State              string `json:"state"`
	Exporter           string `json:"exporter,omitempty"`
	ServiceName        string `json:"serviceName,omitempty"`
	EndpointConfigured bool   `json:"endpointConfigured"`
	Insecure           bool   `json:"insecure"`
	PublicEndpoint     bool   `json:"publicEndpoint"`
}

// TraceContext describes the server span associated with one HTTP request.
type TraceContext struct {
	Active    bool   `json:"active"`
	TraceID   string `json:"traceId,omitempty"`
	SpanID    string `json:"spanId,omitempty"`
	Sampled   bool   `json:"sampled"`
	Recording bool   `json:"recording"`
}

type config struct {
	enabled            bool
	serviceName        string
	endpoint           string
	endpointConfigured bool
	insecure           bool
	publicEndpoint     bool
}

var otelRuntimeErrorNotice sync.Once

// Runtime owns the optional trace provider and its HTTP middleware.
type Runtime struct {
	status      Status
	provider    trace.TracerProvider
	propagators propagation.TextMapPropagator
	sdkProvider *sdktrace.TracerProvider
	shutdown    sync.Once
	shutdownErr error
}

type statusContextKey struct{}

// Initialize builds an OTLP/gRPC trace provider when SIMPLEAPP_TRACING_ENABLED is
// true. Invalid tracing configuration fails open: the application continues
// serving with a categorical init_failed status and no exporter.
func Initialize(ctx context.Context, release string) *Runtime {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := configFromEnvironment()
	if err != nil {
		return failedRuntime(cfg)
	}
	if !cfg.enabled {
		return disabledRuntime(cfg)
	}
	configureSafeOTelLogging()

	// Configuration is validated before the exporter reads the standard OTEL
	// environment variables. No raw exporter values or errors are exposed.
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return failedRuntime(cfg)
	}

	res, err := resource.New(
		ctx,
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		// Application identity wins over arbitrary resource attributes so the
		// exported resource and public status cannot disagree.
		resource.WithAttributes(resourceAttributes(cfg.serviceName, release)...),
	)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = exporter.Shutdown(shutdownCtx)
		cancel()
		return failedRuntime(cfg)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	propagators := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	return &Runtime{
		status: Status{
			Enabled:            true,
			State:              StateConfigured,
			Exporter:           "otlp/grpc",
			ServiceName:        cfg.serviceName,
			EndpointConfigured: cfg.endpointConfigured,
			Insecure:           cfg.insecure,
			PublicEndpoint:     cfg.publicEndpoint,
		},
		provider:    provider,
		propagators: propagators,
		sdkProvider: provider,
	}
}

// Middleware returns Gorilla mux instrumentation plus a request-scoped copy
// of the safe tracing status. Kubernetes probe traffic is never traced.
func (runtime *Runtime) Middleware() func(http.Handler) http.Handler {
	if runtime == nil {
		runtime = disabledRuntime(config{})
	}
	withStatus := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), statusContextKey{}, runtime.status)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	if runtime.status.State != StateConfigured {
		return withStatus
	}

	options := []otelmux.Option{
		otelmux.WithTracerProvider(runtime.provider),
		otelmux.WithPropagators(runtime.propagators),
		otelmux.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/status/healthz" && r.URL.Path != "/status/readyz"
		}),
	}
	if runtime.status.PublicEndpoint {
		options = append(options, otelmux.WithPublicEndpoint())
	}
	instrument := otelmux.Middleware(runtime.status.ServiceName, options...)
	return func(next http.Handler) http.Handler {
		return instrument(withStatus(next))
	}
}

// Shutdown flushes completed spans at most once. Callers should provide a
// bounded context so collector outages cannot delay process termination.
func (runtime *Runtime) Shutdown(ctx context.Context) error {
	if runtime == nil || runtime.sdkProvider == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime.shutdown.Do(func() {
		runtime.shutdownErr = runtime.sdkProvider.Shutdown(ctx)
	})
	return runtime.shutdownErr
}

// Status returns the safe process-level tracing configuration state.
func (runtime *Runtime) Status() Status {
	if runtime == nil {
		return Status{State: StateDisabled, PublicEndpoint: true}
	}
	return runtime.status
}

// CurrentStatus returns the safe tracing status placed on a request context.
func CurrentStatus(ctx context.Context) Status {
	if ctx != nil {
		if status, ok := ctx.Value(statusContextKey{}).(Status); ok {
			return status
		}
	}
	return Status{State: StateDisabled, PublicEndpoint: true}
}

// CurrentTrace returns identifiers for the active request span, when present.
func CurrentTrace(ctx context.Context) TraceContext {
	if ctx == nil {
		return TraceContext{}
	}
	span := trace.SpanFromContext(ctx)
	spanContext := span.SpanContext()
	if !spanContext.IsValid() {
		return TraceContext{}
	}
	return TraceContext{
		Active:    true,
		TraceID:   spanContext.TraceID().String(),
		SpanID:    spanContext.SpanID().String(),
		Sampled:   spanContext.IsSampled(),
		Recording: span.IsRecording(),
	}
}

// SetRequestID adds an already validated request identifier to the active
// server span without recording any request headers, URL parameters, or body.
func SetRequestID(ctx context.Context, requestID string) {
	if ctx == nil || requestID == "" {
		return
	}
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(attribute.String("request.id", requestID))
	}
}

func configFromEnvironment() (config, error) {
	rawServiceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	serviceNameInvalid := rawServiceName != "" && (len(rawServiceName) > 256 || !utf8.ValidString(rawServiceName) || containsControlCharacter(rawServiceName))
	if rawServiceName == "" || serviceNameInvalid {
		rawServiceName = defaultServiceName
	}
	cfg := config{
		serviceName:    rawServiceName,
		publicEndpoint: true,
	}

	rawEnabled := strings.TrimSpace(os.Getenv("SIMPLEAPP_TRACING_ENABLED"))
	if rawEnabled == "" {
		return cfg, nil
	}
	enabled, err := strconv.ParseBool(rawEnabled)
	if err != nil {
		cfg.enabled = true
		return cfg, fmt.Errorf("invalid SIMPLEAPP_TRACING_ENABLED")
	}
	cfg.enabled = enabled
	if !enabled {
		return cfg, nil
	}

	if rawSDKDisabled := strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")); rawSDKDisabled != "" {
		sdkDisabled, parseErr := parseStandardBoolean(rawSDKDisabled)
		if parseErr != nil {
			return cfg, fmt.Errorf("invalid OpenTelemetry SDK disabled setting")
		}
		if sdkDisabled {
			cfg.enabled = false
			return cfg, nil
		}
	}

	exporter := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER")))
	switch exporter {
	case "none":
		cfg.enabled = false
		return cfg, nil
	case "", "otlp":
	default:
		return cfg, fmt.Errorf("only the OTLP trace exporter is supported")
	}
	if serviceNameInvalid {
		return cfg, fmt.Errorf("invalid OpenTelemetry service name")
	}

	if err := validateOTLPEnvironment(); err != nil {
		return cfg, err
	}

	endpoint := firstEnvironmentValue(
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	)
	cfg.endpoint = endpoint
	cfg.endpointConfigured = endpoint != ""
	if !cfg.endpointConfigured {
		return cfg, fmt.Errorf("OTLP endpoint is required when tracing is enabled")
	}
	protocol := strings.ToLower(firstEnvironmentValue(
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
	))
	if protocol != "" && protocol != "grpc" {
		return cfg, fmt.Errorf("only the OTLP gRPC protocol is supported")
	}

	cfg.insecure = effectiveOTLPInsecure(
		endpoint,
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")),
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE")),
		otelTLSCredentialsConfigured(),
	)

	rawPublicEndpoint := strings.TrimSpace(os.Getenv("SIMPLEAPP_TRACE_PUBLIC_ENDPOINT"))
	if rawPublicEndpoint != "" {
		cfg.publicEndpoint, err = strconv.ParseBool(rawPublicEndpoint)
		if err != nil {
			return cfg, fmt.Errorf("invalid public endpoint setting")
		}
	}

	sampler := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	switch sampler {
	case "", "always_on", "always_off", "traceidratio", "parentbased_always_on", "parentbased_always_off", "parentbased_traceidratio":
	default:
		return cfg, fmt.Errorf("unsupported trace sampler")
	}
	if sampler == "traceidratio" || sampler == "parentbased_traceidratio" {
		if samplerArg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); samplerArg != "" {
			ratio, ratioErr := strconv.ParseFloat(samplerArg, 64)
			if ratioErr != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
				return cfg, fmt.Errorf("invalid trace sampler ratio")
			}
		}
	}
	return cfg, nil
}

// validateOTLPEnvironment validates every exporter value the upstream parser
// will inspect, including lower-precedence values. This prevents malformed
// credentials, endpoints, and file paths from being echoed by dependency logs
// and turns partial/fallback configuration into a categorical init failure.
func validateOTLPEnvironment() error {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if err := validateEndpoint(value); err != nil {
				return fmt.Errorf("invalid OTLP endpoint")
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if _, err := parseStandardBoolean(value); err != nil {
				return fmt.Errorf("invalid OTLP insecure setting")
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if err := validateHeaders(value); err != nil {
				return fmt.Errorf("invalid OTLP headers")
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			milliseconds, err := strconv.ParseInt(value, 10, 64)
			if err != nil || milliseconds <= 0 || milliseconds > math.MaxInt64/int64(time.Millisecond) {
				return fmt.Errorf("invalid OTLP timeout")
			}
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" && value != "gzip" {
			return fmt.Errorf("invalid OTLP compression")
		}
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE"} {
		if path := strings.TrimSpace(os.Getenv(name)); path != "" {
			contents, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("invalid OTLP certificate")
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(contents) {
				return fmt.Errorf("invalid OTLP certificate")
			}
		}
	}
	for _, pair := range [][2]string{
		{"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY"},
		{"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY"},
	} {
		certPath := strings.TrimSpace(os.Getenv(pair[0]))
		keyPath := strings.TrimSpace(os.Getenv(pair[1]))
		if (certPath == "") != (keyPath == "") {
			return fmt.Errorf("incomplete OTLP client certificate configuration")
		}
		if certPath != "" {
			certPEM, certErr := os.ReadFile(certPath)
			keyPEM, keyErr := os.ReadFile(keyPath)
			if certErr != nil || keyErr != nil {
				return fmt.Errorf("invalid OTLP client certificate")
			}
			if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
				return fmt.Errorf("invalid OTLP client certificate")
			}
		}
	}
	if value := strings.TrimSpace(os.Getenv("OTEL_RESOURCE_ATTRIBUTES")); value != "" {
		for _, pair := range strings.Split(value, ",") {
			key, encoded, found := strings.Cut(pair, "=")
			if !found || strings.TrimSpace(key) == "" {
				return fmt.Errorf("invalid OpenTelemetry resource attributes")
			}
			decoded, err := url.PathUnescape(strings.TrimSpace(encoded))
			if err != nil || !utf8.ValidString(decoded) {
				return fmt.Errorf("invalid OpenTelemetry resource attributes")
			}
		}
	}
	return nil
}

func parseStandardBoolean(value string) (bool, error) {
	switch {
	case strings.EqualFold(strings.TrimSpace(value), "true"):
		return true, nil
	case strings.EqualFold(strings.TrimSpace(value), "false"):
		return false, nil
	default:
		return false, fmt.Errorf("boolean must be true or false")
	}
}

func effectiveOTLPInsecure(endpoint, generalSetting, traceSetting string, tlsCredentialsConfigured bool) bool {
	parsed, _ := url.Parse(endpoint)
	insecure := strings.EqualFold(parsed.Scheme, "http")
	if generalSetting != "" {
		insecure = strings.EqualFold(generalSetting, "true")
	}
	if traceSetting != "" {
		insecure = strings.EqualFold(traceSetting, "true")
	}
	// The exporter prioritizes configured gRPC transport credentials over its
	// insecure flag, even though the boolean environment options are parsed
	// after the certificate options.
	if tlsCredentialsConfigured {
		return false
	}
	return insecure
}

func otelTLSCredentialsConfigured() bool {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	for _, pair := range [][2]string{
		{"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY"},
		{"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY"},
	} {
		if strings.TrimSpace(os.Getenv(pair[0])) != "" && strings.TrimSpace(os.Getenv(pair[1])) != "" {
			return true
		}
	}
	return false
}

func validateEndpoint(value string) error {
	if len(value) > 2048 {
		return fmt.Errorf("endpoint is too long")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" {
		return fmt.Errorf("endpoint must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") {
		return fmt.Errorf("endpoint credentials, queries, and fragments are not allowed")
	}
	if strings.ContainsAny(parsed.Host, "\r\n\t ") {
		return fmt.Errorf("endpoint host contains whitespace")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return fmt.Errorf("endpoint port is missing")
	}
	if port := parsed.Port(); port != "" {
		number, parseErr := strconv.Atoi(port)
		if parseErr != nil || number < 1 || number > 65535 {
			return fmt.Errorf("endpoint port is invalid")
		}
	}
	// Verify bracket and host:port syntax without requiring DNS resolution.
	if strings.Contains(parsed.Host, ":") {
		if _, _, splitErr := net.SplitHostPort(parsed.Host); splitErr != nil && parsed.Port() != "" {
			return fmt.Errorf("endpoint host syntax is invalid")
		}
	}
	return nil
}

func validateHeaders(value string) error {
	if len(value) > 64*1024 {
		return fmt.Errorf("headers are too large")
	}
	for _, pair := range strings.Split(value, ",") {
		name, encoded, found := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		if !found || !validHeaderName(name) {
			return fmt.Errorf("header syntax is invalid")
		}
		decoded, err := url.PathUnescape(encoded)
		if err != nil || !utf8.ValidString(decoded) || containsControlCharacter(decoded) {
			return fmt.Errorf("header value is invalid")
		}
	}
	return nil
}

func containsControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character > 127 || !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", character)) {
			return false
		}
	}
	return true
}

// configureSafeOTelLogging prevents upstream configuration and exporter
// errors from echoing endpoint credentials, metadata headers, or TLS paths.
// The application emits only one categorical degradation notice.
func configureSafeOTelLogging() {
	otel.SetLogger(logr.Discard())
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		otelRuntimeErrorNotice.Do(func() {
			log.Print("OpenTelemetry reported an internal error; trace export may be degraded")
		})
	}))
}

func disabledRuntime(cfg config) *Runtime {
	return &Runtime{
		status: Status{
			State:          StateDisabled,
			ServiceName:    cfg.serviceName,
			PublicEndpoint: true,
		},
		provider:    noop.NewTracerProvider(),
		propagators: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}
}

func failedRuntime(cfg config) *Runtime {
	return &Runtime{
		status: Status{
			Enabled:            cfg.enabled,
			State:              StateInitFailed,
			Exporter:           "otlp/grpc",
			ServiceName:        cfg.serviceName,
			EndpointConfigured: cfg.endpointConfigured,
			Insecure:           cfg.insecure,
			PublicEndpoint:     cfg.publicEndpoint,
		},
		provider:    noop.NewTracerProvider(),
		propagators: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}
}

func firstEnvironmentValue(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func resourceAttributes(serviceName, release string) []attribute.KeyValue {
	attributes := []attribute.KeyValue{
		attribute.String("service.name", serviceName),
	}
	if release != "" {
		attributes = append(attributes, attribute.String("service.version", release))
	}
	for _, item := range []struct {
		attribute string
		env       string
	}{
		{attribute: "service.instance.id", env: "POD_UID"},
		{attribute: "k8s.namespace.name", env: "POD_NAMESPACE"},
		{attribute: "k8s.pod.name", env: "POD_NAME"},
		{attribute: "k8s.pod.uid", env: "POD_UID"},
		{attribute: "k8s.node.name", env: "NODE_NAME"},
		{attribute: "k8s.container.name", env: "CONTAINER_NAME"},
	} {
		if value := strings.TrimSpace(os.Getenv(item.env)); value != "" {
			attributes = append(attributes, attribute.String(item.attribute, value))
		}
	}
	return attributes
}
