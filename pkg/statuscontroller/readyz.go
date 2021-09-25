//statuscontroller defines all handlerfucs in the /status/ directory
package statuscontroller

import (
	"io"
	"net/http"
)

func readyzEndpoint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `{"alive": true}`)
	//http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}
