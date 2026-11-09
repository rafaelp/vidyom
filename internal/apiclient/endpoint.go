package apiclient

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Kind is a transport of §5.3.
type Kind string

const (
	KindUnix  Kind = "unix"
	KindHTTPS Kind = "https"
	KindSSH   Kind = "ssh"
)

// Endpoint is where a daemon is reached.
type Endpoint struct {
	Kind Kind

	Socket string // unix: socket path

	URL         string // https: https://host[:port]
	Fingerprint string // https: lowercase hex SHA-256 of the leaf certificate (I164)
	CAFile      string // https: additional trust root
	ClientCert  string // https: mTLS certificate path
	ClientKey   string // https: mTLS key path

	SSHTarget    string // ssh: user@host
	RemoteSocket string // ssh: daemon socket on that host
	SSHBinary    string // ssh: client program; default "ssh"
}

// Display renders the endpoint for messages. It holds nothing secret.
func (e Endpoint) Display() string {
	switch e.Kind {
	case KindUnix:
		return "unix:" + e.Socket
	case KindSSH:
		return "ssh:" + e.SSHTarget + ":" + e.RemoteSocket
	default:
		return e.URL
	}
}

// ValidateHTTPSURL enforces I163: https only, no credentials and no path.
func ValidateHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errs.Newf(errs.CodeInvalidArgument, "invalid server URL %q", raw)
	}
	if u.Scheme != "https" {
		return errs.Newf(errs.CodeInvalidArgument, "the server URL must use https (D12), got %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return errs.New(errs.CodeInvalidArgument, "the server URL has no host")
	}
	if u.User != nil {
		return errs.New(errs.CodeInvalidArgument, "the server URL must not contain credentials")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errs.New(errs.CodeInvalidArgument, "the server URL must be https://host[:port], with no path or query")
	}
	return nil
}

var fingerprintHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NormalizeFingerprint accepts hex SHA-256 with or without colons, in either case,
// with an optional sha256: prefix, and returns lowercase hex.
func NormalizeFingerprint(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	if !fingerprintHex.MatchString(s) {
		return "", errs.New(errs.CodeInvalidArgument, "invalid TLS fingerprint: expected the SHA-256 of the certificate in hex")
	}
	return s, nil
}

// CertificateFingerprint returns the fingerprint of a DER certificate in the
// normalized form.
func CertificateFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// fingerprintError is a pin mismatch: possibly an interception, never retried.
type fingerprintError struct{ got, want string }

func (e *fingerprintError) Error() string {
	return fmt.Sprintf("TLS certificate fingerprint %s does not match the pinned %s", e.got, e.want)
}

func tlsConfig(e Endpoint) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if e.Fingerprint != "" {
		want, err := NormalizeFingerprint(e.Fingerprint)
		if err != nil {
			return nil, err
		}
		// The pin replaces chain and hostname validation for the self-signed
		// loopback certificate of I18. VerifyConnection also runs on resumed
		// sessions, which VerifyPeerCertificate would not.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return &fingerprintError{got: "(none)", want: want}
			}
			got := CertificateFingerprint(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return &fingerprintError{got: got, want: want}
			}
			return nil
		}
	} else if e.CAFile != "" {
		pem, err := os.ReadFile(e.CAFile)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "cannot read the CA file "+e.CAFile)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errs.Newf(errs.CodeInvalidArgument, "the CA file %s holds no certificate", e.CAFile)
		}
		cfg.RootCAs = pool
	}
	if e.ClientCert != "" || e.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(e.ClientCert, e.ClientKey)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "cannot load the client certificate")
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// transport is a configured round tripper and whatever it must release.
type transport struct {
	baseURL      string
	roundTripper http.RoundTripper
	closeFunc    func() error
}

func (t *transport) close() error {
	if t.closeFunc != nil {
		return t.closeFunc()
	}
	return nil
}

const dialTimeout = 15 * time.Second

func newTransport(e Endpoint) (*transport, error) {
	base := &http.Transport{
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		// Streaming and upload bodies must not be buffered or transparently
		// decompressed behind the client's back.
		DisableCompression: true,
	}
	switch e.Kind {
	case KindUnix:
		if e.Socket == "" {
			return nil, errs.New(errs.CodeInvalidArgument, "no Unix socket path configured")
		}
		dialer := net.Dialer{Timeout: dialTimeout}
		base.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", e.Socket)
		}
		return &transport{baseURL: "http://vidyom", roundTripper: base}, nil
	case KindHTTPS:
		if err := ValidateHTTPSURL(e.URL); err != nil {
			return nil, err
		}
		cfg, err := tlsConfig(e)
		if err != nil {
			return nil, err
		}
		base.TLSClientConfig = cfg
		base.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
		return &transport{baseURL: strings.TrimRight(e.URL, "/"), roundTripper: base}, nil
	case KindSSH:
		tunnel, err := newSSHTunnel(e)
		if err != nil {
			return nil, err
		}
		dialer := net.Dialer{Timeout: dialTimeout}
		base.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			path, err := tunnel.ensure(ctx)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, "unix", path)
		}
		return &transport{baseURL: "http://vidyom", roundTripper: base, closeFunc: tunnel.close}, nil
	default:
		return nil, errs.Newf(errs.CodeInvalidArgument, "unknown server kind %q", e.Kind)
	}
}

// classifyTLS turns a certificate failure into a clear, non-retryable error, or
// returns nil when err is something else.
func classifyTLS(err error) *errs.Error {
	var pin *fingerprintError
	if errors.As(err, &pin) {
		return errs.Wrap(errs.CodeUnavailable, err,
			"the server's TLS certificate does not match the pinned fingerprint; this may be an interception, so nothing was sent. If the certificate really changed, check the new fingerprint with the operator and run server add --force").
			With("reason", "tls_fingerprint_mismatch")
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &unknownAuthority), errors.As(err, &verify):
		return errs.Wrap(errs.CodeUnavailable, err,
			"the server's TLS certificate is not trusted. For a self-signed certificate, pin it with server add --fingerprint; for an internal CA, use --ca-file").
			With("reason", "tls_untrusted")
	case errors.As(err, &hostname):
		return errs.Wrap(errs.CodeUnavailable, err, "the server's TLS certificate does not match its name").
			With("reason", "tls_hostname")
	case errors.As(err, &invalid):
		return errs.Wrap(errs.CodeUnavailable, err, "the server's TLS certificate is invalid").
			With("reason", "tls_invalid")
	}
	if strings.Contains(err.Error(), "protocol version") {
		return errs.Wrap(errs.CodeUnavailable, err, "the server does not support TLS 1.3, the minimum this client accepts").
			With("reason", "tls_version")
	}
	return nil
}
