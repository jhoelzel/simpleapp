//statuscontroller defines all handlerfucs in the /status/ directory
package statuscontroller

import (
	"net/http"
)

func readyzEndpoint(w http.ResponseWriter, r *http.Request) {

	w.WriteHeader(http.StatusOK)
	//http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}
