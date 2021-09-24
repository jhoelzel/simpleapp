// router defines all the routes and middlewares in this package
package router

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
)

func logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x, err := httputil.DumpRequest(r, true)
		if err != nil {
			http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
			return
		}
		log.Println(fmt.Sprintf("%q", x))
		h.ServeHTTP(w, r)
	})
}
