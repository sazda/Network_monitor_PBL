package console

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeCisco je SSH server koji glumi Cisco CLI (lozinka „dobra”; nalog
// „slab” ima samo korisnički pristup, odziv „>”).
func fakeCisco(t *testing.T) (addr string, stop func()) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if string(p) != "dobra" {
			return nil, errors.New("pogrešna lozinka")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveCisco(c, cfg)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func serveCisco(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	prompt := "HQ_SW#"
	if sc.User() == "slab" {
		prompt = "HQ_SW>"
	}
	for nc := range chans {
		ch, creqs, _ := nc.Accept()
		go func() {
			for r := range creqs {
				r.Reply(r.Type == "pty-req" || r.Type == "shell", nil)
			}
		}()
		io.WriteString(ch, "\r\n*** Samo ovlasceni pristup ***\r\n\r\n"+prompt)
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 1024)
		for {
			n, err := ch.Read(tmp)
			if err != nil {
				break
			}
			buf = append(buf, tmp[:n]...)
			for {
				i := strings.IndexByte(string(buf), '\n')
				if i < 0 {
					break
				}
				line := strings.TrimSpace(string(buf[:i]))
				buf = buf[i+1:]
				io.WriteString(ch, line+"\r\n") // eho
				switch {
				case line == "exit":
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
					ch.Close()
					return
				case line == "terminal length 0":
				case prompt == "HQ_SW>" && line == "show running-config":
					io.WriteString(ch, "                ^\r\n% Invalid input detected at '^' marker.\r\n\r\n")
				case line == "show running-config":
					io.WriteString(ch, "Building configuration...\r\n\r\nCurrent configuration : 42 bytes\r\n!\r\nhostname HQ_SW\r\n!\r\nend\r\n\r\n")
				case line == "show logging":
					io.WriteString(ch, "Syslog logging: enabled\r\n*Nov 10 09:12:01: %LINK-3-UPDOWN: Interface Gi1/0/3, changed state to up\r\n")
				}
				io.WriteString(ch, prompt)
			}
		}
	}
}

func TestRunSwitchCommand(t *testing.T) {
	addr, stop := fakeCisco(t)
	defer stop()
	kh := newKnownHosts(t.TempDir())
	ctx := context.Background()

	out, err := runSwitchCommand(ctx, addr, "FIRMA\\pera", "dobra", kh.callback("10.0.35.6"), "show running-config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Building configuration...") || !strings.Contains(out, "hostname HQ_SW\n!\nend") ||
		strings.Contains(out, "HQ_SW#") || strings.Contains(out, "ovlasceni") {
		t.Fatalf("backup: %q", out)
	}
	out, err = runSwitchCommand(ctx, addr, "a", "dobra", kh.callback("10.0.35.6"), "show logging")
	if err != nil || !strings.Contains(out, "%LINK-3-UPDOWN") || strings.Contains(out, "show logging") {
		t.Fatalf("log: %q %v", out, err)
	}
	if _, err := runSwitchCommand(ctx, addr, "a", "losa", kh.callback("10.0.35.6"), "show logging"); err == nil || !strings.Contains(err.Error(), "nalog i lozinku") {
		t.Fatalf("pogrešna lozinka: %v", err)
	}
	if _, err := runSwitchCommand(ctx, addr, "slab", "dobra", kh.callback("10.0.35.6"), "show running-config"); err == nil || !strings.Contains(err.Error(), "privilegovani") {
		t.Fatalf("korisnički pristup: %v", err)
	}
	// Drugi „switch” na istoj adresi (drugi ključ): prijava se odbija.
	addr2, stop2 := fakeCisco(t)
	defer stop2()
	if _, err := runSwitchCommand(ctx, addr2, "a", "dobra", kh.callback("10.0.35.6"), "show logging"); !errors.Is(err, errHostKeyChanged) {
		t.Fatalf("promenjen ključ: %v", err)
	}
	kh.forget("10.0.35.6")
	if _, err := runSwitchCommand(ctx, addr2, "a", "dobra", kh.callback("10.0.35.6"), "show logging"); err != nil {
		t.Fatalf("posle brisanja ključa: %v", err)
	}
}

func TestMaintAPI(t *testing.T) {
	s, _ := newTestServer(t)
	var gotCmd, gotUser string
	s.sshRun = func(_ context.Context, addr, user, pass string, _ ssh.HostKeyCallback, cmd string) (string, error) {
		gotCmd, gotUser = cmd, user
		if pass != "lozinka-123" {
			return "", errors.New("prijava na switch nije uspela: proverite nalog i lozinku")
		}
		return "hostname HQ_FIN_S-I\n", nil
	}
	if err := s.net.setDevices([]NetDevice{
		{ID: "sw1", Name: "HQ_FIN_S-I", IP: "10.0.35.6", Type: "switch", Active: true, SNMP: true},
		{ID: "r1", Name: "MPLS ruter", IP: "10.0.35.1", Type: "router", Active: true, SNMP: true},
	}); err != nil {
		t.Fatal(err)
	}
	s.net.mu.Lock()
	now := time.Now()
	s.net.rt("sw1").snmp = &snmpRuntime{ok: true, checked: now, sys: snmpSystem{Model: "C9200L-48P-4X"}}
	s.net.rt("r1").snmp = &snmpRuntime{ok: true, checked: now, sys: snmpSystem{Model: "ISR4331"}}
	s.net.mu.Unlock()

	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	do(h, "POST", "/api/users", "10.0.0.5:1", userRequest{Name: "op", Password: "op-lozinka", Role: RoleOperator}, "admin", "admin-lozinka")
	req := maintRequest{User: `FIRMA\pera`, Password: "lozinka-123"}

	if w := do(h, "POST", "/api/maint/sw1/backup", "10.0.0.5:1", req, "op", "op-lozinka"); w.Code != http.StatusForbidden {
		t.Fatalf("operater: %d", w.Code)
	}
	if w := do(h, "POST", "/api/maint/r1/backup", "10.0.0.5:1", req, "admin", "admin-lozinka"); w.Code != http.StatusBadRequest {
		t.Fatalf("ruter: %d", w.Code)
	}
	if w := do(h, "POST", "/api/maint/sw1/reload", "10.0.0.5:1", req, "admin", "admin-lozinka"); w.Code != http.StatusNotFound {
		t.Fatalf("nedozvoljena radnja: %d", w.Code)
	}
	w := do(h, "POST", "/api/maint/sw1/backup", "10.0.0.5:1", req, "admin", "admin-lozinka")
	if w.Code != 200 || w.Body.String() != "hostname HQ_FIN_S-I\n" || gotCmd != "show running-config" || gotUser != `FIRMA\pera` {
		t.Fatalf("backup: %d %q %q", w.Code, w.Body.String(), gotCmd)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="HQ_FIN_S-I-backup-`) {
		t.Fatalf("naziv fajla: %q", cd)
	}
	w = do(h, "POST", "/api/maint/sw1/log", "10.0.0.5:1", maintRequest{User: `FIRMA\a`, Password: "losa"}, "admin", "admin-lozinka")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "nalog i lozinku") || gotCmd != "show logging" {
		t.Fatalf("pogrešna lozinka: %d %s", w.Code, w.Body.String())
	}
	del := httptest.NewRequest("DELETE", "/api/maint/sw1/hostkey", nil)
	del.RemoteAddr = "10.0.0.5:1"
	del.SetBasicAuth("admin", "admin-lozinka")
	del.Header.Set("X-Requested-With", "fetch")
	dw := httptest.NewRecorder()
	h.ServeHTTP(dw, del)
	if dw.Code != http.StatusNoContent {
		t.Fatalf("brisanje ključa: %d", dw.Code)
	}
}
