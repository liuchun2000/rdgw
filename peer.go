package remotedialer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rancher/remotedialer/metrics"
	"github.com/sirupsen/logrus"
)

var (
	Token = "X-API-Tunnel-Token"
	ID    = "X-API-Tunnel-ID"
)

func (s *Server) AddPeer(url, id, token string) {
	if s.PeerID == "" || s.PeerToken == "" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	peer := peer{
		url:    url,
		id:     id,
		token:  token,
		cancel: cancel,
	}

	logrus.Infof("Adding peer %s, %s", url, id)

	s.peerLock.Lock()
	defer s.peerLock.Unlock()

	if p, ok := s.peers[id]; ok {
		if p.equals(peer) {
			return
		}
		p.cancel()
	}

	s.peerStarted = true
	s.peers[id] = peer
	go peer.start(ctx, s)
}

func (s *Server) RemovePeer(id string) {
	s.peerLock.Lock()
	defer s.peerLock.Unlock()

	if p, ok := s.peers[id]; ok {
		logrus.Infof("Removing peer %s", id)
		p.cancel()
	}
	delete(s.peers, id)
}

type peer struct {
	url, id, token string
	cancel         func()
}

func (p peer) equals(other peer) bool {
	return p.url == other.url &&
		p.id == other.id &&
		p.token == other.token
}

func (p *peer) start(ctx context.Context, s *Server) {
	tlsConfig, err := peerTLSConfig(s.peerTLS(), p.url)
	if err != nil {
		logrus.Errorf("Failed to connect to peer %s: malformed peer URL", p.url)
		return
	}
	headers := http.Header{
		ID:    {s.PeerID},
		Token: {s.PeerToken},
	}

	dialer := &websocket.Dialer{
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: HandshakeTimeOut,
	}
	ctx = context.WithValue(ctx, ContextKeyCaller, fmt.Sprintf("Peer url:%s, id:%s", p.url, p.id))

outer:
	for {
		select {
		case <-ctx.Done():
			break outer
		default:
		}

		metrics.IncSMTotalAddPeerAttempt(p.id)
		ws, _, err := dialer.Dial(p.url, headers)
		if err != nil {
			logrus.Errorf("Failed to connect to peer %s [local ID=%s]: %v", p.url, s.PeerID, err)
			time.Sleep(5 * time.Second)
			continue
		}
		metrics.IncSMTotalPeerConnected(p.id)

		session := NewClientSession(func(string, string) bool { return true }, ws)
		session.dialer = func(ctx context.Context, network, address string) (net.Conn, error) {
			parts := strings.SplitN(network, "::", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid clientKey/proto: %s", network)
			}
			d := s.Dialer(parts[0])
			return d(ctx, parts[1], address)
		}

		s.sessions.addListener(session)
		_, err = session.Serve(ctx)
		s.sessions.removeListener(session)
		session.Close()
		metrics.IncSMTotalPeerDisConnected(p.id)

		if err != nil {
			logrus.Errorf("Failed to serve peer connection %s: %v", p.id, err)
		}

		ws.Close()
		time.Sleep(5 * time.Second)
	}
}

func peerTLSConfig(cfg *tls.Config, rawURL string) (*tls.Config, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("invalid peer URL")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, fmt.Errorf("invalid peer URL port")
		}
	}
	if cfg == nil {
		cfg = &tls.Config{}
	}
	cfg = cfg.Clone()
	cfg.ServerName = u.Hostname()
	return cfg, nil
}
