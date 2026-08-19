// router defines all the routes and middlewares in this package
package router

import (
	"github.com/gorilla/mux"
	"github.com/jhoelzel/simpleapp/pkg/homecontroller"
	"github.com/jhoelzel/simpleapp/pkg/statuscontroller"
)

// Get returns a router with all routes and optional outer middlewares set up.
// Optional middlewares run before the access logger so request-scoped trace
// context is available for log correlation.
func Get(release string, middlewares ...mux.MiddlewareFunc) *mux.Router {
	r := mux.NewRouter()
	homecontroller.RegisterSubRouter(r)
	statuscontroller.RegisterSubRouter(r)
	for _, middleware := range middlewares {
		if middleware != nil {
			r.Use(middleware)
		}
	}
	r.Use(accessLogMiddleware(release))

	return r
}
