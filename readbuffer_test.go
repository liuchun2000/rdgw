package remotedialer

import (
	"bytes"
	"errors"
	"testing"
)

func TestReadBufferOfferRejectsOversizeMessage(t *testing.T) {
	r := newReadBuffer(1, nil)
	data := bytes.Repeat([]byte("x"), maxBufferLimit+1)

	err := r.Offer(bytes.NewReader(data))
	if !errors.Is(err, errReadBufferLimit) {
		t.Fatalf("Offer() error = %v, want %v", err, errReadBufferLimit)
	}
	if got, want := r.buf.Len(), maxBufferLimit; got != want {
		t.Fatalf("buffer length = %d, want %d", got, want)
	}
}
