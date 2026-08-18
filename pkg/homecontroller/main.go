// Package homecontroller serves the interactive request and network
// diagnostics dashboard.
package homecontroller

import "github.com/gorilla/mux"

// RegisterSubRouter registers interactive and machine-readable diagnostics.
func RegisterSubRouter(r *mux.Router) {
	r.HandleFunc("/api/request", requestDetailsEndpoint)
	r.HandleFunc("/api/port-check", PortCheckEndpoint)
	r.HandleFunc("/", homeEndpoint)
}
