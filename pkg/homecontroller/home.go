//homecontroller defines all handlerfucs in the root directory
package homecontroller

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/jhoelzel/simpleapp/pkg/version"
)

//Home returns a simple HTTP handler function which writes a response.
func homeEndpoint(w http.ResponseWriter, r *http.Request) {
	info := struct {
		BuildTime string `json:"buildTime"`
		Commit    string `json:"commit"`
		Release   string `json:"release"`
	}{
		version.BuildTime, version.Commit, version.Release,
	}

	body, err := json.Marshal(info)
	if err != nil {
		log.Printf("Could not json.Marshal info data: %v", err)
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)

}
