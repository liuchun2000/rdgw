package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rancher/remotedialer"
	"github.com/sirupsen/logrus"
)

var counter int64

type authStore struct {
	mu         sync.RWMutex
	snapshot   authSnapshot
	authFile   string
	originFile string
}

type authSnapshot struct {
	tokens  map[string][]string
	origins map[string]struct{}
}

func (a *authStore) reload() error {
	var raw map[string][]string
	if a.authFile != "" {
		data, err := os.ReadFile(a.authFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
	}
	next := make(map[string][]string, len(raw))
	for id, tokens := range raw {
		if id == "" || len(tokens) == 0 {
			return fmt.Errorf("invalid client auth entry")
		}
		for _, token := range tokens {
			if token == "" {
				return fmt.Errorf("empty client token")
			}
		}
		next[id] = append([]string(nil), tokens...)
	}
	origins := map[string]struct{}{}
	if a.originFile != "" {
		data, err := os.ReadFile(a.originFile)
		if err != nil {
			return err
		}
		var values []string
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, origin := range values {
			if origin != "" {
				origins[origin] = struct{}{}
			}
		}
	}
	a.mu.Lock()
	a.snapshot = authSnapshot{tokens: next, origins: origins}
	a.mu.Unlock()
	return nil
}

func (a *authStore) authorize(req *http.Request) (string, bool, error) {
	id, token := req.Header.Get("X-Tunnel-ID"), req.Header.Get("X-Tunnel-Token")
	a.mu.RLock()
	snapshot := a.snapshot
	origin := req.Header.Get("Origin")
	_, originOK := snapshot.origins[origin]
	ok := false
	for _, candidate := range snapshot.tokens[id] {
		if len(candidate) == len(token) && subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			ok = true
		}
	}
	a.mu.RUnlock()
	if origin != "" && !originOK {
		return id, false, nil
	}
	if a.authFile == "" {
		return id, id != "", nil
	}
	if id == "" || token == "" {
		return id, false, nil
	}
	return id, ok, nil
}

func normalizeTarget(scheme, rawHost string) (string, string, string, error) {
	if scheme != "http" && scheme != "https" && scheme != "ws" && scheme != "wss" {
		return "", "", "", fmt.Errorf("unsupported scheme")
	}
	if rawHost == "" || strings.ContainsAny(rawHost, "\r\n\x00") {
		return "", "", "", fmt.Errorf("invalid host")
	}
	host, port := rawHost, 80
	hostAuthority := rawHost
	hasExplicitPort := false
	if scheme == "https" || scheme == "wss" {
		port = 443
	}
	if h, p, err := net.SplitHostPort(rawHost); err == nil {
		host = h
		hasExplicitPort = true
		parsed, err := strconv.Atoi(p)
		if err != nil || parsed < 1 || parsed > 65535 {
			return "", "", "", fmt.Errorf("invalid port")
		}
		port = parsed
	} else if strings.Count(rawHost, ":") > 1 {
		host = strings.Trim(rawHost, "[]")
		if net.ParseIP(host) == nil {
			return "", "", "", fmt.Errorf("invalid host")
		}
	} else if strings.Contains(rawHost, ":") {
		return "", "", "", fmt.Errorf("invalid host authority")
	}
	if host == "" {
		return "", "", "", fmt.Errorf("invalid host")
	}
	if !hasExplicitPort {
		hostAuthority = host
	}
	return host, net.JoinHostPort(host, strconv.Itoa(port)), hostAuthority, nil
}

func countFlag(name string) int {
	count := 0
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-"+name || strings.HasPrefix(os.Args[i], "-"+name+"=") {
			count++
		}
	}
	return count
}

func loadRoots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no certificates found in CA file")
	}
	return roots, nil
}

func validateListenAddress(address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	parsed, err := strconv.Atoi(port)
	if err != nil || parsed < 1 || parsed > 65535 {
		return fmt.Errorf("invalid port")
	}
	return nil
}

func authorizer(req *http.Request) (string, bool, error) {
	id := req.Header.Get("x-tunnel-id")
	return id, id != "", nil
}

// proxyHTTP 处理普通 HTTP 请求（支持任意方法）
type proxyConfig struct {
	verify bool
	roots  *x509.CertPool
}

func proxyHTTP(server *remotedialer.Server, rw http.ResponseWriter, req *http.Request, cfg proxyConfig) {
	id := atomic.AddInt64(&counter, 1)

	clientKey := req.PathValue("id")
	scheme := req.PathValue("scheme")
	host, targetAddr, hostAuthority, err := normalizeTarget(scheme, req.PathValue("host"))
	if err != nil {
		http.Error(rw, "invalid target", http.StatusBadRequest)
		return
	}
	path := req.PathValue("path")

	targetURL := fmt.Sprintf("%s://%s", scheme, host)
	if path != "" {
		targetURL += "/" + path
	}

	dialer := server.Dialer(clientKey)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	conn, err := dialer(ctx, "tcp", targetAddr)
	if err != nil {
		logrus.Errorf("[%03d] DIAL ERR %s: %v", id, targetAddr, err)
		http.Error(rw, "upstream unavailable", http.StatusBadGateway)
		return
	}

	// 新增：根据 scheme 决定是否进行 TLS 握手
	if scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: host, RootCAs: cfg.roots,
			InsecureSkipVerify: !cfg.verify, MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			logrus.Errorf("[%03d] TLS HANDSHAKE ERR %s: %v", id, targetAddr, err)
			conn.Close()
			http.Error(rw, "upstream TLS handshake failed", http.StatusBadGateway)
			return
		}
		conn = tlsConn // 后续 Write/Read 都走 TLS 连接
	}
	defer conn.Close()

	// 构造发往目标服务的请求：路径、Host 都要改成目标服务认识的形式
	outReq := req.Clone(req.Context())
	outReq.URL.Scheme = ""
	outReq.URL.Host = ""
	outReq.RequestURI = ""

	// 关键修复：去掉 /client/{id}/{scheme}/{host} 前缀，只保留目标路径
	if path == "" {
		outReq.URL.Path = "/"
	} else {
		outReq.URL.Path = "/" + path
	}
	outReq.URL.RawPath = ""

	// Host 头改成目标服务的 Host
	outReq.Host = hostAuthority

	removeHopByHopHeaders(outReq.Header)

	if err := outReq.Write(conn); err != nil {
		logrus.Errorf("[%03d] WRITE ERR %s: %v", id, targetURL, err)
		http.Error(rw, "upstream write failed", http.StatusBadGateway)
		return
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), outReq)
	if err != nil {
		logrus.Errorf("[%03d] READ ERR %s: %v", id, targetURL, err)
		http.Error(rw, "upstream response failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, h := range v {
			rw.Header().Add(k, h)
		}
	}
	rw.WriteHeader(resp.StatusCode)

	n, _ := io.Copy(rw, resp.Body)
	logrus.Infof("[%03d] %s %s://%s -> %d (%d bytes)", id, req.Method, scheme, host, resp.StatusCode, n)
}

// proxyWebSocket 处理 WebSocket 升级请求
func proxyWebSocket(server *remotedialer.Server, rw http.ResponseWriter, req *http.Request, cfg proxyConfig) {
	id := atomic.AddInt64(&counter, 1)

	clientKey := req.PathValue("id")
	scheme := req.PathValue("scheme")
	host, targetAddr, hostAuthority, err := normalizeTarget(scheme, req.PathValue("host"))
	if err != nil {
		http.Error(rw, "invalid target", http.StatusBadRequest)
		return
	}
	path := req.PathValue("path")

	dialer := server.Dialer(clientKey)
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	conn, err := dialer(ctx, "tcp", targetAddr)
	if err != nil {
		logrus.Errorf("[%03d] WS DIAL ERR %s: %v", id, targetAddr, err)
		http.Error(rw, "upstream unavailable", http.StatusBadGateway)
		return
	}

	if scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         host,
			RootCAs:            cfg.roots,
			InsecureSkipVerify: !cfg.verify,
			MinVersion:         tls.VersionTLS12,
		})

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			logrus.Errorf("[%03d] WS TLS HANDSHAKE ERR %s: %v", id, targetAddr, err)
			conn.Close()
			http.Error(rw, "upstream TLS handshake failed", http.StatusBadGateway)
			return
		}

		conn = tlsConn
	}

	defer conn.Close()

	// 构造发往后端的 WebSocket 握手请求
	outReq := req.Clone(req.Context())
	outReq.URL.Scheme = ""
	outReq.URL.Host = ""
	outReq.RequestURI = ""

	if path == "" {
		outReq.URL.Path = "/"
	} else {
		outReq.URL.Path = "/" + path
	}
	outReq.URL.RawPath = ""
	outReq.Host = hostAuthority

	// 注意：WebSocket 握手必须保留 Upgrade / Connection 头，所以不能调用 removeHopByHopHeaders
	// 这里只移除代理相关的头
	outReq.Header.Del("Proxy-Authenticate")
	outReq.Header.Del("Proxy-Authorization")

	if err := outReq.Write(conn); err != nil {
		logrus.Errorf("[%03d] WS WRITE ERR: %v", id, err)
		http.Error(rw, "upstream write failed", http.StatusBadGateway)
		return
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, outReq)
	if err != nil {
		logrus.Errorf("[%03d] WS READ ERR: %v", id, err)
		http.Error(rw, "upstream response failed", http.StatusBadGateway)
		return
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		for k, v := range resp.Header {
			if isHopByHopHeader(k) {
				continue
			}
			for _, h := range v {
				rw.Header().Add(k, h)
			}
		}
		rw.WriteHeader(resp.StatusCode)
		io.Copy(rw, resp.Body)
		resp.Body.Close()
		return
	}

	hijacker, ok := rw.(http.Hijacker)
	if !ok {
		http.Error(rw, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		logrus.Errorf("[%03d] WS HIJACK ERR: %v", id, err)
		return
	}
	defer clientConn.Close()

	if err := resp.Write(clientConn); err != nil {
		logrus.Errorf("[%03d] WS WRITE 101 ERR: %v", id, err)
		return
	}

	if br.Buffered() > 0 {
		buffered, _ := br.Peek(br.Buffered())
		clientConn.Write(buffered)
		br.Discard(len(buffered))
	}
	if clientBuf.Reader.Buffered() > 0 {
		buffered, _ := clientBuf.Reader.Peek(clientBuf.Reader.Buffered())
		conn.Write(buffered)
		clientBuf.Reader.Discard(len(buffered))
	}

	logrus.Infof("[%03d] WS UPGRADED %s/%s/%s", id, scheme, host, path)

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(conn, clientConn)
		conn.Close()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, conn)
		clientConn.Close()
		done <- struct{}{}
	}()
	<-done
	logrus.Infof("[%03d] WS CLOSED", id)
}

func isWebSocketRequest(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// removeHopByHopHeaders 移除普通 HTTP 转发中不应传递的 hop-by-hop header
func removeHopByHopHeaders(h http.Header) {
	for _, k := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(k)
	}
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func dispatch(server *remotedialer.Server, rw http.ResponseWriter, req *http.Request, cfg proxyConfig) {
	if isWebSocketRequest(req) {
		if req.Method != http.MethodGet {
			rw.Header().Set("Allow", http.MethodGet)
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		proxyWebSocket(server, rw, req, cfg)
		return
	}
	proxyHTTP(server, rw, req, cfg)
}

func main() {
	var (
		addr           string
		connectAddr    string
		proxyAddr      string
		mode           string
		rawAddr        string
		peerID         string
		peerToken      string
		peers          string
		clientAuthFile string
		originFile     string
		caFile         string
		verifyUpstream bool
		debug          bool
	)
	flag.StringVar(&addr, "listen", ":8123", "Listen address")
	flag.StringVar(&connectAddr, "connect-listen", "", "Split mode /connect listen address")
	flag.StringVar(&proxyAddr, "proxy-listen", "", "Split mode /client listen address")
	flag.StringVar(&mode, "listener-mode", "single", "Listener mode: single or split")
	flag.StringVar(&rawAddr, "socks5-listen", ":2222", "SOCKS5 listen address for SSH/any TCP")
	flag.StringVar(&peerID, "id", "", "Peer ID")
	flag.StringVar(&peerToken, "token", "", "Peer Token")
	flag.StringVar(&peers, "peers", "", "Peers format id:token:url,id:token:url")
	flag.StringVar(&clientAuthFile, "client-auth-file", "", "JSON client token file")
	flag.StringVar(&originFile, "origin-allowlist-file", "", "JSON Origin allowlist file")
	flag.StringVar(&caFile, "upstream-ca-file", "", "CA bundle for upstream HTTPS/WSS")
	flag.BoolVar(&verifyUpstream, "upstream-tls-verify", false, "Verify upstream HTTPS/WSS certificates")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	for _, name := range []string{"listen", "connect-listen", "proxy-listen", "listener-mode"} {
		if countFlag(name) > 1 {
			logrus.Fatalf("duplicate -%s", name)
		}
	}
	flag.Parse()
	if mode != "single" && mode != "split" {
		logrus.Fatalf("invalid listener mode %q", mode)
	}
	if mode == "split" && (countFlag("listen") > 0 || connectAddr == "" || proxyAddr == "") {
		logrus.Fatal("split mode requires connect-listen and proxy-listen and forbids listen")
	}
	if mode == "single" && (connectAddr != "" || proxyAddr != "") {
		logrus.Fatal("connect-listen and proxy-listen require split mode")
	}
	if mode == "split" && connectAddr == proxyAddr {
		logrus.Fatal("listener addresses must differ")
	}
	addressesToValidate := []string{rawAddr}
	if mode == "single" {
		addressesToValidate = append(addressesToValidate, addr)
	} else {
		addressesToValidate = append(addressesToValidate, connectAddr, proxyAddr)
	}
	for _, address := range addressesToValidate {
		if address != "" && validateListenAddress(address) != nil {
			logrus.Fatalf("invalid listener address %q", address)
		}
	}

	if debug {
		logrus.SetLevel(logrus.DebugLevel)
		remotedialer.PrintTunnelData = true
	}

	auth := &authStore{authFile: clientAuthFile, originFile: originFile}
	if clientAuthFile != "" || originFile != "" {
		if err := auth.reload(); err != nil {
			logrus.Fatalf("load client auth: %v", err)
		}
	}
	var roots *x509.CertPool
	if caFile != "" {
		var err error
		roots, err = loadRoots(caFile)
		if err != nil {
			logrus.Fatalf("load upstream CA: %v", err)
		}
	}
	proxyCfg := proxyConfig{verify: verifyUpstream, roots: roots}
	handler := remotedialer.New(auth.authorize, remotedialer.DefaultErrorWriter)
	handler.PeerToken = peerToken
	handler.PeerID = peerID

	for _, peer := range strings.Split(peers, ",") {
		parts := strings.SplitN(strings.TrimSpace(peer), ":", 3)
		if len(parts) != 3 {
			continue
		}
		handler.AddPeer(parts[2], parts[0], parts[1])
	}

	// 启动 SOCKS5 监听（和 HTTP 监听并行，共用同一个 handler）
	var socks *socks5Listener
	if rawAddr != "" {
		var err error
		socks, err = startSocks5Listener(handler, rawAddr)
		if err != nil {
			logrus.Fatalf("start socks5 listener failed: %v", err)
		}
	}

	connectRouter := http.NewServeMux()
	connectRouter.HandleFunc("/connect", func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || !isWebSocketRequest(req) {
			rw.Header().Set("Allow", http.MethodGet)
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handler.ServeHTTP(rw, req)
	})
	proxyRouter := http.NewServeMux()
	clientHandler := func(rw http.ResponseWriter, req *http.Request) { dispatch(handler, rw, req, proxyCfg) }
	proxyRouter.HandleFunc("/client/{id}/{scheme}/{host}", clientHandler)
	proxyRouter.HandleFunc("/client/{id}/{scheme}/{host}/{path...}", clientHandler)
	if mode == "single" {
		connectRouter.HandleFunc("/client/{id}/{scheme}/{host}", clientHandler)
		connectRouter.HandleFunc("/client/{id}/{scheme}/{host}/{path...}", clientHandler)
	}
	addresses := []string{addr}
	routers := []*http.ServeMux{connectRouter}
	if mode == "split" {
		addresses = []string{connectAddr, proxyAddr}
		routers = []*http.ServeMux{connectRouter, proxyRouter}
	}
	servers := make([]*http.Server, len(addresses))
	listeners := make([]net.Listener, len(addresses))
	for i, address := range addresses {
		ln, err := net.Listen("tcp", address)
		if err != nil {
			for _, opened := range listeners {
				if opened != nil {
					_ = opened.Close()
				}
			}
			logrus.Fatalf("listen %s: %v", address, err)
		}
		listeners[i] = ln
		servers[i] = &http.Server{Handler: routers[i], ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	}
	serveErrors := make(chan error, len(servers))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if clientAuthFile != "" || originFile != "" {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					if err := auth.reload(); err != nil {
						logrus.Errorf("reload auth files: %v", err)
					}
				}
			}
		}()
	}
	for i := range servers {
		go func(i int) {
			if err := servers[i].Serve(listeners[i]); err != nil && err != http.ErrServerClosed {
				serveErrors <- err
			}
		}(i)
	}
	if socks != nil {
		go func() {
			if err := <-socks.Errors(); err != nil {
				serveErrors <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
	case err := <-serveErrors:
		logrus.Errorf("HTTP listener failed: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdownCtx)
	}
	if socks != nil {
		_ = socks.Close()
	}
}
