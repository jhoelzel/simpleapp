//statuscontroller defines all handlerfucs in the /status/ directory
package statuscontroller

import "github.com/gorilla/mux"

func RegisterSubRouter(r *mux.Router) {
	statusRouter := r.PathPrefix("/status").Subrouter()
	statusRouter.HandleFunc("/info", infoEndpoint).Methods("GET")
	statusRouter.HandleFunc("/healthz", healthzEndpoint).Methods("GET")
	statusRouter.HandleFunc("/readyz", readyzEndpoint).Methods("GET")
}
