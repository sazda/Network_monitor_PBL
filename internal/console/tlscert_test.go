package console

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// Windows Server 2025 podrazumevano izvozi PFX sa AES-256 šifrovanjem
// (pkcs12.Modern), a starije verzije sa 3DES (pkcs12.Legacy).
func TestLoadPFX(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "monitor.firma.local"},
		DNSNames:     []string{"monitor.firma.local"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	for name, enc := range map[string]*pkcs12.Encoder{"modern": pkcs12.Modern, "legacy": pkcs12.LegacyDES} {
		t.Run(name, func(t *testing.T) {
			pfx, err := enc.Encode(key, cert, nil, "tajna")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "c.pfx")
			os.WriteFile(path, pfx, 0o600)
			got, err := LoadPFX(path, "tajna")
			if err != nil {
				t.Fatal(err)
			}
			if got.Leaf.Subject.CommonName != "monitor.firma.local" {
				t.Fatalf("pogrešan sertifikat: %v", got.Leaf.Subject)
			}
			if _, err := LoadPFX(path, "pogresna"); err == nil {
				t.Fatal("očekivana greška za pogrešnu lozinku")
			}
		})
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("lozinka123")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{AgentToken: "0123456789abcdef", AdminUser: "admin", AdminHash: h, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !s.checkPassword("lozinka123") || !s.checkPassword("lozinka123") || s.checkPassword("pogresna") {
		t.Fatal("provera lozinke ne radi")
	}
}
