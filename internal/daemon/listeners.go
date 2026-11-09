package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
)

// ListenerInfo describes a bound listener, for doctor and for the operator's log.
type ListenerInfo struct {
	Kind              string // "unix" or "https"
	Address           string // socket path, or host:port
	Mode              string // socket mode
	Interface         string
	TLSMode           config.TLSMode
	MutualTLS         bool
	Public            bool // reachable beyond private ranges (§9.3)
	LoopbackPublished bool // I133
	CertNotAfter      time.Time
}

// listenerPlan is everything checked and loaded, with nothing bound yet, so a bad
// certificate or a missing interface stops startup before any socket exists (I134).
type listenerPlan struct {
	info    ListenerInfo
	network string
	address string
	tls     *tls.Config
	mode    os.FileMode // unix only
}

// boundListener is a plan with its socket.
type boundListener struct {
	info ListenerInfo
	ln   net.Listener
	tls  *tls.Config
}

func validateListeners(l config.Listeners) error {
	if l.Unix == nil && l.HTTPS == nil {
		return errors.New("no listener is configured")
	}
	if l.Unix != nil {
		if err := validateUnix(*l.Unix); err != nil {
			return err
		}
	}
	if l.HTTPS != nil {
		if err := validateHTTPS(*l.HTTPS); err != nil {
			return err
		}
	}
	return nil
}

// socketMode parses and checks a socket mode (I136).
func socketMode(raw string) (os.FileMode, error) {
	if raw == "" {
		return 0o600, nil
	}
	n, err := strconv.ParseUint(raw, 8, 32)
	if err != nil || n > 0o777 {
		return 0, fmt.Errorf("listeners.unix.mode %q is not an octal file mode", raw)
	}
	mode := os.FileMode(n)
	if mode&0o007 != 0 {
		return 0, fmt.Errorf("listeners.unix.mode %q gives access to everyone, which would make peer identity meaningless", raw)
	}
	if mode&0o600 != 0o600 {
		return 0, fmt.Errorf("listeners.unix.mode %q must give the owner read and write", raw)
	}
	return mode, nil
}

func validateUnix(u config.UnixListener) error {
	if !peerCredentialsSupported {
		return errors.New("listeners.unix is not available on this platform, which has no peer credentials: use listeners.https on loopback")
	}
	if u.Path == "" || !filepath.IsAbs(u.Path) {
		return errors.New("listeners.unix.path must be absolute")
	}
	if len(u.Path) >= maxSocketPath {
		return fmt.Errorf("listeners.unix.path is %d bytes; this platform allows fewer than %d", len(u.Path), maxSocketPath)
	}
	_, err := socketMode(u.Mode)
	return err
}

func validateHTTPS(h config.HTTPSListener) error {
	if (h.Bind == "") == (h.Interface == "") {
		return errors.New("listeners.https needs exactly one of bind or interface")
	}
	if h.Port < 0 || h.Port > 65535 {
		return fmt.Errorf("listeners.https.port %d is out of range", h.Port)
	}
	if h.Bind != "" && net.ParseIP(h.Bind) == nil {
		return fmt.Errorf("listeners.https.bind %q must be an IP address, not a host name, which could resolve to anything", h.Bind)
	}
	switch h.TLS.Mode {
	case config.TLSAuto:
		if h.TLS.CertFile != "" || h.TLS.KeyFile != "" {
			return errors.New("listeners.https.tls: mode auto takes no certFile or keyFile")
		}
	case config.TLSFiles:
		if h.TLS.CertFile == "" || h.TLS.KeyFile == "" {
			return errors.New("listeners.https.tls: mode files needs certFile and keyFile")
		}
	case "":
		return errors.New("listeners.https.tls.mode is required: auto or files")
	default:
		return fmt.Errorf("listeners.https.tls.mode %q is unknown", h.TLS.Mode)
	}
	if h.Bind != "" {
		return checkAddressRules(h, []net.IP{net.ParseIP(h.Bind)})
	}
	return nil
}

// checkAddressRules holds I19 and I133, once the addresses are known.
func checkAddressRules(h config.HTTPSListener, ips []net.IP) error {
	for _, ip := range ips {
		switch {
		case ip.IsUnspecified():
			if !h.AllowPublic {
				return fmt.Errorf("listeners.https binds %s, every interface: this needs allowPublic and is never a fallback (§9.3)", ip)
			}
			if h.TLS.Mode != config.TLSFiles {
				return fmt.Errorf("listeners.https binds %s: this needs a real certificate, tls.mode files", ip)
			}
			if h.LoopbackPublished {
				return errors.New("listeners.https.loopbackPublished needs a private address, not every interface")
			}
		case h.TLS.Mode == config.TLSAuto && ip.IsLoopback():
		case h.TLS.Mode == config.TLSAuto && h.LoopbackPublished && isPrivate(ip):
		case h.TLS.Mode == config.TLSAuto && h.LoopbackPublished:
			return fmt.Errorf("listeners.https.loopbackPublished needs a private address, not %s", ip)
		case h.TLS.Mode == config.TLSAuto:
			return fmt.Errorf("listeners.https on %s: tls.mode auto is for loopback only; provide a certificate, or set loopbackPublished for a container port published on host loopback", ip)
		}
	}
	return nil
}

// isPrivate covers RFC 1918, ULA and the shared range overlay networks use, such as
// Tailscale's 100.64.0.0/10.
func isPrivate(ip net.IP) bool {
	if ip.IsPrivate() || ip.IsLoopback() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1]&0xc0 == 64
	}
	return false
}

// resolveAddresses turns the configuration into IPs. A missing or down interface,
// or one with no usable address, is an error; there is no fallback (§9.3, I134).
func resolveAddresses(h config.HTTPSListener, lookup func(string) ([]net.IP, error)) ([]net.IP, error) {
	if h.Bind != "" {
		return []net.IP{net.ParseIP(h.Bind)}, nil
	}
	ips, err := lookup(h.Interface)
	if err != nil {
		return nil, fmt.Errorf("listeners.https.interface %q: %w; refusing to start rather than listening anywhere else", h.Interface, err)
	}
	return ips, nil
}

func interfaceAddresses(name string) ([]net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	if iface.Flags&net.FlagUp == 0 {
		return nil, errors.New("the interface is down")
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, addr := range addrs {
		var ip net.IP
		switch a := addr.(type) {
		case *net.IPNet:
			ip = a.IP
		case *net.IPAddr:
			ip = a.IP
		}
		if ip == nil || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			continue
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		return nil, errors.New("the interface has no usable address")
	}
	return ips, nil
}

func buildTLS(h config.HTTPSListener, cert tls.Certificate) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
	if h.TLS.ClientCAFile != "" {
		pem, err := os.ReadFile(h.TLS.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("listeners.https.tls.clientCAFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("listeners.https.tls.clientCAFile holds no certificate")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// planListeners validates, resolves and loads everything without binding. It also
// returns the fingerprint to pin when a listener uses an automatic certificate.
func planListeners(cfg config.Daemon, lookup func(string) ([]net.IP, error)) ([]listenerPlan, string, error) {
	if err := validateListeners(cfg.Listeners); err != nil {
		return nil, "", err
	}
	var plans []listenerPlan
	var fingerprint string

	if u := cfg.Listeners.Unix; u != nil {
		mode, err := socketMode(u.Mode)
		if err != nil {
			return nil, "", err
		}
		if err := prepareSocketPath(u.Path); err != nil {
			return nil, "", err
		}
		plans = append(plans, listenerPlan{
			info:    ListenerInfo{Kind: "unix", Address: u.Path, Mode: fmt.Sprintf("%04o", mode)},
			network: "unix", address: u.Path, mode: mode,
		})
	}

	if h := cfg.Listeners.HTTPS; h != nil {
		ips, err := resolveAddresses(*h, lookup)
		if err != nil {
			return nil, "", err
		}
		if err := checkAddressRules(*h, ips); err != nil {
			return nil, "", err
		}
		var cert tls.Certificate
		if h.TLS.Mode == config.TLSAuto {
			cert, fingerprint, err = ensureAutoCert(cfg.DataDir, ips)
		} else {
			cert, err = tls.LoadX509KeyPair(h.TLS.CertFile, h.TLS.KeyFile)
		}
		if err != nil {
			return nil, "", fmt.Errorf("listeners.https.tls: %w", err)
		}
		tlsConfig, err := buildTLS(*h, cert)
		if err != nil {
			return nil, "", err
		}
		notAfter, err := certNotAfter(cert)
		if err != nil {
			return nil, "", fmt.Errorf("listeners.https.tls: %w", err)
		}
		for _, ip := range ips {
			address := net.JoinHostPort(ip.String(), strconv.Itoa(h.Port))
			plans = append(plans, listenerPlan{
				info: ListenerInfo{
					Kind: "https", Address: address, Interface: h.Interface,
					TLSMode: h.TLS.Mode, MutualTLS: h.TLS.ClientCAFile != "",
					Public:            !isPrivate(ip),
					LoopbackPublished: h.LoopbackPublished,
					CertNotAfter:      notAfter,
				},
				network: "tcp", address: address, tls: tlsConfig,
			})
		}
	}
	return plans, fingerprint, nil
}

func certNotAfter(cert tls.Certificate) (time.Time, error) {
	if len(cert.Certificate) == 0 {
		return time.Time{}, errors.New("empty certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	return leaf.NotAfter, nil
}

// bindAll binds every plan, or none: on any failure every socket opened so far is
// closed, and nothing was ever served (I19).
func bindAll(ctx context.Context, plans []listenerPlan) ([]boundListener, error) {
	var bound []boundListener
	closeAll := func() {
		for _, b := range bound {
			_ = b.ln.Close()
		}
	}
	var lc net.ListenConfig
	for _, plan := range plans {
		ln, err := lc.Listen(ctx, plan.network, plan.address)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("listen on %s: %w", plan.info.Address, err)
		}
		info := plan.info
		if plan.network == "unix" {
			if err := os.Chmod(plan.address, plan.mode); err != nil {
				_ = ln.Close()
				closeAll()
				return nil, fmt.Errorf("set the socket's mode: %w", err)
			}
		} else {
			// With port 0 the real port is only known now.
			info.Address = ln.Addr().String()
		}
		bound = append(bound, boundListener{info: info, ln: ln, tls: plan.tls})
	}
	return bound, nil
}

// prepareSocketPath creates or checks the socket's directory and clears a stale
// socket. Anything unexpected aborts (I136).
func prepareSocketPath(path string) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create the socket's directory: %w", err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("check the socket's directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the socket's parent %s is not a directory", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("the socket's directory %s is writable by others (mode %04o): another user could replace the socket", dir, info.Mode().Perm())
	}
	if err := checkOwner(dir, info); err != nil {
		return err
	}

	existing, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check the socket path: %w", err)
	}
	if existing.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("another daemon is listening on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the stale socket: %w", err)
	}
	return nil
}
