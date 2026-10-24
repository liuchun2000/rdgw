package remotedialer

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestConnectionSetWriteDeadlineConcurrentWrite(t *testing.T) {
	t.Parallel()

	s := setupDummySession(t, 0)
	s.conn = &fakeWSConn{
		writeMessageCallback: func(int, time.Time, []byte) error {
			return nil
		},
	}
	conn := newConnection(getDummyConnectionID(), s, "test", "test")

	const iterations = 1000
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			if _, err := conn.Write([]byte("test")); err != nil {
				t.Errorf("Write() error = %v", err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			if err := conn.Pause(); err != nil {
				t.Errorf("Pause() error = %v", err)
				return
			}
			if err := conn.Resume(); err != nil {
				t.Errorf("Resume() error = %v", err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			if err := conn.SetWriteDeadline(time.Time{}); err != nil {
				t.Errorf("SetWriteDeadline() error = %v", err)
				return
			}
		}
	}()

	close(start)
	wg.Wait()
}

func TestPauseControlWriteHasDeadlineAndDoesNotHoldReadBufferLock(t *testing.T) {
	s := setupDummySession(t, 0)
	conn := newConnection(getDummyConnectionID(), s, "test", "test")
	called := make(chan time.Time, 1)
	s.conn = &fakeWSConn{
		writeMessageCallback: func(_ int, deadline time.Time, _ []byte) error {
			called <- deadline
			_ = conn.buffer.Status()
			return errors.New("write failed")
		},
	}

	data := bytes.Repeat([]byte("x"), MaxBuffer+1)
	if err := conn.OnData(bytes.NewReader(data)); err == nil {
		t.Fatal("OnData() succeeded after Pause write failed")
	}
	select {
	case deadline := <-called:
		if deadline.IsZero() {
			t.Fatal("Pause write used an unbounded deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("Pause write did not complete; read buffer lock may be held")
	}
}

func TestConnectionWriteDeadlineDoesNotCloseSuccessfulWrite(t *testing.T) {
	s := setupDummySession(t, 0)
	s.conn = &fakeWSConn{
		writeMessageCallback: func(int, time.Time, []byte) error {
			return nil
		},
	}
	conn := newConnection(getDummyConnectionID(), s, "test", "test")
	s.addConnection(conn.connID, conn)

	if err := conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond)
	if err := conn.Err(); err != nil {
		t.Fatalf("successful write closed the connection: %v", err)
	}
	if got := s.getConnection(conn.connID); got != conn {
		t.Fatal("successful write removed the connection after its deadline")
	}
}
