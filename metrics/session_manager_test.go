package metrics

import "testing"

func TestRegisterIsIdempotent(t *testing.T) {
	Register()
	Register()
}
