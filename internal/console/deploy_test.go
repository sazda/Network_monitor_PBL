package console

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseHosts(t *testing.T) {
	got, err := parseHosts([]string{"PC01\npc01, PC02;10.0.21.21  inf-milosm.firma.local"})
	if err != nil || strings.Join(got, "|") != "PC01|PC02|10.0.21.21|inf-milosm.firma.local" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "pc 01;bad_name", "-pc", "pc;rm -rf"} {
		if _, err := parseHosts([]string{bad}); err == nil {
			t.Errorf("očekivana greška za %q", bad)
		}
	}
}

func TestDescribeOutcome(t *testing.T) {
	code := func(c int) *int { return &c }
	cases := []struct {
		o    deployOutcome
		ok   bool
		want string
	}{
		{deployOutcome{ExitCode: code(0), Service: "Running"}, true, "instaliran (servis: Running)"},
		{deployOutcome{ExitCode: code(3010)}, true, "potreban restart"},
		{deployOutcome{ExitCode: code(1603)}, false, "1603"},
		{deployOutcome{Message: "Access is denied."}, false, "nije lokalni administrator"},
		{deployOutcome{Message: "WinRM cannot complete the operation"}, false, "WinRM nije dostupan"},
	}
	for _, c := range cases {
		ok, msg := describeOutcome(c.o, false)
		if ok != c.ok || !strings.Contains(msg, c.want) {
			t.Errorf("%+v: %v %q", c.o, ok, msg)
		}
	}
}

func TestDeployAPI(t *testing.T) {
	s, fc := newTestServer(t)
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	s.log = log.New(writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logBuf.Write(p) }), "", 0)

	// MSI agenta pored konfiguracije.
	os.WriteFile(filepath.Join(fc.Dir(), "NetworkMonitorAgent.msi"), []byte("msi"), 0o600)

	var mu sync.Mutex
	var seen []deployTarget
	s.deploy = newDeployManager(func(ctx context.Context, t deployTarget) deployOutcome {
		mu.Lock()
		seen = append(seen, t)
		mu.Unlock()
		if t.Computer == "LOS-PC" {
			return deployOutcome{Message: "Access is denied."}
		}
		c := 0
		return deployOutcome{OK: true, ExitCode: &c, Service: "Running"}
	})
	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	do(h, "POST", "/api/users", "10.0.0.5:1", userRequest{Name: "op", Password: "op-lozinka", Role: RoleOperator}, "admin", "admin-lozinka")

	req := deployRequest{Hosts: []string{"PC01\nPC02, LOS-PC"}, ConsoleURL: "https://netmon.firma.local:8443/", User: `FIRMA\admin`, Password: "tajna-lozinka-123"}
	if w := do(h, "POST", "/api/deploy", "10.0.0.5:1", req, "op", "op-lozinka"); w.Code != http.StatusForbidden {
		t.Fatalf("operater pokreće instalaciju: %d", w.Code)
	}
	bad := req
	bad.ConsoleURL = "https://localhost:8443"
	if w := do(h, "POST", "/api/deploy", "10.0.0.5:1", bad, "admin", "admin-lozinka"); w.Code != 400 {
		t.Fatalf("localhost kao adresa konzole: %d", w.Code)
	}

	w := do(h, "POST", "/api/deploy", "10.0.0.5:1", req, "admin", "admin-lozinka")
	if w.Code != http.StatusAccepted {
		t.Fatalf("pokretanje: %d %s", w.Code, w.Body.String())
	}
	var job DeployJob
	json.Unmarshal(w.Body.Bytes(), &job)

	deadline := time.Now().Add(5 * time.Second)
	for !job.Finished && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		json.Unmarshal(do(h, "GET", "/api/deploy/"+job.ID, "10.0.0.5:1", nil, "admin", "admin-lozinka").Body.Bytes(), &job)
	}
	if !job.Finished || len(job.Items) != 3 {
		t.Fatalf("posao nije završen: %+v", job)
	}
	for _, it := range job.Items {
		want := "ok"
		if it.Host == "LOS-PC" {
			want = "error"
		}
		if it.Status != want {
			t.Errorf("%s: %s (%s)", it.Host, it.Status, it.Message)
		}
	}
	// Izvršilac dobija token i adresu konzole; lozinka nikad ne ide u log.
	for _, tg := range seen {
		if tg.Token != fc.AgentToken || tg.URL != "https://netmon.firma.local:8443" || tg.Password != "tajna-lozinka-123" {
			t.Fatalf("pogrešni podaci za izvršioca: %+v", tg)
		}
	}
	logMu.Lock()
	defer logMu.Unlock()
	if strings.Contains(logBuf.String(), "tajna-lozinka-123") || strings.Contains(logBuf.String(), fc.AgentToken) {
		t.Fatal("lozinka ili token su upisani u log")
	}
	if !strings.Contains(logBuf.String(), "instalacija agenata za računare pokrenuta") {
		t.Fatal("pokretanje instalacije nije zabeleženo u logu")
	}
	for _, tg := range seen {
		if tg.MSIName != "NetworkMonitorAgent.msi" || tg.Service != "network-monitor-agent" {
			t.Fatalf("paket za računare: %+v", tg)
		}
	}
	logMu.Unlock()

	// Agent za servere: poseban MSI i servis.
	srv := req
	srv.Hosts = []string{"SQL01"}
	srv.Kind = "server"
	if w := do(h, "POST", "/api/deploy", "10.0.0.5:1", srv, "admin", "admin-lozinka"); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "NetworkMonitorServerAgent.msi") {
		t.Fatalf("bez MSI-ja za servere: %d %s", w.Code, w.Body.String())
	}
	bad = srv
	bad.Kind = "nesto"
	if w := do(h, "POST", "/api/deploy", "10.0.0.5:1", bad, "admin", "admin-lozinka"); w.Code != 400 {
		t.Fatalf("nepoznata vrsta: %d", w.Code)
	}
	os.WriteFile(filepath.Join(fc.Dir(), "NetworkMonitorServerAgent.msi"), []byte("msi"), 0o600)
	w = do(h, "POST", "/api/deploy", "10.0.0.5:1", srv, "admin", "admin-lozinka")
	if w.Code != http.StatusAccepted {
		t.Fatalf("serveri: %d %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &job)
	deadline = time.Now().Add(5 * time.Second)
	for !job.Finished && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		json.Unmarshal(do(h, "GET", "/api/deploy/"+job.ID, "10.0.0.5:1", nil, "admin", "admin-lozinka").Body.Bytes(), &job)
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if !job.Finished || last.Computer != "SQL01" || last.MSIName != "NetworkMonitorServerAgent.msi" ||
		last.Service != "network-monitor-server-agent" || !strings.HasSuffix(last.MSI, "NetworkMonitorServerAgent.msi") {
		t.Fatalf("paket za servere: %+v", last)
	}
	logMu.Lock()
	if !strings.Contains(logBuf.String(), "instalacija agenata za servere pokrenuta") {
		t.Fatal("instalacija za servere nije zabeležena")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
