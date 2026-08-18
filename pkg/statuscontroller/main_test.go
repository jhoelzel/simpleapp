package statuscontroller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestRegisterSubRouter(t *testing.T) {
	router := mux.NewRouter()
	RegisterSubRouter(router)

	for _, path := range []string{"/status/", "/status/healthz", "/status/readyz"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d, want %d", path, recorder.Code, http.StatusOK)
			}
		})
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/status/healthz", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST healthz returned %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}
