package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA file names inside the CA directory.
const (
	CACertFile = "ca.pem"
	CAKeyFile  = "ca-key.pem"
)

// LoadOrCreateCA loads the MITM CA from dir, generating it on first use.
// The key is written with 0600 permissions. The CA is never installed in any
// system store. It returns the CA, the path of its certificate and whether
// it was just created.
func LoadOrCreateCA(dir string) (*tls.Certificate, string, bool, error) {
	certPath := filepath.Join(dir, CACertFile)
	keyPath := filepath.Join(dir, CAKeyFile)

	_, errCert := os.Stat(certPath)
	_, errKey := os.Stat(keyPath)
	switch {
	case errCert == nil && errKey == nil:
		ca, err := loadCA(certPath, keyPath)
		return ca, certPath, false, err
	case errors.Is(errCert, fs.ErrNotExist) && errors.Is(errKey, fs.ErrNotExist):
		ca, err := createCA(dir, certPath, keyPath)
		return ca, certPath, true, err
	case errCert != nil && !errors.Is(errCert, fs.ErrNotExist):
		return nil, "", false, fmt.Errorf("CA certificate: %w", errCert)
	case errKey != nil && !errors.Is(errKey, fs.ErrNotExist):
		return nil, "", false, fmt.Errorf("CA key: %w", errKey)
	default:
		return nil, "", false, fmt.Errorf("CA directory %s holds only one of %s and %s: remove it to regenerate", dir, CACertFile, CAKeyFile)
	}
}

func loadCA(certPath, keyPath string) (*tls.Certificate, error) {
	ca, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	leaf, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse CA: %w", err)
	}
	if !leaf.IsCA {
		return nil, fmt.Errorf("%s is not a CA certificate", certPath)
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("CA %s expired on %s: remove the directory to regenerate it", certPath, leaf.NotAfter.Format(time.DateOnly))
	}
	ca.Leaf = leaf
	return &ca, nil
}

func createCA(dir, certPath, keyPath string) (*tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create CA dir: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	host, _ := os.Hostname()
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("ricochet local MITM CA (%s)", host),
			Organization: []string{"ricochet debugging proxy"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	// Key first, with restrictive permissions; O_EXCL avoids clobbering.
	if err := writeExclusive(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := writeExclusive(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		os.Remove(keyPath)
		return nil, err
	}
	return loadCA(certPath, keyPath)
}

func writeExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// certCache caches the leaf certificates generated for MITM'd hosts.
type certCache struct {
	mu    sync.Mutex
	certs map[string]*tls.Certificate
}

func newCertCache() *certCache { return &certCache{certs: map[string]*tls.Certificate{}} }

// Fetch implements goproxy.CertStorage.
func (c *certCache) Fetch(host string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.certs[host]; ok && (cert.Leaf == nil || time.Now().Before(cert.Leaf.NotAfter)) {
		return cert, nil
	}
	cert, err := gen()
	if err != nil {
		return nil, err
	}
	c.certs[host] = cert
	return cert, nil
}

// ServerTLSConfig returns the TLS configuration used towards servers for
// MITM'd requests: the system roots plus the PEM certificates of caFiles
// (e.g. the internal CA of a Kubernetes cluster). insecure disables server
// certificate verification altogether. It returns nil when neither is set.
func ServerTLSConfig(caFiles []string, insecure bool) (*tls.Config, error) {
	if len(caFiles) == 0 && !insecure {
		return nil, nil
	}
	cfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // explicit opt-in (--upstream-insecure)
	if len(caFiles) == 0 {
		return cfg, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	for _, f := range caFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read server CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("server CA %s: no PEM certificate found", f)
		}
	}
	cfg.RootCAs = pool
	return cfg, nil
}
