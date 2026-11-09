package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const autoCertLifetime = 10 * 365 * 24 * time.Hour

// Fingerprint is what clients pin: "sha256:" and lowercase hex of the leaf's DER
// (I18, I135). The CLI and the worker accept this form.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ensureAutoCert returns the listener's self-signed certificate, reusing the one in
// the data directory so pins survive restarts. It regenerates only an expired or
// unreadable one, which clients then see as a pin mismatch (I135).
func ensureAutoCert(dataDir string, ips []net.IP) (tls.Certificate, string, error) {
	dir := filepath.Join(dataDir, "tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create the certificate directory: %w", err)
	}
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if notAfter, err := certNotAfter(cert); err == nil && time.Now().Before(notAfter) {
			return cert, Fingerprint(cert.Certificate[0]), nil
		}
	}

	certPEM, keyPEM, err := generateCert(ips)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeFileAtomic(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, Fingerprint(cert.Certificate[0]), nil
}

func generateCert(ips []net.IP) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	sans := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			sans = append(sans, ip)
		}
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "vidyomd"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(autoCertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
