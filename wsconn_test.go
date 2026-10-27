package rdgw

import (
	"errors"
	"io"
	"testing"
	"time"
)

type fakeWSConn struct {
	writeMessageCallback func(int, time.Time, []byte) error
}

func (f fakeWSConn) Close() error {
	return nil
}

func (f fakeWSConn) NextReader() (int, io.Reader, error) {
	return 0, nil, errors.New("not implemented")
}

func (f fakeWSConn) WriteMessage(messageType int, deadline time.Time, data []byte) error {
	if cb := f.writeMessageCallback; cb != nil {
		return cb(messageType, deadline, data)
	}
	return errors.New("callback not provided")
}

func (f fakeWSConn) WriteControl(int, time.Time, []byte) error {
	return errors.New("not implemented")
}

func TestWSWrapperWriteMessageTimeoutDoesNotLeaveWriterBlocked(t *testing.T) {
	conn := testServerWS(t, nil)
	wrapper := newWSConn(conn)

	<-wrapper.writeMu
	if err := wrapper.WriteMessage(1, time.Now().Add(20*time.Millisecond), []byte("blocked")); err == nil {
		t.Fatal("WriteMessage succeeded while the write lock was held")
	}
	wrapper.writeMu <- struct{}{}

	if err := wrapper.WriteMessage(1, time.Now().Add(time.Second), []byte("available")); err != nil {
		t.Fatalf("WriteMessage remained blocked after timeout: %v", err)
	}
}
