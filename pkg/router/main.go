// router defines all the routes and middlewares in this package
package router

import (
	"github.com/gorilla/mux"
	"github.com/jhoelzel/simpleapp/pkg/homecontroller"
	"github.com/jhoelzel/simpleapp/pkg/statuscontroller"
)

// Get Returns a router with all neccessairy  routes set up
func Get(buildTime, commit, release string) *mux.Router {
	r := mux.NewRouter()
	homecontroller.RegisterSubRouter(r)
	statuscontroller.RegisterSubRouter(r)

	return r
}
