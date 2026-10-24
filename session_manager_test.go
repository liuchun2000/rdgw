package remotedialer

import (
	"fmt"
	"testing"
	"time"
)

type testSessionListener struct {
	started chan string
	finish  chan struct{}
	events  chan string
	closed  chan struct{}
}

func (l *testSessionListener) sessionAdded(clientKey string, sessionKey int64) error {
	name := fmt.Sprintf("add:%s:%d", clientKey, sessionKey)
	l.started <- name
	if l.finish != nil {
		select {
		case <-l.finish:
		case <-l.closed:
		}
	}
	l.events <- name
	return nil
}

func (l *testSessionListener) sessionRemoved(clientKey string, sessionKey int64) error {
	name := fmt.Sprintf("remove:%s:%d", clientKey, sessionKey)
	l.events <- name
	return nil
}

func (l *testSessionListener) closeSession() {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
}

func TestSessionManagerNotificationsAreOrderedOutsideManagerLock(t *testing.T) {
	sm := newSessionManager()
	listener := &testSessionListener{
		started: make(chan string, 1),
		finish:  make(chan struct{}),
		events:  make(chan string, 2),
		closed:  make(chan struct{}),
	}
	queue := newSessionNotificationQueue(sm, listener)
	sm.Lock()
	sm.listeners[listener] = queue
	if !queue.enqueue(sessionEvent{clientKey: "foo", sessionKey: 1}) || !queue.enqueue(sessionEvent{clientKey: "foo", sessionKey: 1, removed: true}) {
		sm.Unlock()
		t.Fatal("failed to enqueue session notifications")
	}
	sm.Unlock()

	select {
	case got := <-listener.started:
		if got != "add:foo:1" {
			t.Fatalf("first notification = %q, want add", got)
		}
	case <-time.After(time.Second):
		t.Fatal("add notification did not start")
	}

	listed := make(chan struct{})
	go func() {
		sm.listClients()
		close(listed)
	}()
	select {
	case <-listed:
	case <-time.After(time.Second):
		t.Fatal("blocked notification held the session manager lock")
	}

	close(listener.finish)
	for _, want := range []string{"add:foo:1", "remove:foo:1"} {
		select {
		case got := <-listener.events:
			if got != want {
				t.Fatalf("notification = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q notification", want)
		}
	}
	sm.removeListener(listener)
}

func TestSessionManagerNotificationQueueOverflowClosesListener(t *testing.T) {
	sm := newSessionManager()
	listener := &testSessionListener{
		started: make(chan string, 1),
		finish:  make(chan struct{}),
		events:  make(chan string, 1),
		closed:  make(chan struct{}),
	}
	queue := newSessionNotificationQueue(sm, listener)
	sm.Lock()
	sm.listeners[listener] = queue
	if !queue.enqueue(sessionEvent{clientKey: "foo", sessionKey: -1}) {
		sm.Unlock()
		t.Fatal("failed to enqueue notification to block the worker")
	}
	sm.Unlock()
	select {
	case <-listener.started:
	case <-time.After(time.Second):
		t.Fatal("notification worker did not start")
	}

	sm.Lock()
	for i := 0; i < maxSessionNotifyQueue; i++ {
		if !queue.enqueue(sessionEvent{clientKey: "foo", sessionKey: int64(i)}) {
			sm.Unlock()
			t.Fatal("notification queue filled before its configured limit")
		}
	}
	if queue.enqueue(sessionEvent{clientKey: "foo", sessionKey: maxSessionNotifyQueue}) {
		sm.Unlock()
		t.Fatal("notification queue accepted an event after reaching its limit")
	}
	delete(sm.listeners, listener)
	sm.Unlock()
	sm.closeListeners([]*sessionNotificationQueue{queue})
	select {
	case <-listener.closed:
	case <-time.After(time.Second):
		t.Fatal("listener was not closed after notification queue overflow")
	}
}
