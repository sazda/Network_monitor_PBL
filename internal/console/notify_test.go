package console

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type sentLog struct {
	mu   sync.Mutex
	msgs []notifyMessage
}

func (l *sentLog) send(_ context.Context, _ NotifyConfig, m notifyMessage) []channelResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, m)
	return []channelResult{{Channel: "test", OK: true}}
}

func (l *sentLog) take() []notifyMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.msgs
	l.msgs = nil
	return m
}

func newTestNotifier(cfg NotifyConfig) (*notifier, *sentLog, *time.Time) {
	names := map[string][2]string{
		"a1": {"INF-MILOSM", "Filijala"},
		"a2": {"HRD-SMILJAM", "Filijala"},
		"a3": {"FIN-SANDRAA", "Jug"},
	}
	n := newNotifier(cfg, log.New(io.Discard, "", 0), func(id string) (string, string) {
		return names[id][0], names[id][1]
	})
	sl := &sentLog{}
	n.send = sl.send
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.Local)
	n.now = func() time.Time { return now }
	return n, sl, &now
}

func ag(id string, online bool) AgentInfo {
	a := AgentInfo{Online: online}
	a.AgentID = id
	return a
}

func TestNotifierDelayGroupingAndRecovery(t *testing.T) {
	n, sl, now := newTestNotifier(NotifyConfig{Enabled: true, Watch: []string{"a1", "a2"}, DelayMinutes: 5, TeamsURL: "https://x"})

	// Kratak prekid (kraći od 5 min) ne šalje ništa.
	n.agentChanged(ag("a1", false))
	*now = now.Add(2 * time.Minute)
	n.tick()
	n.agentChanged(ag("a1", true))
	*now = now.Add(10 * time.Minute)
	n.tick()
	if m := sl.take(); len(m) != 0 {
		t.Fatalf("kratak prekid ne treba da šalje obaveštenje: %+v", m)
	}

	// Dva označena i jedan neoznačen računar odlaze: jedno grupisano obaveštenje.
	n.agentChanged(ag("a1", false))
	n.agentChanged(ag("a2", false))
	n.agentChanged(ag("a3", false))
	*now = now.Add(5 * time.Minute)
	n.tick()
	m := sl.take()
	if len(m) != 1 || m[0].Title != "Network Monitor: 2 računara nisu na vezi" || !m[0].Down {
		t.Fatalf("očekivano jedno obaveštenje za 2 računara, dobijeno %+v", m)
	}
	if !strings.Contains(m[0].Body, "HRD-SMILJAM (Filijala)") || !strings.Contains(m[0].Body, "INF-MILOSM (Filijala)") || strings.Contains(m[0].Body, "FIN-SANDRAA") {
		t.Fatalf("neispravan tekst:\n%s", m[0].Body)
	}

	// Ne ponavlja se dok je računar i dalje van veze.
	*now = now.Add(time.Hour)
	n.tick()
	if m := sl.take(); len(m) != 0 {
		t.Fatalf("obaveštenje ne treba da se ponavlja: %+v", m)
	}

	// Povratak: „ponovo na vezi” samo za računare za koje je poslato „nije na vezi”.
	n.agentChanged(ag("a1", true))
	n.agentChanged(ag("a3", true))
	n.tick()
	m = sl.take()
	if len(m) != 1 || m[0].Title != "Network Monitor: INF-MILOSM je ponovo na vezi" || m[0].Down || !m[0].Recovery {
		t.Fatalf("očekivano obaveštenje o povratku, dobijeno %+v", m)
	}
	if !strings.Contains(m[0].Body, "nije bio na vezi 1 h 5 min") {
		t.Fatalf("trajanje prekida:\n%s", m[0].Body)
	}
}

func TestNotifierAllAgentsDisabledAndStartup(t *testing.T) {
	n, sl, now := newTestNotifier(NotifyConfig{Enabled: false, AllAgents: true, TeamsURL: "https://x"})
	n.agentChanged(ag("a3", false))
	*now = now.Add(10 * time.Minute)
	n.tick()
	if m := sl.take(); len(m) != 0 {
		t.Fatalf("isključena obaveštenja: %+v", m)
	}

	// Posle restarta konzole: agent koji se vrati u roku ne izaziva obaveštenje,
	// a onaj koji se ne vrati izaziva.
	n, sl, now = newTestNotifier(NotifyConfig{Enabled: true, AllAgents: true, TeamsURL: "https://x"})
	n.startupOffline([]string{"a1", "a2"})
	*now = now.Add(20 * time.Second)
	n.agentChanged(ag("a1", true))
	*now = now.Add(5 * time.Minute)
	n.tick()
	m := sl.take()
	if len(m) != 1 || m[0].Title != "Network Monitor: HRD-SMILJAM nije na vezi" {
		t.Fatalf("posle restarta: %+v", m)
	}

	// Uklonjen agent se više ne prati.
	n.agentChanged(ag("a3", false))
	n.forget("a3")
	*now = now.Add(10 * time.Minute)
	n.tick()
	if m := sl.take(); len(m) != 0 {
		t.Fatalf("uklonjen agent: %+v", m)
	}
}

func TestNotifyConfigValidate(t *testing.T) {
	bad := []NotifyConfig{
		{EmailHost: "mail", EmailFrom: "netmon@firma.local", Channels: &NotifyChannels{Email: ChannelOpts{On: true}}}, // bez primaoca
		{Channels: &NotifyChannels{SMS: ChannelOpts{On: true}}},                                                       // SMS bez SMTP-a
		{Channels: &NotifyChannels{Teams: ChannelOpts{On: true}}},                                                     // Teams bez adrese
		{EmailHost: "mail", EmailFrom: "x", EmailTo: "a@b.c"},                                                         // neispravan pošiljalac
		{EmailHost: "mail", EmailFrom: "n@f.l", EmailTo: "a@b.c", EmailUser: "u", EmailTLS: "off"},                    // lozinka bez TLS-a
		{TeamsURL: "http://nije-https"},                                                                               //
		{NtfyURL: "ntfy.sh/tema"},                                                                                     // bez šeme
		{EmailHost: "mail", EmailFrom: "n@f.l", EmailTo: "a@b.c", EmailPort: 70000},                                   //
		{DelayMinutes: 5000, TeamsURL: "https://x"},                                                                   //
	}
	for i, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("%d: očekivana greška za %+v", i, c)
		}
	}
	ok := NotifyConfig{Enabled: true, EmailHost: "mail.firma.local", EmailFrom: "Network Monitor <netmon@firma.local>",
		EmailTo: "a@firma.local, b@firma.local", NtfyURL: "http://ntfy.firma.local/netmon"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	if ok.EmailPort != 25 {
		t.Fatalf("podrazumevani port: %d", ok.EmailPort)
	}
}

// fakeSMTP je minimalni SMTP server (bez TLS-a) koji pamti primljenu poruku.
func fakeSMTP(t *testing.T) (port int, got chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { io.WriteString(c, s+"\r\n") }
		say("220 test")
		var rcpt []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-test")
				say("250 8BITMIME")
			case strings.HasPrefix(cmd, "RCPT"):
				rcpt = append(rcpt, strings.TrimSpace(line))
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				got <- strings.Join(rcpt, "\n") + "\n" + b.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestSendChannels(t *testing.T) {
	port, gotMail := fakeSMTP(t)

	var teams map[string]any
	var ntfyTitle, ntfyBody, ntfyPrio, ntfyAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /teams", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&teams)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /netmon", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ntfyTitle, ntfyBody = r.URL.Query().Get("title"), string(b)
		ntfyPrio, ntfyAuth = r.Header.Get("Priority"), r.Header.Get("Authorization")
	})
	mux.HandleFunc("POST /bad", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nema teme", http.StatusForbidden)
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	cfg := NotifyConfig{
		EmailHost: "127.0.0.1", EmailPort: port, EmailFrom: "Network Monitor <netmon@firma.local>",
		EmailTo: "admin@firma.local, dezurni@firma.local", TeamsURL: hs.URL + "/teams",
		NtfyURL: hs.URL + "/netmon", NtfyToken: "tk_abc", Enabled: true,
	}
	cfg.normalize()
	m := notifyMessage{Title: "Network Monitor: INF-MILOSM nije na vezi", Body: "• INF-MILOSM (Dolina) – nije na vezi od 09:00\n", Down: true, Scope: scopeAgents}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := sendAll(ctx, cfg, m)
	for _, r := range res {
		if !r.OK {
			t.Fatalf("%s: %s", r.Channel, r.Error)
		}
	}
	if len(res) != 3 {
		t.Fatalf("kanala: %d", len(res))
	}

	raw := <-gotMail
	if !strings.Contains(raw, "<admin@firma.local>") || !strings.Contains(raw, "<dezurni@firma.local>") {
		t.Fatalf("primaoci:\n%s", raw)
	}
	if !strings.Contains(raw, "Subject: Network Monitor: INF-MILOSM nije na vezi\r\n") || !strings.Contains(raw, "Importance: high") {
		t.Fatalf("zaglavlja:\n%s", raw)
	}
	_, body, _ := strings.Cut(raw, "\r\n\r\n")
	dec, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if !strings.Contains(string(dec), "INF-MILOSM (Dolina)") {
		t.Fatalf("telo e-maila: %q", dec)
	}

	card, _ := json.Marshal(teams)
	if !strings.Contains(string(card), "AdaptiveCard") || !strings.Contains(string(card), "INF-MILOSM nije na vezi") {
		t.Fatalf("Teams: %s", card)
	}
	if ntfyTitle != m.Title || ntfyBody != m.Body || ntfyPrio != "high" || ntfyAuth != "Bearer tk_abc" {
		t.Fatalf("ntfy: %q %q %q %q", ntfyTitle, ntfyBody, ntfyPrio, ntfyAuth)
	}

	// Greška servera se vraća sa tekstom odgovora.
	m.Channel = chNtfy // proba jednog kanala: bez obzira na prekidače
	res = sendAll(ctx, NotifyConfig{NtfyURL: hs.URL + "/bad"}, m)
	if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Error, "403") || !strings.Contains(res[0].Error, "nema teme") {
		t.Fatalf("očekivana greška: %+v", res)
	}
}

func TestNotifyAPI(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	const ip = "10.0.0.5:1"
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	adm := []string{"admin", "admin-lozinka"}

	w := do(h, "POST", "/api/notify", ip, map[string]any{
		"enabled": true, "delay_minutes": 3, "channels": map[string]any{"email": map[string]bool{"on": true, "agents": true, "net": true}}, "email_host": "mail.firma.local", "email_from": "netmon@firma.local",
		"email_to": "admin@firma.local", "email_user": "netmon", "email_password": "tajna-lozinka",
	}, adm...)
	if w.Code != http.StatusOK {
		t.Fatalf("snimanje: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "tajna-lozinka") || !strings.Contains(w.Body.String(), `"email_password_set":true`) {
		t.Fatalf("lozinka ne sme da se vraća: %s", w.Body)
	}
	// Snimanje bez lozinke zadržava postojeću.
	w = do(h, "POST", "/api/notify", ip, map[string]any{
		"enabled": true, "email_host": "mail.firma.local", "email_from": "netmon@firma.local",
		"email_to": "admin@firma.local", "email_user": "netmon",
	}, adm...)
	if w.Code != http.StatusOK || s.notify.config().EmailPassword != "tajna-lozinka" {
		t.Fatalf("lozinka je izgubljena: %d %s", w.Code, w.Body)
	}
	if s.cfg.File.Notify == nil || s.cfg.File.Notify.EmailPassword != "tajna-lozinka" {
		t.Fatal("podešavanja nisu snimljena u console.json")
	}
	// Neispravno se odbija.
	if nc := s.notify.config(); !nc.sends(chEmail, scopeAgents) {
		t.Fatal("e-mail kanal treba da je uključen")
	}
	if w := do(h, "POST", "/api/notify", ip, map[string]any{"enabled": true, "channels": map[string]any{"teams": map[string]bool{"on": true}}}, adm...); w.Code != http.StatusBadRequest {
		t.Fatalf("očekivano 400, dobijeno %d", w.Code)
	}
	// Označavanje nepoznatog agenta.
	if w := do(h, "POST", "/api/agents/nema/watch", ip, map[string]any{"watch": true}, adm...); w.Code != http.StatusNotFound {
		t.Fatalf("nepoznat agent: %d", w.Code)
	}

	// Operater nema pristup.
	do(h, "POST", "/api/users", ip, userRequest{Name: "pera", Password: "pera-lozinka", Role: RoleOperator}, adm...)
	op := []string{"pera", "pera-lozinka"}
	for _, c := range []struct{ method, path string }{{"GET", "/api/notify"}, {"POST", "/api/notify"}, {"POST", "/api/notify/test"}, {"POST", "/api/agents/x/watch"}} {
		if w := do(h, c.method, c.path, ip, map[string]any{}, op...); w.Code != http.StatusForbidden {
			t.Fatalf("operater %s %s: %d", c.method, c.path, w.Code)
		}
	}
}
