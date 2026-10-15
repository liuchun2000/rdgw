package remotedialer

import (
	"crypto/tls"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	errFailedAuth       = errors.New("failed authentication")
	errWrongMessageType = errors.New("wrong websocket message type")
)

type Authorizer func(req *http.Request) (clientKey string, authed bool, err error)
type ErrorWriter func(rw http.ResponseWriter, req *http.Request, code int, err error)

func DefaultErrorWriter(rw http.ResponseWriter, req *http.Request, code int, err error) {
	rw.WriteHeader(code)
	rw.Write([]byte(err.Error()))
}

type Server struct {
	PeerID                  string
	PeerToken               string
	ClientConnectAuthorizer ConnectAuthorizer
	authorizer              Authorizer
	errorWriter             ErrorWriter
	sessions                *sessionManager
	peers                   map[string]peer
	peerLock                sync.Mutex
	peerTLSConfig           *tls.Config
	peerStarted             bool
}

// SetPeerTLSConfig configures TLS used for outbound peer WebSocket connections.
// The configuration is copied before use and may be set before AddPeer.
func (s *Server) SetPeerTLSConfig(cfg *tls.Config) error {
	if cfg == nil {
		return errors.New("peer TLS config must not be nil")
	}
	if cfg.InsecureSkipVerify {
		return errors.New("peer TLS config must verify certificates")
	}
	if cfg.MinVersion != 0 && cfg.MinVersion < tls.VersionTLS12 {
		return errors.New("peer TLS config requires TLS 1.2 or newer")
	}
	if cfg.MaxVersion != 0 && cfg.MaxVersion < tls.VersionTLS12 {
		return errors.New("peer TLS config requires TLS 1.2 or newer")
	}
	s.peerLock.Lock()
	defer s.peerLock.Unlock()
	if s.peerStarted {
		return errors.New("peer TLS config must be set before AddPeer")
	}
	s.peerTLSConfig = cfg.Clone()
	return nil
}

func (s *Server) peerTLS() *tls.Config {
	s.peerLock.Lock()
	defer s.peerLock.Unlock()
	if s.peerTLSConfig == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	cfg := s.peerTLSConfig.Clone()
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}

func New(auth Authorizer, errorWriter ErrorWriter) *Server {
	return &Server{
		peers:       map[string]peer{},
		authorizer:  auth,
		errorWriter: errorWriter,
		sessions:    newSessionManager(),
	}
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	clientKey, authed, peer, err := s.auth(req)
	if err != nil {
		s.errorWriter(rw, req, 400, err)
		return
	}
	if !authed {
		s.errorWriter(rw, req, 401, errFailedAuth)
		return
	}

	logrus.Infof("Handling backend connection request [%s]", clientKey)

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		CheckOrigin:      func(r *http.Request) bool { return true },
		Error:            s.errorWriter,
	}

	wsConn, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		s.errorWriter(rw, req, 400, errors.Wrapf(err, "Error during upgrade for host [%v]", clientKey))
		return
	}

	session := s.sessions.add(clientKey, wsConn, peer)
	session.auth = s.ClientConnectAuthorizer
	defer s.sessions.remove(session)

	code, err := session.Serve(req.Context())
	if err != nil {
		// Hijacked so we can't write to the client
		logrus.Infof("error in remotedialer server [%d]: %v", code, err)
	}
}

func (s *Server) ListClients() []string {
	return s.sessions.listClients()
}

func (s *Server) auth(req *http.Request) (clientKey string, authed, peer bool, err error) {
	id := req.Header.Get(ID)
	token := req.Header.Get(Token)
	if id != "" && token != "" {
		// peer authentication
		s.peerLock.Lock()
		p, ok := s.peers[id]
		s.peerLock.Unlock()

		if ok && p.token == token {
			return id, true, true, nil
		}
	}

	id, authed, err = s.authorizer(req)
	return id, authed, false, err
}
