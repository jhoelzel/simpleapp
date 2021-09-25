//statuscontroller defines all handlerfucs in the /status/ directory
package statuscontroller

import (
	"testing"

	"github.com/gorilla/mux"
)

func TestRegisterSubRouter(t *testing.T) {
	type args struct {
		r *mux.Router
	}
	tests := []struct {
		name string
		args args
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			RegisterSubRouter(tt.args.r)
		})
	}
}
