package main

import (
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
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rancher/remotedialer"
	"github.com/rancher/remotedialer/metrics"
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
	if strings.ContainsAny(host, "/\\@?#%\x00\r\n\t ") {
		return "", "", "", fmt.Errorf("invalid host")
	}
	if !hasExplicitPort {
		if strings.Contains(host, ":") {
			// IPv6 裸地址缺端口时 Host authority 必须加方括号
			hostAuthority = "[" + host + "]"
		}
	}
	return host, net.JoinHostPort(host, strconv.Itoa(port)), hostAuthority, nil
}

func countFlag(name string) int {
	count := 0
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--" {
			break
		}
		if arg == "-"+name || arg == "--"+name || strings.HasPrefix(arg, "-"+name+"=") || strings.HasPrefix(arg, "--"+name+"=") {
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

// proxyConfig controls the example proxy's resource and upstream TLS policy.
type proxyConfig struct {
	verify                bool
	roots                 *x509.CertPool
	idleTimeout           time.Duration
	responseHeaderTimeout time.Duration
	httpLimit             chan struct{}
	socksLimit            chan struct{}
	transports            *transportPool
}

type transportPool struct {
	mu         sync.Mutex
	transports map[string]*http.Transport
}

func (p *transportPool) get(clientKey string, server *remotedialer.Server, cfg proxyConfig) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.transports == nil {
		p.transports = make(map[string]*http.Transport)
	}
	if transport := p.transports[clientKey]; transport != nil {
		return transport
	}
	dialer := server.Dialer(clientKey)
	transport := newTransport(dialer, cfg)
	p.transports[clientKey] = transport
	return transport
}

func (p *transportPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, transport := range p.transports {
		transport.CloseIdleConnections()
	}
}

func newTransport(dialer remotedialer.Dialer, cfg proxyConfig) *http.Transport {
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: cfg.responseHeaderTimeout,
		TLSClientConfig: &tls.Config{
			RootCAs:            cfg.roots,
			InsecureSkipVerify: !cfg.verify,
			MinVersion:         tls.VersionTLS12,
		},
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer(ctx, network, address)
	}
	return transport
}

type idleTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t idleTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		return resp, err
	}
	resp.Body = &idleTimeoutBody{ReadCloser: resp.Body, timeout: t.timeout}
	return resp, nil
}

type idleTimeoutBody struct {
	io.ReadCloser
	timeout  time.Duration
	timedOut atomic.Bool
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	if b.timeout <= 0 {
		return b.ReadCloser.Read(p)
	}
	timer := time.AfterFunc(b.timeout, func() {
		b.timedOut.Store(true)
		_ = b.ReadCloser.Close()
	})
	n, err := b.ReadCloser.Read(p)
	timer.Stop()
	if b.timedOut.Load() && n == 0 {
		return 0, fmt.Errorf("upstream response idle timeout after %s", b.timeout)
	}
	return n, err
}

func targetPath(req *http.Request) (string, error) {
	escaped := req.URL.EscapedPath()
	parts := strings.Split(escaped, "/")
	if len(parts) < 5 || parts[1] != "client" {
		return "", fmt.Errorf("invalid proxy path")
	}
	// 保留原始编码：ReverseProxy 的 Rewrite 里直接写回 RawPath，
	// %2F、%3F 这类编码语义不会在转发过程中被二次解码或丢失
	if len(parts) == 5 {
		return "/", nil
	}
	return "/" + strings.Join(parts[5:], "/"), nil
}

func newProxy(server *remotedialer.Server, req *http.Request, cfg proxyConfig) (*httputil.ReverseProxy, error) {
	clientKey := req.PathValue("id")
	scheme := req.PathValue("scheme")
	_, _, hostAuthority, err := normalizeTarget(scheme, req.PathValue("host"))
	if err != nil {
		return nil, err
	}
	pathEscaped, err := targetPath(req)
	if err != nil {
		return nil, err
	}
	// Path 存解码值、RawPath 存原始转义值，保证标准库按 RawPath 原样转发
	path, err := url.PathUnescape(pathEscaped)
	if err != nil {
		return nil, err
	}
	targetScheme := scheme
	if scheme == "ws" {
		targetScheme = "http"
	} else if scheme == "wss" {
		targetScheme = "https"
	}
	target := &url.URL{Scheme: targetScheme, Host: hostAuthority}
	pool := cfg.transports
	if pool == nil {
		pool = &transportPool{}
	}
	transport := pool.get(clientKey, server, cfg)
	proxy := &httputil.ReverseProxy{
		Transport:     idleTimeoutTransport{base: transport, timeout: cfg.idleTimeout},
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = path
			if path != pathEscaped {
				pr.Out.URL.RawPath = pathEscaped
			}
			pr.Out.Host = hostAuthority
			pr.SetXForwarded()
		},
		ErrorHandler: func(rw http.ResponseWriter, r *http.Request, err error) {
			logrus.Errorf("proxy %s://%s via client %s failed: %v", scheme, hostAuthority, clientKey, err)
			http.Error(rw, "upstream proxy failed: "+err.Error(), http.StatusBadGateway)
		},
	}
	return proxy, nil
}

func proxyRequest(server *remotedialer.Server, rw http.ResponseWriter, req *http.Request, cfg proxyConfig) {
	if !tryAcquire(cfg.httpLimit) {
		http.Error(rw, "proxy busy", http.StatusServiceUnavailable)
		return
	}
	defer release(cfg.httpLimit)
	proxy, err := newProxy(server, req, cfg)
	if err != nil {
		http.Error(rw, "invalid target", http.StatusBadRequest)
		return
	}
	proxy.ServeHTTP(rw, req)
}

func isWebSocketRequest(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

func dispatch(server *remotedialer.Server, rw http.ResponseWriter, req *http.Request, cfg proxyConfig) {
	if isWebSocketRequest(req) {
		if req.Method != http.MethodGet {
			rw.Header().Set("Allow", http.MethodGet)
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}
	proxyRequest(server, rw, req, cfg)
}

func main() {
	var (
		addr                  string
		connectAddr           string
		proxyAddr             string
		mode                  string
		rawAddr               string
		httpLimit             int
		socksLimit            int
		idleTimeout           time.Duration
		responseHeaderTimeout time.Duration
		peerID                string
		peerToken             string
		peers                 string
		clientAuthFile        string
		originFile            string
		caFile                string
		peerCAFile            string
		verifyUpstream        bool
		debug                 bool
	)
	flag.StringVar(&addr, "listen", ":8123", "Listen address")
	flag.StringVar(&connectAddr, "connect-listen", "", "Split mode /connect listen address")
	flag.StringVar(&proxyAddr, "proxy-listen", "", "Split mode /client listen address")
	flag.StringVar(&mode, "listener-mode", "single", "Listener mode: single or split")
	flag.StringVar(&rawAddr, "socks5-listen", "", "SOCKS5 listen address for SSH/any TCP (empty disables)")
	flag.IntVar(&httpLimit, "http-proxy-limit", 256, "Maximum concurrent HTTP/WS proxy requests; 0 means unlimited")
	flag.IntVar(&socksLimit, "socks5-limit", 32, "Maximum concurrent SOCKS5 connections; 0 means unlimited")
	flag.DurationVar(&idleTimeout, "http-idle-timeout", 60*time.Second, "Maximum time without upstream response body data; 0 disables")
	flag.DurationVar(&responseHeaderTimeout, "response-header-timeout", 120*time.Second, "Maximum time to wait for an upstream response header")
	flag.StringVar(&peerID, "id", "", "Peer ID")
	flag.StringVar(&peerToken, "token", "", "Peer Token")
	flag.StringVar(&peers, "peers", "", "Peers format id:token:url,id:token:url")
	flag.StringVar(&clientAuthFile, "client-auth-file", "", "JSON client token file")
	flag.StringVar(&originFile, "origin-allowlist-file", "", "JSON Origin allowlist file")
	flag.StringVar(&caFile, "upstream-ca-file", "", "CA bundle for upstream HTTPS/WSS")
	flag.StringVar(&peerCAFile, "peer-ca-file", "", "CA bundle for verifying peer WSS certificates")
	flag.BoolVar(&verifyUpstream, "upstream-tls-verify", false, "Verify upstream HTTPS/WSS certificates")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	for _, name := range []string{"listen", "connect-listen", "proxy-listen", "listener-mode"} {
		if countFlag(name) > 1 {
			logrus.Fatalf("duplicate -%s", name)
		}
	}
	flag.Parse()
	// 在启动前注册指标，保证 /metrics 能看到会话/流量计数
	metrics.Register()
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
		// 提醒：-debug 会把隧道明文（含 Cookie/JWT）写入日志，禁止用于生产
		logrus.Warn("debug mode enables PrintTunnelData; tunnel payloads (cookies/JWT) will be logged")
		remotedialer.PrintTunnelData = true
	}

	auth := &authStore{authFile: clientAuthFile, originFile: originFile}
	if clientAuthFile == "" {
		logrus.Warn("client auth disabled: no -client-auth-file configured")
	}
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
	var peerRoots *x509.CertPool
	if peerCAFile != "" {
		var err error
		peerRoots, err = loadRoots(peerCAFile)
		if err != nil {
			logrus.Fatalf("load peer CA: %v", err)
		}
	}
	if httpLimit < 0 || socksLimit < 0 {
		logrus.Fatal("proxy limits must be non-negative")
	}
	if idleTimeout < 0 || responseHeaderTimeout < 0 {
		logrus.Fatal("timeouts must be non-negative")
	}
	var httpLimitCh, socksLimitCh chan struct{}
	if httpLimit > 0 {
		httpLimitCh = make(chan struct{}, httpLimit)
	}
	if socksLimit > 0 {
		socksLimitCh = make(chan struct{}, socksLimit)
	}
	proxyCfg := proxyConfig{
		verify:                verifyUpstream,
		roots:                 roots,
		idleTimeout:           idleTimeout,
		responseHeaderTimeout: responseHeaderTimeout,
		httpLimit:             httpLimitCh,
		socksLimit:            socksLimitCh,
		transports:            &transportPool{transports: make(map[string]*http.Transport)},
	}
	handler := remotedialer.New(auth.authorize, remotedialer.DefaultErrorWriter)
	handler.PeerToken = peerToken
	handler.PeerID = peerID
	if peerRoots != nil {
		// SetPeerTLSConfig 必须在 AddPeer 之前调用
		if err := handler.SetPeerTLSConfig(&tls.Config{RootCAs: peerRoots, MinVersion: tls.VersionTLS12}); err != nil {
			logrus.Fatalf("apply peer TLS config: %v", err)
		}
	}

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
		socks, err = startSocks5Listener(handler, rawAddr, proxyCfg.socksLimit)
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
	proxyRouter.HandleFunc("/metrics", func(rw http.ResponseWriter, req *http.Request) {
		promhttp.Handler().ServeHTTP(rw, req)
	})
	clientHandler := func(rw http.ResponseWriter, req *http.Request) { dispatch(handler, rw, req, proxyCfg) }
	proxyRouter.HandleFunc("/client/{id}/{scheme}/{host}", clientHandler)
	proxyRouter.HandleFunc("/client/{id}/{scheme}/{host}/{path...}", clientHandler)
	if mode == "single" {
		// 单监听模式没有独立的内部代理端口，只能与 /connect 共用监听器。
		connectRouter.HandleFunc("/metrics", func(rw http.ResponseWriter, req *http.Request) {
			promhttp.Handler().ServeHTTP(rw, req)
		})
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
	proxyCfg.transports.close()
	if socks != nil {
		_ = socks.Close()
	}
}
