package console

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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// LoadPFX učitava sertifikat i privatni ključ iz PFX (PKCS#12) fajla, u
// formatu u kom ga izvozi Windows / Active Directory Certificate Services.
func LoadPFX(path, password string) (tls.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return tls.Certificate{}, err
	}
	return ParsePFX(data, password)
}

// ParsePFX dekodira PFX iz memorije.
func ParsePFX(data []byte, password string) (tls.Certificate, error) {
	key, cert, chain, err := pkcs12.DecodeChain(data, password)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("čitanje PFX fajla (proverite lozinku i da je izvezen privatni ključ): %w", err)
	}
	if key == nil || cert == nil {
		return tls.Certificate{}, errors.New("PFX ne sadrži sertifikat sa privatnim ključem")
	}
	out := tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}
	for _, c := range chain {
		out.Certificate = append(out.Certificate, c.Raw)
	}
	return out, nil
}

// LoadCertificate učitava sertifikat iz konfiguracije (PFX ili PEM par).
func LoadCertificate(c *FileConfig) (tls.Certificate, error) {
	var cert tls.Certificate
	var err error
	switch {
	case c.TLSPFX != "":
		cert, err = LoadPFX(c.TLSPFX, c.TLSPFXPassword)
	case c.TLSCert != "" && c.TLSKey != "":
		cert, err = tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
	default:
		return cert, errors.New("TLS sertifikat nije podešen")
	}
	if err != nil {
		return cert, fmt.Errorf("TLS sertifikat: %w", err)
	}
	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	return cert, nil
}

// CertStore drži trenutni TLS sertifikat i dozvoljava njegovu zamenu bez
// restarta konzole (npr. kada administrator otpremi PFX iz web interfejsa).
type CertStore struct {
	mu   sync.RWMutex
	cert *tls.Certificate
}

// NewCertStore kreira skladište sa početnim sertifikatom.
func NewCertStore(cert tls.Certificate) *CertStore { return &CertStore{cert: &cert} }

// GetCertificate se koristi u tls.Config.
func (s *CertStore) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cert, nil
}

// Set menja sertifikat.
func (s *CertStore) Set(cert tls.Certificate) {
	s.mu.Lock()
	s.cert = &cert
	s.mu.Unlock()
}

// CertInfo opisuje sertifikat za prikaz u web interfejsu.
type CertInfo struct {
	Subject    string    `json:"subject"`
	Issuer     string    `json:"issuer"`
	DNSNames   []string  `json:"dns_names"`
	NotAfter   time.Time `json:"not_after"`
	SelfSigned bool      `json:"self_signed"`
}

// Info vraća opis trenutnog sertifikata.
func (s *CertStore) Info() *CertInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cert == nil || s.cert.Leaf == nil {
		return nil
	}
	l := s.cert.Leaf
	return &CertInfo{
		Subject:    l.Subject.String(),
		Issuer:     l.Issuer.String(),
		DNSNames:   l.DNSNames,
		NotAfter:   l.NotAfter,
		SelfSigned: l.Subject.String() == l.Issuer.String(),
	}
}

// GenerateSelfSigned pravi privremeni sertifikat (važi 2 godine) za ime
// računara, localhost i sve lokalne IP adrese, i snima ga u dir.
// Agenti mu NE veruju — služi samo da web interfejs radi odmah posle
// instalacije, dok se ne otpremi sertifikat iz internog CA.
func GenerateSelfSigned(dir string) (certPath, keyPath string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: host, Organization: []string{"Network Monitor (privremeni sertifikat)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ipn.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	certPath = filepath.Join(dir, "selfsigned.crt")
	keyPath = filepath.Join(dir, "selfsigned.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}
