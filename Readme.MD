# simpleapp

`simpleapp` is a small Kubernetes request and network diagnostics application for workshops, troubleshooting, and load-balancer demonstrations. Its home page shows the request that actually reached the pod, the pod/runtime serving it, a bounded port checker that runs from the pod's network namespace, and optional OpenTelemetry trace correlation.

The container remains deliberately small and has no Kubernetes API dependency or RBAC requirement.

## Quick start

Run locally with Go:

```bash
go run ./cmd/simpleapp.go
```

Then open <http://localhost:8080>. The process also starts TCP echo listeners on ports 5060 and 5061 and a UDP echo listener on port 5060.

Build and run the container:

```bash
make docker-build
docker run --rm --name simpleapp \
  -p 8080:8080 \
  -p 5060:5060/tcp \
  -p 5060:5060/udp \
  -p 5061:5061/tcp \
  ghcr.io/jhoelzel/webrequesttest:0.0.5
```

## What the dashboard shows

The request section includes:

- method, request URI, host, HTTP protocol, and the pod-side transport scheme;
- direct peer and local socket addresses, with IPv4 and IPv6 handled separately;
- all multi-value request headers, query parameters, trailers, transfer encoding, and a bounded body preview;
- TLS version, cipher, SNI, negotiated protocol, and client-certificate counts when TLS reaches the pod;
- highlighted `Forwarded`, `X-Forwarded-*`, `X-Real-IP`, Cloudflare, Envoy, request-ID, and trace-related headers;
- the active OpenTelemetry trace ID, server span ID, sampling state, and safe exporter status when tracing is enabled;
- a reconstructed request and a JSON representation for scripts.

The runtime section includes:

- pod name, namespace, UID and IP, host IP, node, and service account from the Downward API;
- configured CPU/memory requests and limits;
- image/build metadata, hostname, process identity, uptime, Go version, OS/architecture, memory, and goroutine counts;
- network interfaces, addresses, and the kernel-selected outbound interface address.

Credential-like header and query values are shown as `[REDACTED]` by default. Request bodies are limited to a 64 KiB preview and are not written to application logs.

## Endpoints

| Endpoint | Purpose |
| --- | --- |
| `/` | Interactive HTML dashboard; accepts any HTTP method so request bodies can be demonstrated |
| `/?format=json&pretty=1` | JSON view of the request and runtime |
| `/api/request` | Machine-readable request/runtime echo; accepts any HTTP method |
| `/api/port-check` | Bounded TCP/UDP checker; `POST` with JSON only |
| `/status/` | Compact build and process information |
| `/status/healthz` | Kubernetes liveness/startup probe |
| `/status/readyz` | Kubernetes readiness probe |

Example request inspection:

```bash
curl -sS \
  -H 'X-Forwarded-For: 203.0.113.10, 10.0.0.8' \
  -H 'X-Forwarded-Proto: https' \
  -H 'X-Request-ID: workshop-42' \
  'http://localhost:8080/api/request?pretty=1'
```

## Port checker

The dashboard has text boxes for a target and a port expression. Port expressions support:

```text
443
80,443,8080
5000-5010
53,80,443,5000-5010
all
```

`all` means ports 1 through 65535. Explicit expressions are normalized, deduplicated, and limited to 4096 ports; use `all` for a full scan. Checks use at most 128 workers, a per-port timeout between 1 and 5000 ms, a 20-second overall deadline, request cancellation, and one active scan per pod. Full scans retain only open TCP ports or responsive UDP ports instead of returning tens of thousands of rows.

Example API call:

```bash
curl -sS http://localhost:8080/api/port-check \
  -H 'Content-Type: application/json' \
  --data '{"target":"self","ports":"80,443,5060-5061","protocol":"both","timeout_ms":500}'
```

TCP `open` means a connection completed. UDP `responded` means the checker sent a datagram and received one back. UDP silence is `unknown`; a generic UDP probe cannot prove that a port is closed.

### Target safety policy

A diagnostics pod exposed through a public load balancer must not become an unrestricted internal-network scanner. By default, only these targets are permitted:

- `self` and loopback;
- addresses assigned to the pod's local interfaces;
- the Downward API `POD_IP`, when present.

Add remote targets explicitly with `PORT_CHECK_ALLOWED_TARGETS`. Values can be exact hostnames, IP addresses, or CIDRs:

```yaml
env:
- name: PORT_CHECK_ALLOWED_TARGETS
  value: "orders.default.svc.cluster.local,10.96.0.0/12,2001:db8:42::/64"
```

An explicit hostname is resolved once and the validated IP literal is used for the scan. `PORT_CHECK_ALLOWED_TARGETS=*` permits everything, but should only be used in an isolated workshop cluster with an appropriate NetworkPolicy.

## Optional OTLP tracing

Tracing is disabled by default and creates no exporter or background workers. Enable the application gate and configure the standard OpenTelemetry OTLP variables to export route-aware HTTP server spans over gRPC:

```bash
SIMPLEAPP_TRACING_ENABLED=true \
OTEL_SERVICE_NAME=webrequesttest \
OTEL_TRACES_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://grafana-tempo.monitoring.svc.cluster.local:4317 \
OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc \
OTEL_EXPORTER_OTLP_TRACES_INSECURE=true \
OTEL_TRACES_SAMPLER=parentbased_always_on \
go run ./cmd/simpleapp.go
```

The middleware uses Gorilla route templates such as `GET /api/request`, extracts W3C Trace Context and Baggage, correlates access logs with trace/span IDs, and excludes Kubernetes health/readiness probes. A port-check request creates one aggregate child span, never one span per probed port.

`SIMPLEAPP_TRACE_PUBLIC_ENDPOINT` defaults to `true`: a client-provided trace context is linked instead of becoming the parent, which prevents an untrusted Internet client from controlling the server trace and sampling decision. Set it to `false` only when the application is reachable exclusively through a trusted proxy that sanitizes incoming trace headers; this preserves an end-to-end parent/child trace from that proxy.

The diagnostics state `configured` means the asynchronous exporter initialized. It does not claim that the collector is reachable or accepting spans. TLS is the default for OTLP; use insecure transport only for a trusted in-cluster collector. Exporter headers and client key/certificate settings can be supplied with the standard `OTEL_EXPORTER_OTLP_TRACES_*` variables, but credentials must come from Kubernetes Secrets and are never rendered by the dashboard or API. Do not place secrets in `OTEL_RESOURCE_ATTRIBUTES`.

The OTLP endpoint must be an absolute `http://` or `https://` URL with a host; credentials, query strings, and fragments are rejected. `OTEL_SDK_DISABLED=true` or `OTEL_TRACES_EXPORTER=none` disables tracing even when the application gate is enabled. Malformed exporter settings fail open as the safe categorical state `init_failed` and are never copied into logs or diagnostic responses. The application validates both general and trace-specific exporter variables—even an overridden lower-precedence value—so stale malformed settings must be removed. Configured CA or client credentials take precedence over insecure transport and are reported as TLS.

## Kubernetes deployment

The checked-in Kustomize overlays create their namespaces:

```bash
# NodePort workshop environment
kubectl apply -k kube-manifests/kustomize/overlays/dev

# LoadBalancer environment
kubectl apply -k kube-manifests/kustomize/overlays/prod
```

Or generate release manifests for a chosen version and image repository:

```bash
make kube-manifests VERSION=0.0.5
make kube-apply-lb NS=default
# or: make kube-apply-nodeport NS=default
```

The pod runs as UID/GID 65532 with a read-only root filesystem, no Linux capabilities, no privilege escalation, the runtime-default seccomp profile, and no mounted service-account token. Startup, liveness, and readiness checks stay independent of request inspection and port scans.

The HTTP Services intentionally expose only port 80 to container port 8080. The 5060/5061 listeners are declared as container ports for pod-to-pod exercises, but are not published by the public Service. Add dedicated Services if a workshop specifically needs TCP or UDP exposure.

If upgrading a Deployment created from an older manifest, delete and recreate it once. Older releases included `version` in `spec.selector`; Kubernetes does not allow changing a Deployment selector in place.

## Useful demonstrations

### Watch load balancing across replicas

```bash
kubectl -n simpleapp-prod scale deployment/simpleapp --replicas=3
```

Refresh the dashboard. The hero color is stable per pod UID, and the pod name plus `X-Simpleapp-Pod` response header makes each backend visible.

### Compare layer 4 and layer 7 behavior

A Kubernetes `Service` with `type: LoadBalancer` is commonly layer 4. It generally does not add HTTP forwarding headers. Put the Service behind an HTTP Ingress or Gateway to demonstrate `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Proto`, and trace headers.

Forwarding headers are displayed as untrusted claims. Only the deployment knows which proxy CIDRs are trusted and whether that proxy removes client-supplied forwarding headers.

### Observe source-address changes

Compare the direct peer shown by the pod with different Service implementations and `externalTrafficPolicy` settings. `externalTrafficPolicy: Local` can preserve the original source address on supported platforms, but changes traffic distribution and availability behavior.

### Exercise probes and shutdown

The HTTP, TCP, and UDP listeners share signal-driven shutdown. Delete a pod while sending requests to observe readiness removal, termination grace, and graceful HTTP shutdown without making liveness depend on any external service.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port, validated to 1–65535; 5060 and 5061 are reserved for the demo listeners |
| `DIAGNOSTIC_REDACT_NAMES` | empty | Additional comma-separated header or query names whose values must be redacted |
| `PORT_CHECK_ALLOWED_TARGETS` | local/self only | Comma-separated hostnames, IPs, CIDRs, or `*` |
| `SHOW_SENSITIVE_DATA` | `false` | Shows credential-like header/query values; unsafe on shared or public demos |
| `SIMPLEAPP_TRACING_ENABLED` | `false` | Initializes the optional OTLP/gRPC trace pipeline when `true`; requires an OTLP endpoint |
| `SIMPLEAPP_TRACE_PUBLIC_ENDPOINT` | `true` | Links untrusted incoming trace context; set `false` only behind a trace-header-sanitizing trusted proxy |
| `OTEL_SERVICE_NAME` | `simpleapp` | OpenTelemetry `service.name` resource attribute |
| `OTEL_TRACES_EXPORTER` | `otlp` when enabled | Must be `otlp`; no metrics or log SDK is initialized |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | required when enabled | OTLP gRPC collector; falls back to `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `grpc` | Only `grpc` is supported; the general protocol variable is a fallback |
| `OTEL_EXPORTER_OTLP_TRACES_INSECURE` | `false` | Disables transport security; use only for a trusted in-cluster collector |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on` | Standard SDK sampler configuration; ratio values are validated from 0 through 1 |
| `OTEL_RESOURCE_ATTRIBUTES` | empty | Additional OpenTelemetry resource attributes; never include secrets |
| `POD_NAME`, `POD_NAMESPACE`, `POD_UID`, `POD_IP`, `HOST_IP`, `NODE_NAME`, `SERVICE_ACCOUNT` | empty locally | Allowlisted Downward API identity fields |
| `CPU_REQUEST_MILLICORES`, `CPU_LIMIT_MILLICORES` | empty locally | Resource values injected by the manifest |
| `MEMORY_REQUEST_BYTES`, `MEMORY_LIMIT_BYTES` | empty locally | Resource values injected by the manifest |

`SHOW_SENSITIVE_DATA=true` is intentionally noisy in the process log. Do not enable it when an ingress injects internal authorization headers or when the dashboard is being recorded.

## Development and verification

Development and container builds use Go 1.25 or newer.

```bash
make fmt
make vet
make test
make build
```

The tests cover request redaction and body restoration, IPv6 socket parsing, TLS/proxy fields, HTML escaping, JSON rendering, port grammar and policy, bounded concurrency/cancellation, real loopback TCP and UDP probes, access-log secret handling, and Kubernetes health endpoints.

The Dockerfile performs a multi-stage build and copies only the statically linked application into the non-root distroless runtime image.
