package console

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSplitAccount(t *testing.T) {
	for _, c := range []struct{ in, user, dom string }{
		{`FIRMA\srvc_as`, "srvc_as", "FIRMA"},
		{` firma\pera `, "pera", "firma"},
		{"pera@firma.local", "pera@firma.local", ""},
	} {
		u, d, err := splitAccount(c.in)
		if err != nil || u != c.user || d != c.dom {
			t.Errorf("%q: %q %q %v", c.in, u, d, err)
		}
	}
	for _, bad := range []string{"srvc_as", `\x`, `FIRMA\`, "a@b", `A\b\c`} {
		if _, _, err := splitAccount(bad); err == nil {
			t.Errorf("%q: prihvaćen", bad)
		}
	}
}

func TestADVerifierCache(t *testing.T) {
	v := newADVerifier()
	now := time.Now()
	v.now = func() time.Time { return now }
	var calls int
	v.auth = func(user, domain, pass string) error {
		calls++
		if pass != "dobra" {
			return &adError{Msg: "pogrešan nalog ili lozinka"}
		}
		return nil
	}
	if err := v.check(`FIRMA\a`, "losa"); err == nil {
		t.Fatal("pogrešna lozinka prihvaćena")
	}
	if err := v.check(`FIRMA\a`, "dobra"); err != nil {
		t.Fatal(err)
	}
	if err := v.check(`firma\A`, "dobra"); err != nil || calls != 2 {
		t.Fatalf("zapamćena provera: %v, poziva %d", err, calls)
	}
	// Druga lozinka za isti nalog se ponovo proverava.
	if err := v.check(`FIRMA\a`, "losa"); err == nil || calls != 3 {
		t.Fatalf("druga lozinka: %v, poziva %d", err, calls)
	}
	now = now.Add(adOKFor + time.Second)
	v.check(`FIRMA\a`, "dobra")
	if calls != 4 {
		t.Fatalf("istekla provera: %d", calls)
	}
}

type maintFixture struct {
	s      *Server
	h      http.Handler
	sshN   atomic.Int32
	reply  func(addr string) (string, error)
	adPass string
}

func newMaintFixture(t *testing.T, n int) *maintFixture {
	t.Helper()
	f := &maintFixture{adPass: "lozinka-123"}
	s, _ := newTestServer(t)
	f.s = s
	s.ad.auth = func(_, _, pass string) error {
		if pass != f.adPass {
			return &adError{Msg: "domen je odbio prijavu: pogrešan nalog ili lozinka (switch-evi nisu kontaktirani)"}
		}
		return nil
	}
	s.sshRun = func(_ context.Context, addr, _, _ string, _ ssh.HostKeyCallback, cmd string) (string, error) {
		f.sshN.Add(1)
		if f.reply != nil {
			return f.reply(addr)
		}
		return "hostname " + addr + "\n" + cmd + "\n", nil
	}
	var devs []NetDevice
	for i := 1; i <= n; i++ {
		devs = append(devs, NetDevice{ID: fmt.Sprintf("sw%d", i), Name: fmt.Sprintf("HQ_SW-%d", i), IP: fmt.Sprintf("10.0.35.%d", i), Type: "switch", Active: true, SNMP: true})
	}
	if err := s.net.setDevices(devs); err != nil {
		t.Fatal(err)
	}
	s.net.mu.Lock()
	for _, d := range devs {
		s.net.rt(d.ID).snmp = &snmpRuntime{ok: true, checked: time.Now(), sys: snmpSystem{Model: "C9200L-48P-4X"}}
	}
	s.net.mu.Unlock()
	f.h = s.Handler()
	do(f.h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	return f
}

func (f *maintFixture) start(t *testing.T, action, pass string, ids ...string) (int, maintJobView, string) {
	t.Helper()
	w := do(f.h, "POST", "/api/maintjobs", "10.0.0.5:1", maintJobRequest{maintRequest: maintRequest{User: `FIRMA\pera`, Password: pass}, Action: action, IDs: ids}, "admin", "admin-lozinka")
	var v maintJobView
	json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v, w.Body.String()
}

func (f *maintFixture) wait(t *testing.T, id string) maintJobView {
	t.Helper()
	for i := 0; i < 500; i++ {
		w := do(f.h, "GET", "/api/maintjobs/"+id, "10.0.0.5:1", nil, "admin", "admin-lozinka")
		var v maintJobView
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("stanje: %d %s", w.Code, w.Body)
		}
		if v.Done {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("posao se nije završio")
	return maintJobView{}
}

func TestMaintJobZip(t *testing.T) {
	f := newMaintFixture(t, 3)
	f.reply = func(addr string) (string, error) {
		if strings.HasPrefix(addr, "10.0.35.2:") {
			return "", errHostKeyChanged
		}
		return "hostname " + addr + "\n", nil
	}
	// Pogrešna lozinka: domen odbija, switch-evi se ne kontaktiraju.
	if code, _, body := f.start(t, "backup", "losa", "sw1", "sw2", "sw3"); code != http.StatusUnprocessableEntity || !strings.Contains(body, "nisu kontaktirani") || f.sshN.Load() != 0 {
		t.Fatalf("pogrešna lozinka: %d %s, ssh %d", code, body, f.sshN.Load())
	}
	if code, _, _ := f.start(t, "reload", "lozinka-123", "sw1"); code != http.StatusBadRequest {
		t.Fatalf("nedozvoljena radnja: %d", code)
	}
	code, v, body := f.start(t, "backup", "lozinka-123", "sw1", "sw2", "sw3", "sw1")
	if code != http.StatusAccepted || v.Total != 3 {
		t.Fatalf("pokretanje: %d %s", code, body)
	}
	v = f.wait(t, v.ID)
	if v.OK != 2 || v.Failed != 1 || v.Stopped != "" || !strings.Contains(v.Results[1].Error, "zaboravite stari ključ") {
		t.Fatalf("ishod: %+v", v)
	}
	if strings.Contains(fmt.Sprint(v.Results), "hostname") {
		t.Fatal("sadržaj konfiguracije u stanju posla")
	}
	w := do(f.h, "GET", "/api/maintjobs/"+v.ID+"/zip", "10.0.0.5:1", nil, "admin", "admin-lozinka")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "switch-backup-") {
		t.Fatalf("zip: %d %v", w.Code, w.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[zf.Name] = string(b)
	}
	if len(files) != 3 {
		t.Fatalf("fajlovi: %v", files)
	}
	rep := files["izvestaj.txt"]
	if !strings.Contains(rep, "Uspešno: 2 od 3") || !strings.Contains(rep, "GREŠKA   HQ_SW-2") || !strings.Contains(rep, `FIRMA\pera`) || strings.Contains(rep, "lozinka-123") {
		t.Fatalf("izveštaj:\n%s", rep)
	}
	for name, body := range files {
		if strings.HasPrefix(name, "HQ_SW-1-backup-") && body != "hostname 10.0.35.1:22\n" {
			t.Fatalf("%s: %q", name, body)
		}
	}
}

func TestMaintJobStopsOnSwitchAuth(t *testing.T) {
	f := newMaintFixture(t, 7)
	var mu sync.Mutex
	f.reply = func(string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return "", errSwitchAuth
	}
	code, v, body := f.start(t, "log", "lozinka-123", "sw1", "sw2", "sw3", "sw4", "sw5", "sw6", "sw7")
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	v = f.wait(t, v.ID)
	// Najviše 4 istovremena pokušaja; ostali se ne pokreću.
	if v.Stopped == "" || v.Skipped != 3 || v.Failed != 4 || f.sshN.Load() != 4 {
		t.Fatalf("prekid: %+v ssh=%d", v, f.sshN.Load())
	}
}

func TestMaintSingleChecksDomain(t *testing.T) {
	f := newMaintFixture(t, 1)
	w := do(f.h, "POST", "/api/maint/sw1/log", "10.0.0.5:1", maintRequest{User: `FIRMA\a`, Password: "losa"}, "admin", "admin-lozinka")
	if w.Code != http.StatusUnprocessableEntity || f.sshN.Load() != 0 {
		t.Fatalf("pogrešna lozinka: %d ssh=%d", w.Code, f.sshN.Load())
	}
	f.s.ad.auth = func(string, string, string) error {
		return &adError{Msg: "kontroler domena nije dostupan", Unavailable: true}
	}
	if w := do(f.h, "POST", "/api/maint/sw1/log", "10.0.0.5:1", maintRequest{User: `FIRMA\a`, Password: "lozinka-123"}, "admin", "admin-lozinka"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("domen nedostupan: %d", w.Code)
	}
	f.s.ad.auth = func(string, string, string) error { return errADUnsupported }
	if w := do(f.h, "POST", "/api/maint/sw1/log", "10.0.0.5:1", maintRequest{User: "bez-domena", Password: "x"}, "admin", "admin-lozinka"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("nalog bez domena: %d", w.Code)
	}
	if w := do(f.h, "POST", "/api/maint/sw1/log", "10.0.0.5:1", maintRequest{User: `FIRMA\a`, Password: "x"}, "admin", "admin-lozinka"); w.Code != 200 || f.sshN.Load() != 1 {
		t.Fatalf("bez provere (nije Windows): %d", w.Code)
	}
}
