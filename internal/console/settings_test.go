package console

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

func newTestServer(t *testing.T) (*Server, *FileConfig) {
	t.Helper()
	dir := t.TempDir()
	fc, err := LoadFileConfig(filepath.Join(dir, "console.json"))
	if err != nil {
		t.Fatal(err)
	}
	fc.DataDir = filepath.Join(dir, "data") // ne podrazumevani sistemski folder
	if _, err := fc.EnsureDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := fc.Save(); err != nil {
		t.Fatal(err)
	}
	cert, err := LoadCertificate(fc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		AgentToken: fc.AgentToken, AdminUser: fc.AdminUser, DataDir: fc.DataDir,
		File: fc, Certs: NewCertStore(cert), Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, fc
}

func do(h http.Handler, method, path, remote string, body any, auth ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = remote
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if len(auth) == 2 {
		req.SetBasicAuth(auth[0], auth[1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestFirstRunDefaults(t *testing.T) {
	_, fc := newTestServer(t)
	if len(fc.Locations) == 0 {
		t.Fatal("podrazumevane lokacije nisu upisane")
	}
	if len(fc.AgentToken) != 64 || fc.TLSCert == "" || fc.TLSKey == "" {
		t.Fatalf("nisu generisane podrazumevane vrednosti: %+v", fc)
	}
	// Ponovno učitavanje ne sme da promeni token.
	fc2, _ := LoadFileConfig(fc.Path())
	if changed, _ := fc2.EnsureDefaults(); changed || fc2.AgentToken != fc.AgentToken {
		t.Fatal("ponovno pokretanje je promenilo konfiguraciju")
	}
	// Namerno obrisane lokacije ostaju prazne i posle restarta.
	fc2.Update(func(c *FileConfig) { c.Locations = []Location{} })
	fc3, _ := LoadFileConfig(fc.Path())
	if changed, _ := fc3.EnsureDefaults(); changed || fc3.Locations == nil || len(fc3.Locations) != 0 {
		t.Fatalf("obrisane lokacije su vraćene: %+v", fc3.Locations)
	}
}

func TestSetupMode(t *testing.T) {
	s, fc := newTestServer(t)
	h := s.Handler()

	// Sa drugog računara: konzola nije podešena.
	if w := do(h, "GET", "/", "10.0.0.5:5000", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("udaljeni pristup u režimu podešavanja: %d", w.Code)
	}
	// Preko reverse proxy-ja (loopback + X-Forwarded-For): takođe odbijeno.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "10.0.0.5")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("proxy pristup u režimu podešavanja: %d", w.Code)
	}
	// Sa samog servera: stranica za podešavanje.
	if w := do(h, "GET", "/", "127.0.0.1:5000", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "setup.js") {
		t.Fatalf("lokalni pristup: %d %s", w.Code, w.Body.String())
	}
	// Prekratka lozinka.
	if w := do(h, "POST", "/api/setup", "127.0.0.1:5000", setupRequest{User: "admin", Password: "kratka"}); w.Code != 400 {
		t.Fatalf("kratka lozinka: %d", w.Code)
	}
	if w := do(h, "POST", "/api/setup", "127.0.0.1:5000", setupRequest{User: "netadmin", Password: "dobra-lozinka"}); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	// Drugi pokušaj ne sme da pregazi lozinku.
	if w := do(h, "POST", "/api/setup", "127.0.0.1:5000", setupRequest{Password: "druga-lozinka"}); w.Code == 200 {
		t.Fatal("setup je dozvoljen dva puta")
	}
	// Sada važi normalna prijava, i sa drugih računara.
	if w := do(h, "GET", "/api/agents", "10.0.0.5:5000", nil, "netadmin", "dobra-lozinka"); w.Code != 200 {
		t.Fatalf("prijava posle podešavanja: %d", w.Code)
	}
	if w := do(h, "GET", "/api/agents", "10.0.0.5:5000", nil, "netadmin", "pogresna"); w.Code != 401 {
		t.Fatalf("pogrešna lozinka: %d", w.Code)
	}
	// Lozinka je snimljena u fajl.
	fc2, _ := LoadFileConfig(fc.Path())
	if fc2.AdminUser != "netadmin" || !strings.HasPrefix(fc2.AdminPasswordHash, "$2") {
		t.Fatalf("lozinka nije snimljena: %+v", fc2)
	}
}

func TestSettingsPasswordAndCertificate(t *testing.T) {
	s, fc := newTestServer(t)
	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "lozinka-1"})

	w := do(h, "GET", "/api/settings", "10.0.0.5:1", nil, "admin", "lozinka-1")
	var st settingsResponse
	json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || st.AgentToken != fc.AgentToken || st.Certificate == nil || !st.Certificate.SelfSigned {
		t.Fatalf("settings: %d %s", w.Code, w.Body.String())
	}

	// Promena lozinke: pogrešna trenutna, pa ispravna.
	if w := do(h, "POST", "/api/settings/password", "10.0.0.5:1", passwordRequest{Current: "x", NewPassword: "lozinka-2"}, "admin", "lozinka-1"); w.Code != 403 {
		t.Fatalf("pogrešna trenutna lozinka: %d", w.Code)
	}
	if w := do(h, "POST", "/api/settings/password", "10.0.0.5:1", passwordRequest{Current: "lozinka-1", NewPassword: "lozinka-2"}, "admin", "lozinka-1"); w.Code != 200 {
		t.Fatalf("promena lozinke: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/settings", "10.0.0.5:1", nil, "admin", "lozinka-1"); w.Code != 401 {
		t.Fatal("stara lozinka i dalje radi")
	}

	// Otpremanje PFX-a iz „AD CS-a”.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Firma Root CA"},
		NotBefore: time.Now(), NotAfter: time.Now().AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	leafT := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "netmon.firma.local"},
		DNSNames: []string{"netmon.firma.local"}, NotBefore: time.Now(), NotAfter: time.Now().AddDate(1, 0, 0)}
	der, _ := x509.CreateCertificate(rand.Reader, leafT, ca, &key.PublicKey, key)
	leaf, _ := x509.ParseCertificate(der)
	pfx, _ := pkcs12.Modern.Encode(key, leaf, nil, "pfx-lozinka")

	if w := do(h, "POST", "/api/settings/certificate", "10.0.0.5:1",
		certificateRequest{PFXBase64: base64.StdEncoding.EncodeToString(pfx), Password: "pogresna"}, "admin", "lozinka-2"); w.Code != 400 {
		t.Fatalf("pogrešna PFX lozinka: %d", w.Code)
	}
	w = do(h, "POST", "/api/settings/certificate", "10.0.0.5:1",
		certificateRequest{PFXBase64: base64.StdEncoding.EncodeToString(pfx), Password: "pfx-lozinka"}, "admin", "lozinka-2")
	if w.Code != 200 {
		t.Fatalf("otpremanje sertifikata: %d %s", w.Code, w.Body.String())
	}
	if info := s.cfg.Certs.Info(); info.SelfSigned || info.DNSNames[0] != "netmon.firma.local" {
		t.Fatalf("sertifikat nije zamenjen: %+v", info)
	}
	// Posle restarta konzola učitava novi sertifikat iz konfiguracije.
	fc2, _ := LoadFileConfig(fc.Path())
	cert, err := LoadCertificate(fc2)
	if err != nil || cert.Leaf.Subject.CommonName != "netmon.firma.local" || fc2.TLSCert != "" {
		t.Fatalf("posle restarta: %v %+v", err, fc2)
	}
	if fi, err := os.Stat(fc2.TLSPFX); err != nil || fi.Size() == 0 {
		t.Fatal("PFX nije snimljen")
	}
}

func TestLoadFileConfigBOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.json")
	os.WriteFile(path, []byte("\xef\xbb\xbf{\"agent_token\":\"0123456789abcdef\",\"tls_pfx\":\"c.pfx\"}"), 0o600)
	c, err := LoadFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentToken != "0123456789abcdef" || c.TLSPFX != filepath.Join(dir, "c.pfx") || c.Listen != ":8443" {
		t.Fatalf("pogrešno učitano: %+v", c)
	}
}
