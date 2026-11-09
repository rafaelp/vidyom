// Package apitest is a fake daemon for tests of the client, built from the shared
// route table: exactly the routes of api.Routes exist, and each request is checked
// against what the table declares (tenant rule D32, authentication, idempotency
// keys on mutations). It serves over a real Unix socket or real TLS 1.3. Only tests
// import it; it never links into a binary.
package apitest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Call is one request the server received.
type Call struct {
	Route   string
	Method  string
	Path    string
	Query   url.Values
	Header  http.Header
	Body    []byte
	Mutates bool
}

// JSONBody decodes the request body.
func (c Call) JSONBody(t testing.TB) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(c.Body, &out); err != nil {
		t.Fatalf("%s: body is not JSON: %v: %q", c.Route, err, c.Body)
	}
	return out
}

// Server is the fake daemon.
type Server struct {
	t        testing.TB
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    []Call
	mux      *http.ServeMux
	closers  []func()

	// Token, when set, is the only accepted bearer credential on user routes. An
	// empty Token accepts any request, which is how the Unix socket's peer identity
	// looks from the client.
	Token string
}

// New builds a server with every route answering 501 not_implemented until a test
// gives it a handler.
func New(t testing.TB) *Server {
	s := &Server{t: t, handlers: map[string]http.HandlerFunc{}, mux: http.NewServeMux()}
	for _, r := range api.Routes {
		route := r
		s.mux.HandleFunc(route.Pattern(), func(w http.ResponseWriter, req *http.Request) { s.serve(route, w, req) })
	}
	t.Cleanup(s.Close)
	return s
}

// Close stops all listeners.
func (s *Server) Close() {
	s.mu.Lock()
	closers := s.closers
	s.closers = nil
	s.mu.Unlock()
	for _, f := range closers {
		f()
	}
}

func (s *Server) serve(route api.Route, w http.ResponseWriter, req *http.Request) {
	var body []byte
	if route.Name != api.RouteUploadsChunk { // chunk bodies are the upload fake's
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{
		Route: route.Name, Method: req.Method, Path: req.URL.Path, Query: req.URL.Query(),
		Header: req.Header.Clone(), Body: body, Mutates: route.Mutating(),
	})
	handler := s.handlers[route.Name]
	s.mu.Unlock()

	w.Header().Set(api.HeaderRequestID, "req-test")
	if err := s.check(route, req); err != nil {
		WriteError(w, err)
		return
	}
	if handler == nil {
		WriteError(w, errs.NotImplemented(route.Name))
		return
	}
	handler(w, req)
}

// check applies what the route table declares, as the real server must.
func (s *Server) check(route api.Route, req *http.Request) *errs.Error {
	tenant := req.Header.Get(api.HeaderTenant)
	switch route.Tenant {
	case api.TenantRequired:
		if tenant == "" {
			return errs.New(errs.CodeTenantRequired, "the Vidyom-Tenant header is required")
		}
	case api.TenantNone:
		if tenant != "" {
			s.t.Errorf("%s: tenant-free route received Vidyom-Tenant: %s", route.Name, tenant)
		}
	}
	authz := req.Header.Get("Authorization")
	switch route.Auth {
	case api.AuthNone:
		if authz != "" {
			s.t.Errorf("%s: unauthenticated route received a credential", route.Name)
		}
	case api.AuthUser:
		if s.Token != "" && authz != "Bearer "+s.Token {
			return errs.New(errs.CodeUnauthenticated, "a valid device credential is required")
		}
	}
	if route.Mutating() && req.Header.Get(api.HeaderIdempotencyKey) == "" {
		s.t.Errorf("%s: mutating route received no Idempotency-Key", route.Name)
	}
	return nil
}

// Handle sets a route's handler.
func (s *Server) Handle(route string, h http.HandlerFunc) {
	api.ByName(route) // panics on an unknown route
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[route] = h
}

// Handler returns a route's current handler.
func (s *Server) Handler(route string) http.HandlerFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handlers[route]
}

// JSON answers a route with a fixed JSON body.
func (s *Server) JSON(route string, status int, v any) {
	s.Handle(route, func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, status, v) })
}

// Error answers a route with a coded error.
func (s *Server) Error(route string, e *errs.Error) {
	s.Handle(route, func(w http.ResponseWriter, _ *http.Request) { WriteError(w, e) })
}

// Calls returns recorded requests, optionally only of the named routes.
func (s *Server) Calls(routes ...string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(routes) == 0 {
		return append([]Call(nil), s.calls...)
	}
	var out []Call
	for _, c := range s.calls {
		for _, r := range routes {
			if c.Route == r {
				out = append(out, c)
			}
		}
	}
	return out
}

// Last returns the most recent call of a route.
func (s *Server) Last(route string) (Call, bool) {
	calls := s.Calls(route)
	if len(calls) == 0 {
		return Call{}, false
	}
	return calls[len(calls)-1], true
}

// Mutations returns every recorded request that could change state.
func (s *Server) Mutations() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if c.Mutates {
			out = append(out, c)
		}
	}
	return out
}

// Reset forgets recorded requests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

// Hit lists the route names that received at least one request.
func (s *Server) Hit() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, c := range s.calls {
		out[c.Route] = true
	}
	return out
}

// WriteJSON writes a JSON answer.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError writes the API's error format.
func WriteError(w http.ResponseWriter, e *errs.Error) {
	out := *e
	out.RequestID = "req-test"
	WriteJSON(w, errs.HTTPStatus(e.Code), errs.Body{Error: &out})
}

// SSEFrame is one frame to write on a stream.
type SSEFrame struct {
	ID   string
	Name string
	Data any
}

// WriteSSEHeaders begins a stream.
func WriteSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", api.SSEContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush(w)
}

// WriteSSE writes one frame and flushes.
func WriteSSE(w http.ResponseWriter, f SSEFrame) {
	if f.ID != "" {
		fmt.Fprintf(w, "id: %s\n", f.ID)
	}
	if f.Name != "" {
		fmt.Fprintf(w, "event: %s\n", f.Name)
	}
	if f.Data != nil {
		raw, _ := json.Marshal(f.Data)
		for _, line := range bytes.Split(raw, []byte("\n")) {
			fmt.Fprintf(w, "data: %s\n", line)
		}
	}
	fmt.Fprint(w, "\n")
	flush(w)
}

// WriteSSEComment writes a keep-alive comment.
func WriteSSEComment(w http.ResponseWriter) {
	fmt.Fprint(w, ": keep-alive\n\n")
	flush(w)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// LastEventID parses the resume header of a stream request; -1 when absent.
func LastEventID(r *http.Request) int64 {
	value := r.Header.Get(api.HeaderLastEventID)
	if value == "" {
		return -1
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// ServeUnix listens on a real Unix socket and returns its path. The path is short,
// because socket paths are limited to about 100 bytes.
func (s *Server) ServeUnix() string {
	dir, err := os.MkdirTemp("", "vdm")
	if err != nil {
		s.t.Fatal(err)
	}
	path := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		s.t.Fatal(err)
	}
	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	s.addCloser(func() {
		_ = srv.Close()
		_ = os.RemoveAll(dir)
	})
	return path
}

// ServeTLS listens on loopback with a fresh self-signed certificate and TLS 1.3,
// like the daemon's auto mode (I18). It returns the URL and the fingerprint hex.
func (s *Server) ServeTLS() (string, string) {
	return s.ServeTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13})
}

// ServeTLSConfig is ServeTLS with a chosen TLS configuration. Certificates are
// filled in when empty.
func (s *Server) ServeTLSConfig(cfg *tls.Config) (string, string) {
	cert, err := SelfSigned()
	if err != nil {
		s.t.Fatal(err)
	}
	if len(cfg.Certificates) == 0 {
		cfg.Certificates = []tls.Certificate{cert}
	}
	srv := httptest.NewUnstartedServer(s.mux)
	srv.TLS = cfg
	// Refused handshakes are what several tests are about.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	s.addCloser(srv.Close)
	return srv.URL, Fingerprint(cfg.Certificates[0].Certificate[0])
}

func (s *Server) addCloser(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, f)
}

// Fingerprint is the SHA-256 of a DER certificate in lowercase hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// SelfSigned makes a throwaway certificate for 127.0.0.1 and localhost.
func SelfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "vidyom-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
