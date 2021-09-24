//statuscontroller defines all handlerfucs in the /status/ directory
package statuscontroller

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jhoelzel/simpleapp/pkg/version"
)

func infoEndpoint(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	hostname, err := os.Hostname()
	if err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("Welcome to our test image!\n"))
	w.Write([]byte("------------------------------------------------\n"))
	w.Write([]byte("Running on container: " + hostname + "\n"))
	w.Write([]byte("The time is: " + time.Now().String() + "\n"))
	w.Write([]byte("The BuildTime is: " + version.BuildTime + "\n"))
	w.Write([]byte("The current Commit is: " + version.Commit + "\n"))
	w.Write([]byte("The current Release is: " + version.Release + "\n"))
	w.Write([]byte("------------------------------------------------\n"))

}
