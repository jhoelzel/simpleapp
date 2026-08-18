package homecontroller

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
)

//go:embed dashboard.html
var dashboardHTML string

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"join":        strings.Join,
	"formatBytes": formatBytes,
}).Parse(dashboardHTML))

type dashboardData struct {
	Request       RequestDetails
	Runtime       RuntimeDetails
	DefaultTarget string
	PodHue        int
}

type diagnosticEnvelope struct {
	Request       RequestDetails `json:"request"`
	Runtime       RuntimeDetails `json:"runtime"`
	DemoListeners []string       `json:"demoListeners"`
	Notes         []string       `json:"notes"`
}

func homeEndpoint(w http.ResponseWriter, r *http.Request) {
	requestDetails := CollectRequestDetails(r)
	runtimeDetails := CollectRuntimeDetails()

	if wantsJSON(r) {
		writeDiagnosticsJSON(w, r, requestDetails, runtimeDetails)
		return
	}

	SetDiagnosticResponseHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")

	identity := runtimeDetails.Kubernetes.PodUID
	if identity == "" {
		identity = runtimeDetails.Hostname
	}
	data := dashboardData{
		Request:       requestDetails,
		Runtime:       runtimeDetails,
		DefaultTarget: "self",
		PodHue:        identityHue(identity),
	}
	if err := dashboardTemplate.Execute(w, data); err != nil {
		log.Printf("render dashboard: %v", err)
	}
}

func requestDetailsEndpoint(w http.ResponseWriter, r *http.Request) {
	requestDetails := CollectRequestDetails(r)
	runtimeDetails := CollectRuntimeDetails()
	writeDiagnosticsJSON(w, r, requestDetails, runtimeDetails)
}

func writeDiagnosticsJSON(w http.ResponseWriter, r *http.Request, requestDetails RequestDetails, runtimeDetails RuntimeDetails) {
	SetDiagnosticResponseHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(true)
	if r.URL.Query().Get("pretty") == "1" {
		encoder.SetIndent("", "  ")
	}
	response := diagnosticEnvelope{
		Request: requestDetails,
		Runtime: runtimeDetails,
		DemoListeners: []string{
			diagnosticRequestPort(requestDetails) + "/tcp (HTTP)",
			"5060/tcp (echo)",
			"5060/udp (echo)",
			"5061/tcp (echo)",
		},
		Notes: []string{
			"Forwarding headers are untrusted claims unless the direct peer is a configured trusted proxy.",
			"The server can only show data retained by the load balancer and Go's HTTP parser; stripped headers and original wire bytes cannot be recovered.",
			"Credential-like header and query values are redacted unless SHOW_SENSITIVE_DATA=true.",
		},
	}
	if err := encoder.Encode(response); err != nil {
		log.Printf("encode request diagnostics: %v", err)
	}
}

func diagnosticRequestPort(requestDetails RequestDetails) string {
	if requestDetails.LocalSocket.Port != "" {
		return requestDetails.LocalSocket.Port
	}
	return diagnosticHTTPPort()
}

func diagnosticHTTPPort() string {
	value := strings.TrimSpace(os.Getenv("PORT"))
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 || port == 5060 || port == 5061 {
		return "8080"
	}
	return strconv.Itoa(port)
}

func wantsJSON(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		return true
	}
	for _, accepted := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(accepted))
		if err != nil || mediaType != "application/json" {
			continue
		}
		quality := 1.0
		if rawQuality, ok := parameters["q"]; ok {
			quality, err = strconv.ParseFloat(rawQuality, 64)
		}
		if err == nil && quality > 0 && quality <= 1 {
			return true
		}
	}
	return false
}

func identityHue(identity string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(identity))
	return int(hash.Sum32() % 360)
}

func formatBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return strconv.FormatUint(value, 10) + " B"
	}
	divisor, exponent := uint64(unit), 0
	for quotient := value / unit; quotient >= unit && exponent < 5; quotient /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(divisor), "KMGTPE"[exponent])
}

func init() {
	// Make the presence of this deliberately unsafe demo switch visible in the
	// process log without ever logging a sensitive value.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SHOW_SENSITIVE_DATA")), "true") {
		log.Print("warning: SHOW_SENSITIVE_DATA=true; diagnostic responses may contain credentials")
	}
}
