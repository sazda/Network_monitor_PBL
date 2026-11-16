package console

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Održavanje switch-eva (kartica „Održavanje”, samo administrator): konzola
// se preko SSH-a prijavljuje nalogom koji administrator upiše za svaku radnju
// (domenski nalog – switch ga proverava preko AAA) i preuzima log ili
// konfiguraciju. Nalog i lozinka se ne čuvaju i ne upisuju u log. Izvršavaju
// se samo unapred zadate komande za pregled.

const (
	sshTimeout    = 90 * time.Second
	knownHostFile = "ssh-known-hosts.json"
)

// maintActions: dozvoljene radnje i komanda za svaku.
var maintActions = map[string]struct {
	cmd, file, label string
}{
	"log":    {"show logging", "log", "log"},
	"backup": {"show running-config", "backup", "backup konfiguracije"},
}

// maintModelOK: radnje su dozvoljene samo za Cisco C9200L i C2960L
// (model iz SNMP-a).
func maintModelOK(model string) bool {
	m := strings.ToUpper(model)
	return strings.Contains(m, "C9200L") || strings.Contains(m, "C2960L")
}

// knownHosts pamti SSH ključ switch-a pri prvoj prijavi; ako se kasnije
// promeni (zamena uređaja ili napad „čovek u sredini”), prijava se odbija dok
// administrator ne obriše zapamćeni ključ.
type knownHosts struct {
	mu   sync.Mutex
	path string
}

func (k *knownHosts) load() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(k.path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (k *knownHosts) save(m map[string]string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}

// errHostKeyChanged: ključ switch-a nije isti kao zapamćeni.
var errHostKeyChanged = errors.New("SSH ključ switch-a se promenio od prve prijave")

// errSwitchAuth: switch je odbio nalog ili lozinku (posao za više switch-eva
// se tada prekida, da se domenski nalog ne zaključa).
var errSwitchAuth = errors.New("prijava na switch nije uspela: proverite nalog i lozinku")

func (k *knownHosts) callback(ip string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		k.mu.Lock()
		defer k.mu.Unlock()
		m := k.load()
		got := base64.StdEncoding.EncodeToString(key.Marshal())
		if want, ok := m[ip]; ok {
			if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
				return fmt.Errorf("%w (sada %s)", errHostKeyChanged, ssh.FingerprintSHA256(key))
			}
			return nil
		}
		m[ip] = got
		return k.save(m)
	}
}

func (k *knownHosts) forget(ip string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	m := k.load()
	delete(m, ip)
	return k.save(m)
}

// sshConfig: algoritmi koje podržavaju i noviji IOS-XE i stariji IOS (2960L).
func sshConfig(user, pass string, hk ssh.HostKeyCallback) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(pass),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				a := make([]string, len(qs))
				for i := range a {
					a[i] = pass
				}
				return a, nil
			}),
		},
		HostKeyCallback:   hk,
		HostKeyAlgorithms: []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "rsa-sha2-512", "rsa-sha2-256", "ssh-rsa"},
		Timeout:           15 * time.Second,
		Config: ssh.Config{
			KeyExchanges: []string{"curve25519-sha256", "curve25519-sha256@libssh.org", "ecdh-sha2-nistp256",
				"diffie-hellman-group14-sha256", "diffie-hellman-group14-sha1"},
			Ciphers: []string{"aes128-gcm@openssh.com", "aes256-gcm@openssh.com", "aes128-ctr", "aes192-ctr", "aes256-ctr"},
		},
	}
}

// promptRe: odziv Cisco CLI-ja („HQ_FIN_S-I#” ili „HQ_FIN_S-I>”).
var promptRe = regexp.MustCompile(`^[\w.\-/:]+[#>]\s*$`)

// runSwitchCommand se prijavljuje na switch i izvršava jednu komandu za
// pregled; vraća samo izlaz te komande.
func runSwitchCommand(ctx context.Context, addr, user, pass string, hk ssh.HostKeyCallback, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("konzola ne može da se poveže na switch (SSH, port 22): %w", err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig(user, pass, hk))
	if err != nil {
		conn.Close()
		if errors.Is(err, errHostKeyChanged) {
			return "", err
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return "", errSwitchAuth
		}
		return "", fmt.Errorf("SSH: %w", err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	go func() { <-ctx.Done(); client.Close() }()

	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	if err := sess.RequestPty("vt100", 200, 512, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		return "", err
	}
	in, err := sess.StdinPipe()
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	sess.Stdout = &limitWriter{w: &out, n: 32 << 20}
	if err := sess.Shell(); err != nil {
		return "", err
	}
	io.WriteString(in, "terminal length 0\n"+cmd+"\nexit\n")
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-done:
	case <-ctx.Done():
		return "", errors.New("switch nije odgovorio na vreme")
	}
	return extractOutput(out.String(), cmd)
}

// extractOutput izdvaja izlaz komande iz celog razgovora sa CLI-jem (bez
// pozdravne poruke, eha komandi i odziva).
func extractOutput(all, cmd string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(all, "\r", ""), "\n")
	start := -1
	userExec := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasSuffix(t, cmd) && (strings.Contains(t, "#") || strings.Contains(t, ">")) {
			start = i + 1
			userExec = strings.Contains(strings.TrimSuffix(t, cmd), ">")
			break
		}
	}
	if start < 0 {
		return "", errors.New("switch nije prihvatio komandu (nalog možda nema pravo pristupa CLI-ju)")
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if promptRe.MatchString(strings.TrimSpace(lines[i])) || strings.HasSuffix(strings.TrimSpace(lines[i]), "exit") && strings.ContainsAny(lines[i], "#>") {
			end = i
			break
		}
	}
	body := strings.Join(lines[start:end], "\n")
	if strings.Contains(body, "% Invalid input") || strings.Contains(body, "% Authorization failed") || strings.Contains(body, "Command authorization failed") {
		if userExec {
			return "", errors.New("nalog nema privilegovani pristup na switch-u (odziv „>”, potreban je „#”)")
		}
		return "", errors.New("switch je odbio komandu (nalog nema pravo za nju)")
	}
	return strings.TrimSpace(body) + "\n", nil
}

// limitWriter ograničava količinu izlaza (zaštita memorije konzole).
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	k := len(p)
	if k > l.n {
		k = l.n
	}
	l.n -= k
	l.w.Write(p[:k])
	return len(p), nil
}

type maintRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// handleMaint: POST /api/maint/{id}/{action} – preuzimanje loga ili
// konfiguracije switch-a (fajl za čuvanje na računaru).
func (s *Server) handleMaint(w http.ResponseWriter, r *http.Request) {
	act, ok := maintActions[r.PathValue("action")]
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("nepoznata radnja"))
		return
	}
	v, ok := s.net.device(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	model := ""
	if v.SNMPInfo != nil {
		model = v.SNMPInfo.Model
	}
	if !maintModelOK(model) {
		writeErr(w, http.StatusBadRequest, errors.New("održavanje je dostupno samo za Cisco C9200L i C2960L (model se čita preko SNMP-a)"))
		return
	}
	var req maintRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.User = strings.TrimSpace(req.User)
	if req.User == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, errors.New("upišite nalog i lozinku"))
		return
	}
	me := identityFrom(r)
	if !s.checkDomainAccount(w, me, req) {
		return
	}
	addr := net.JoinHostPort(v.IP, "22")
	out, err := s.sshRunner()(r.Context(), addr, req.User, req.Password, s.known.callback(v.IP), act.cmd)
	if err != nil {
		s.log.Printf("održavanje: %s (%s) – %s sa %s (%s) nije uspeo: %v", me.Name, req.User, act.label, v.Name, v.IP, err)
		code := http.StatusBadGateway
		if errors.Is(err, errHostKeyChanged) {
			code = http.StatusConflict
		}
		writeErr(w, code, err)
		return
	}
	s.log.Printf("održavanje: %s (nalog na switch-u %s) preuzeo %s sa %s (%s), %d bajtova", me.Name, req.User, act.label, v.Name, v.IP, len(out))
	name := fmt.Sprintf("%s-%s-%s.txt", safeFileName(v.Name), act.file, time.Now().Format("20060102-1504"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, out)
}

func (s *Server) sshRunner() func(ctx context.Context, addr, user, pass string, hk ssh.HostKeyCallback, cmd string) (string, error) {
	if s.sshRun != nil {
		return s.sshRun
	}
	return runSwitchCommand
}

// checkDomainAccount proverava nalog u domenu pre prijave na switch; kada
// provera odbije nalog, šalje grešku i vraća false.
func (s *Server) checkDomainAccount(w http.ResponseWriter, me Identity, req maintRequest) bool {
	err := s.ad.check(req.User, req.Password)
	var ae *adError
	switch {
	case err == nil:
		return true
	case errors.Is(err, errADUnsupported):
		return true // konzola nije na Windows-u: switch sam proverava nalog
	case errors.As(err, &ae):
		s.log.Printf("održavanje: %s – provera domenskog naloga %s nije uspela: %s", me.Name, req.User, ae.Msg)
		code := http.StatusUnprocessableEntity
		if ae.Unavailable {
			code = http.StatusServiceUnavailable
		}
		writeErr(w, code, ae)
		return false
	}
	writeErr(w, http.StatusInternalServerError, err)
	return false
}

// handleMaintForgetKey: DELETE /api/maint/{id}/hostkey – zaboravlja
// zapamćeni SSH ključ switch-a (posle zamene uređaja).
func (s *Server) handleMaintForgetKey(w http.ResponseWriter, r *http.Request) {
	v, ok := s.net.device(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	if err := s.known.forget(v.IP); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("održavanje: %s je obrisao zapamćeni SSH ključ za %s (%s)", identityFrom(r).Name, v.Name, v.IP)
	w.WriteHeader(http.StatusNoContent)
}

var unsafeFileRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFileName(s string) string {
	s = strings.Trim(unsafeFileRe.ReplaceAllString(s, "_"), "_")
	if s == "" {
		s = "switch"
	}
	return s
}

func newKnownHosts(dataDir string) *knownHosts {
	return &knownHosts{path: filepath.Join(dataDir, knownHostFile)}
}
