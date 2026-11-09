package plugins

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

const (
	defaultNetworkPort = 443
	proxyUser          = "plugin"
	proxyDialTimeout   = 10 * time.Second
	proxyMaxConns      = 64
)

// networkRule is one entry of permissions.network (I263).
type networkRule struct {
	host     string // lowercase, without brackets; the suffix for a wildcard, with its dot
	wildcard bool
	port     int
	ip       bool // the rule names an IP literal
}

func parseNetworkRule(text string) (networkRule, error) {
	invalid := errs.Newf(errs.CodeValidationFailed, "invalid network permission %q: use host, host:port or *.suffix", text)
	if text == "" || strings.ContainsAny(text, "/@?# \t\r\n") {
		return networkRule{}, invalid
	}
	host, port := text, defaultNetworkPort
	if h, p, err := net.SplitHostPort(text); err == nil {
		value, err := strconv.Atoi(p)
		if err != nil || value < 1 || value > 65535 {
			return networkRule{}, invalid
		}
		host, port = h, value
	} else if strings.Count(text, ":") > 0 && net.ParseIP(strings.Trim(text, "[]")) == nil {
		return networkRule{}, invalid
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	rule := networkRule{host: host, port: port}
	if strings.HasPrefix(host, "*.") {
		rule.wildcard = true
		rule.host = host[1:]
		// Refuses *.com: a wildcard has to name somebody's domain.
		if strings.Count(rule.host, ".") < 2 {
			return networkRule{}, invalid
		}
	} else if strings.Contains(host, "*") {
		return networkRule{}, invalid
	}
	if ip := net.ParseIP(host); ip != nil {
		rule.ip = true
		rule.host = ip.String()
		return rule, nil
	}
	if rule.host == "" || strings.Trim(rule.host, ".-") == "" {
		return networkRule{}, invalid
	}
	return rule, nil
}

func (r networkRule) matches(host string, port int) bool {
	if port != r.port {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if r.wildcard {
		return strings.HasSuffix(host, r.host) && len(host) > len(r.host)
	}
	return host == r.host
}

// egressProxy enforces a process's host allow-list. The sandbox lets the process reach
// only this proxy's port, so the list holds even for a client that would rather not
// use it; without a sandbox it is advisory (I263).
type egressProxy struct {
	rules    []networkRule
	listener net.Listener
	server   *http.Server
	auth     string
	token    string
	logger   *slog.Logger
	onDeny   func(host string, port int)

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	slots  chan struct{}
	wg     sync.WaitGroup
}

func startProxy(rules []networkRule, logger *slog.Logger, onDeny func(string, int)) (*egressProxy, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw[:])
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start egress proxy: %w", err)
	}
	p := &egressProxy{
		rules:    rules,
		listener: listener,
		token:    token,
		auth:     "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUser+":"+token)),
		logger:   logger,
		onDeny:   onDeny,
		conns:    map[net.Conn]struct{}{},
		slots:    make(chan struct{}, proxyMaxConns),
	}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		_ = p.server.Serve(listener)
	}()
	return p, nil
}

// URL is the proxy address the process is given.
func (p *egressProxy) URL() string {
	u := url.URL{Scheme: "http", User: url.UserPassword(proxyUser, p.token), Host: p.listener.Addr().String()}
	return u.String()
}

// Port is the only TCP port the sandbox lets the process reach.
func (p *egressProxy) Port() int { return p.listener.Addr().(*net.TCPAddr).Port }

// Close stops the proxy and every tunnel through it.
func (p *egressProxy) Close() {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	_ = p.server.Close()
	p.wg.Wait()
}

func (p *egressProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *egressProxy) untrack(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, c)
	_ = c.Close()
}

// allowed returns the rule for a destination.
func (p *egressProxy) allowed(host string, port int) (networkRule, bool) {
	for _, r := range p.rules {
		if r.matches(host, port) {
			return r, true
		}
	}
	return networkRule{}, false
}

func (p *egressProxy) deny(w http.ResponseWriter, host string, port int) {
	p.logger.Warn("plugin network request denied", "event", "plugin.network_denied", "host", host, "port", port)
	if p.onDeny != nil {
		p.onDeny(host, port)
	}
	http.Error(w, "destination is not in this plugin's network permission", http.StatusForbidden)
}

// dialer refuses a link-local address unless the rule named it literally, so a granted
// hostname cannot be resolved to a cloud metadata endpoint (I263).
func (p *egressProxy) dialer(rule networkRule) *net.Dialer {
	return &net.Dialer{
		Timeout: proxyDialTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			if rule.ip {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
				return errors.New("link-local destination refused")
			}
			return nil
		},
	}
}

func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Proxy-Authorization") != p.auth {
		w.Header().Set("Proxy-Authenticate", `Basic realm="vidyom-plugin"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	p.forward(w, r)
}

func hostPort(target string, defaultPort int) (string, int, bool) {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return strings.Trim(target, "[]"), defaultPort, target != ""
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

// tunnel serves CONNECT, which is how every client reaches an HTTPS destination.
func (p *egressProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, ok := hostPort(r.Host, defaultNetworkPort)
	if !ok {
		http.Error(w, "invalid destination", http.StatusBadRequest)
		return
	}
	rule, ok := p.allowed(host, port)
	if !ok {
		p.deny(w, host, port)
		return
	}
	upstream, err := p.dialer(rule).DialContext(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		http.Error(w, "cannot reach the destination", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be tunneled", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if !p.track(client) {
		return
	}
	defer p.untrack(client)
	if !p.track(upstream) {
		return
	}
	defer p.untrack(upstream)
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, buffered)
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if c, ok := client.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// forward serves absolute-URI requests, which is how a client reaches plain HTTP. No
// redirect is followed: the client's next request goes through the check again.
func (p *egressProxy) forward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "this is a forward proxy", http.StatusBadRequest)
		return
	}
	defaultPort := 80
	if r.URL.Scheme == "https" {
		defaultPort = 443
	}
	host, port, ok := hostPort(r.URL.Host, defaultPort)
	if !ok {
		http.Error(w, "invalid destination", http.StatusBadRequest)
		return
	}
	rule, ok := p.allowed(host, port)
	if !ok {
		p.deny(w, host, port)
		return
	}
	dialer := p.dialer(rule)
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}
	defer transport.CloseIdleConnections()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")
	resp, err := transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "cannot reach the destination", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
