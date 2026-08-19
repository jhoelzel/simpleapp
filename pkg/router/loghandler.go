package router

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jhoelzel/simpleapp/pkg/telemetry"
)

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(body)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *responseRecorder) Flush() {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (r *responseRecorder) Push(target string, options *http.PushOptions) error {
	pusher, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (r *responseRecorder) ReadFrom(source io.Reader) (int64, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	var (
		count int64
		err   error
	)
	if readerFrom, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		count, err = readerFrom.ReadFrom(source)
	} else {
		count, err = io.Copy(r.ResponseWriter, source)
	}
	r.bytes += int(count)
	return count, err
}

func accessLogMiddleware(release string) func(http.Handler) http.Handler {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			traceContext := telemetry.CurrentTrace(r.Context())
			requestID := safeRequestID(r.Header.Get("X-Request-ID"))
			if requestID == "" {
				requestID = newRequestID()
			}
			telemetry.SetRequestID(r.Context(), requestID)

			w.Header().Set("X-Request-ID", requestID)
			w.Header().Set("X-Simpleapp-Pod", hostname)
			w.Header().Set("X-Simpleapp-Version", release)

			recorder := &responseRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)
			if recorder.status == 0 {
				recorder.status = http.StatusOK
			}

			log.Printf(
				"request_id=%q trace_id=%q span_id=%q sampled=%t method=%q path=%q proto=%q peer=%q status=%d bytes=%d duration=%s",
				requestID,
				traceContext.TraceID,
				traceContext.SpanID,
				traceContext.Sampled,
				r.Method,
				r.URL.Path,
				r.Proto,
				r.RemoteAddr,
				recorder.status,
				recorder.bytes,
				time.Since(started).Round(time.Microsecond),
			)
		})
	}
}

func safeRequestID(value string) string {
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._:/", char) {
			return ""
		}
	}
	return value
}

func newRequestID() string {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err == nil {
		return hex.EncodeToString(bytes)
	}
	return time.Now().UTC().Format("20060102T150405.000000000")
}
