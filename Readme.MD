# simpleapp

`simpleapp` is a small Kubernetes request and network diagnostics application for workshops, troubleshooting, and load-balancer demonstrations. Its home page shows the request that actually reached the pod, the pod/runtime serving it, and a bounded port checker that runs from the pod's network namespace.

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
  ghcr.io/jhoelzel/webrequesttest:0.0.4
```

## What the dashboard shows

The request section includes:

- method, request URI, host, HTTP protocol, and the pod-side transport scheme;
- direct peer and local socket addresses, with IPv4 and IPv6 handled separately;
- all multi-value request headers, query parameters, trailers, transfer encoding, and a bounded body preview;
- TLS version, cipher, SNI, negotiated protocol, and client-certificate counts when TLS reaches the pod;
- highlighted `Forwarded`, `X-Forwarded-*`, `X-Real-IP`, Cloudflare, Envoy, request-ID, and trace-related headers;
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
make kube-manifests VERSION=0.0.4
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
| `POD_NAME`, `POD_NAMESPACE`, `POD_UID`, `POD_IP`, `HOST_IP`, `NODE_NAME`, `SERVICE_ACCOUNT` | empty locally | Allowlisted Downward API identity fields |
| `CPU_REQUEST_MILLICORES`, `CPU_LIMIT_MILLICORES` | empty locally | Resource values injected by the manifest |
| `MEMORY_REQUEST_BYTES`, `MEMORY_LIMIT_BYTES` | empty locally | Resource values injected by the manifest |

`SHOW_SENSITIVE_DATA=true` is intentionally noisy in the process log. Do not enable it when an ingress injects internal authorization headers or when the dashboard is being recorded.

## Development and verification

```bash
make fmt
make vet
make test
make build
```

The tests cover request redaction and body restoration, IPv6 socket parsing, TLS/proxy fields, HTML escaping, JSON rendering, port grammar and policy, bounded concurrency/cancellation, real loopback TCP and UDP probes, access-log secret handling, and Kubernetes health endpoints.

The Dockerfile performs a multi-stage build and copies only the statically linked application into the non-root distroless runtime image.
