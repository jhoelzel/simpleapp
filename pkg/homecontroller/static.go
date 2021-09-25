//homecontroller defines all handlerfucs in the root directory
package homecontroller

import (
	"net/http"

	"github.com/gorilla/mux"
)

//serveStatic is responsible for serving static files
func serveStatic(router *mux.Router, staticDirectory string) {
	staticPaths := getStaticPaths(staticDirectory)
	for pathName, pathValue := range staticPaths {
		pathPrefix := "/" + pathName + "/"
		router.PathPrefix(pathPrefix).Handler(http.StripPrefix(pathPrefix,
			http.FileServer(http.Dir(pathValue))))
	}
}
//getStaticPaths is responsible for defining static paths
func getStaticPaths(staticDirectory string) map[string]string {
	staticPaths := map[string]string{
		"static": staticDirectory,
	}
	return staticPaths
}
