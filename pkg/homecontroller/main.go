//homecontroller defines all handlerfucs in the root directory
package homecontroller

import (
	"github.com/gorilla/mux"
)

func RegisterSubRouter(r *mux.Router) {
	r.HandleFunc("/", homeEndpoint).Methods("GET")
}
