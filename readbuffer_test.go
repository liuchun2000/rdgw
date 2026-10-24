package remotedialer

import (
	"bytes"
	"errors"
	"testing"
	"time"
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

func TestReadBufferSetDeadlineWakesBlockedRead(t *testing.T) {
	r := newReadBuffer(1, nil)
	result := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 1))
		result <- err
	}()

	deadline := time.Now().Add(time.Second)
	r.SetDeadline(deadline)
	select {
	case err := <-result:
		if err == nil || err.Error() != "deadline exceeded" {
			t.Fatalf("Read() error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read() did not wake at the read deadline")
	}
}
