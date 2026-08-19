// statuscontroller defines all handlerfucs in the /status/ directory
package statuscontroller

import "github.com/gorilla/mux"

func RegisterSubRouter(r *mux.Router) {
	r.HandleFunc("/status/", infoEndpoint).Methods("GET")
	r.HandleFunc("/status/healthz", healthzEndpoint).Methods("GET")
	r.HandleFunc("/status/readyz", readyzEndpoint).Methods("GET")
}
