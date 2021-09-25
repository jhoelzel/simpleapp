//statuscontroller defines all handlerfucs in the /status/ directory
package statuscontroller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_infoEndpoint(t *testing.T) {
	// Create a request to pass to our handler. We don't have any query parameters for now, so we'll
	// pass 'nil' as the third parameter.
	req, err := http.NewRequest("GET", "/status/", nil)
	if err != nil {
		t.Fatal(err)
	}

	// We create a ResponseRecorder (which satisfies http.ResponseWriter) to record the response.
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(infoEndpoint)

	// Our handlers satisfy http.Handler, so we can call their ServeHTTP method
	// directly and pass in our Request and ResponseRecorder.
	handler.ServeHTTP(rr, req)

	// Check the status code is what we expect.
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}
	//define what we are looking for in the body response
	tests := []string{"Running on container", "The time is:", "The current Commit is", "The current Release is: "}
	sBody := rr.Body.String()
	for _, tt := range tests {
		if !strings.Contains(sBody, tt) {
			t.Errorf("handler did not return what we were looking for: got %v want %v",
				rr.Body.String(), tt)
		}
	}

}
