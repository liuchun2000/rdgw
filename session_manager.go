package remotedialer

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/rancher/remotedialer/metrics"
	"github.com/sirupsen/logrus"
)

type sessionListener interface {
	sessionAdded(clientKey string, sessionKey int64) error
	sessionRemoved(clientKey string, sessionKey int64) error
	closeSession()
}

type sessionEvent struct {
	clientKey  string
	sessionKey int64
	removed    bool
}

type sessionManager struct {
	sync.Mutex
	clients   map[string][]*Session
	peers     map[string][]*Session
	listeners map[sessionListener]*sessionNotificationQueue
}

const maxSessionNotifyQueue = 1024

type sessionNotificationQueue struct {
	manager  *sessionManager
	listener sessionListener
	events   chan sessionEvent
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

// 每个 listener 独立排队，避免慢 peer 阻塞其他 peer；队列满时丢弃该 peer 并让它重连重建状态。
func newSessionNotificationQueue(manager *sessionManager, listener sessionListener) *sessionNotificationQueue {
	queue := &sessionNotificationQueue{
		manager:  manager,
		listener: listener,
		events:   make(chan sessionEvent, maxSessionNotifyQueue),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go queue.run()
	return queue
}

func (q *sessionNotificationQueue) enqueue(event sessionEvent) bool {
	select {
	case <-q.stop:
		return false
	default:
	}
	select {
	case q.events <- event:
		return true
	default:
		return false
	}
}

func (q *sessionNotificationQueue) stopAndWait() {
	q.once.Do(func() { close(q.stop) })
	<-q.done
}

func (q *sessionNotificationQueue) run() {
	defer close(q.done)
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		select {
		case <-q.stop:
			return
		case event := <-q.events:
			var err error
			if event.removed {
				err = q.listener.sessionRemoved(event.clientKey, event.sessionKey)
			} else {
				err = q.listener.sessionAdded(event.clientKey, event.sessionKey)
			}
			if err != nil {
				logrus.WithError(err).Warn("session listener notification failed")
				q.listener.closeSession()
				q.manager.listenerFailed(q.listener, q)
				return
			}
		}
	}
}

func newSessionManager() *sessionManager {
	return &sessionManager{
		clients:   map[string][]*Session{},
		peers:     map[string][]*Session{},
		listeners: map[sessionListener]*sessionNotificationQueue{},
	}
}

func toDialer(s *Session, prefix string) Dialer {
	return func(ctx context.Context, proto, address string) (net.Conn, error) {
		if prefix == "" {
			return s.serverConnectContext(ctx, proto, address)
		}
		return s.serverConnectContext(ctx, prefix+"::"+proto, address)
	}
}

func (sm *sessionManager) removeListener(listener sessionListener) {
	sm.Lock()
	queue := sm.listeners[listener]
	delete(sm.listeners, listener)
	sm.Unlock()
	if queue != nil {
		queue.stopAndWait()
	}
}

func (sm *sessionManager) addListener(listener sessionListener) {
	sm.Lock()
	queue := newSessionNotificationQueue(sm, listener)
	sm.listeners[listener] = queue
	overflow := false

	for k, sessions := range sm.clients {
		for _, session := range sessions {
			overflow = !queue.enqueue(sessionEvent{clientKey: k, sessionKey: session.sessionKey})
			if overflow {
				break
			}
		}
		if overflow {
			break
		}
	}

	if !overflow {
		for k, sessions := range sm.peers {
			for _, session := range sessions {
				overflow = !queue.enqueue(sessionEvent{clientKey: k, sessionKey: session.sessionKey})
				if overflow {
					break
				}
			}
			if overflow {
				break
			}
		}
	}
	if overflow {
		delete(sm.listeners, listener)
	}
	sm.Unlock()
	if overflow {
		listener.closeSession()
		queue.stopAndWait()
	}
}

func (sm *sessionManager) listClients() []string {
	sm.Lock()
	defer sm.Unlock()
	clients := make([]string, 0, len(sm.clients))
	for c := range sm.clients {
		clients = append(clients, c)
	}
	return clients
}

func (sm *sessionManager) getDialer(clientKey string) (Dialer, error) {
	sm.Lock()
	localSessions := append([]*Session(nil), sm.clients[clientKey]...)
	if len(localSessions) > 0 {
		sm.Unlock()
		return fallbackDialer(localSessions, ""), nil
	}

	var peerSessions []*Session
	for _, sessions := range sm.peers {
		for _, session := range sessions {
			if session.hasSessionKey(clientKey) {
				peerSessions = append(peerSessions, session)
			}
		}
	}
	sm.Unlock()
	if len(peerSessions) > 0 {
		return fallbackDialer(peerSessions, clientKey), nil
	}

	return nil, fmt.Errorf("failed to find Session for client %s", clientKey)
}

func fallbackDialer(sessions []*Session, prefix string) Dialer {
	return func(ctx context.Context, proto, address string) (net.Conn, error) {
		var lastErr error
		for _, session := range sessions {
			conn, err := toDialer(session, prefix)(ctx, proto, address)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		if lastErr == nil {
			return nil, fmt.Errorf("no session available")
		}
		return nil, lastErr
	}
}

func (sm *sessionManager) add(clientKey string, conn *websocket.Conn, peer bool) *Session {
	sessionKey := rand.Int63()
	session := newSession(sessionKey, clientKey, newWSConn(conn))

	sm.Lock()

	if peer {
		sm.peers[clientKey] = append(sm.peers[clientKey], session)
	} else {
		sm.clients[clientKey] = append(sm.clients[clientKey], session)
	}
	metrics.IncSMTotalAddWS(clientKey, peer)

	var overflow []*sessionNotificationQueue
	for listener, queue := range sm.listeners {
		if !queue.enqueue(sessionEvent{clientKey: clientKey, sessionKey: session.sessionKey}) {
			delete(sm.listeners, listener)
			overflow = append(overflow, queue)
		}
	}
	sm.Unlock()
	sm.closeListeners(overflow)

	return session
}

func (sm *sessionManager) remove(s *Session) {
	var isPeer bool
	sm.Lock()

	for i, store := range []map[string][]*Session{sm.clients, sm.peers} {
		var newSessions []*Session

		for _, v := range store[s.clientKey] {
			if v.sessionKey == s.sessionKey {
				if i == 0 {
					isPeer = false
				} else {
					isPeer = true
				}
				metrics.IncSMTotalRemoveWS(s.clientKey, isPeer)
				continue
			}
			newSessions = append(newSessions, v)
		}

		if len(newSessions) == 0 {
			delete(store, s.clientKey)
		} else {
			store[s.clientKey] = newSessions
		}
	}

	var overflow []*sessionNotificationQueue
	for listener, queue := range sm.listeners {
		if !queue.enqueue(sessionEvent{clientKey: s.clientKey, sessionKey: s.sessionKey, removed: true}) {
			delete(sm.listeners, listener)
			overflow = append(overflow, queue)
		}
	}
	sm.Unlock()
	sm.closeListeners(overflow)

	s.Close()
}

func (sm *sessionManager) closeListeners(queues []*sessionNotificationQueue) {
	for _, queue := range queues {
		queue.listener.closeSession()
		queue.stopAndWait()
	}
}

func (sm *sessionManager) listenerFailed(listener sessionListener, queue *sessionNotificationQueue) {
	sm.Lock()
	defer sm.Unlock()
	if sm.listeners[listener] == queue {
		delete(sm.listeners, listener)
	}
}
