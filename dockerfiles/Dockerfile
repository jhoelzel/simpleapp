FROM golang:1.25-bookworm AS build

ARG BUILD_TIME=unset
ARG COMMIT=unset
ARG VERSION=unset

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X github.com/jhoelzel/simpleapp/pkg/version.Release=${VERSION} \
      -X github.com/jhoelzel/simpleapp/pkg/version.Commit=${COMMIT} \
      -X github.com/jhoelzel/simpleapp/pkg/version.BuildTime=${BUILD_TIME}" \
    -o /out/simpleapp ./cmd/simpleapp.go

FROM gcr.io/distroless/static-debian12:nonroot

ARG BUILD_TIME=unset
ARG COMMIT=unset
ARG VERSION=unset

LABEL org.opencontainers.image.title="simpleapp" \
      org.opencontainers.image.description="Kubernetes request and network diagnostics demo" \
      org.opencontainers.image.created="${BUILD_TIME}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/jhoelzel/simpleapp"

COPY --from=build /out/simpleapp /simpleapp

EXPOSE 8080/tcp
EXPOSE 5060/tcp
EXPOSE 5060/udp
EXPOSE 5061/tcp

USER 65532:65532
ENTRYPOINT ["/simpleapp"]
