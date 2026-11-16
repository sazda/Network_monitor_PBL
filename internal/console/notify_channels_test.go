package console

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeOldConfig(t *testing.T) {
	c := NotifyConfig{Enabled: true, EmailHost: "mail", EmailFrom: "a@b.rs", SMSEmail: "netmon@b.rs", NtfyURL: "https://n/x"}
	c.normalize()
	want := NotifyChannels{
		Email: ChannelOpts{On: false, Agents: true, Net: true}, // nema primalaca
		SMS:   ChannelOpts{On: true, Agents: false, Net: true},
		Teams: ChannelOpts{On: false, Agents: true, Net: true},
		Ntfy:  ChannelOpts{On: true, Agents: true, Net: true},
	}
	if *c.Channels != want {
		t.Fatalf("got %+v", *c.Channels)
	}
}

func TestDeliverRespectsSwitches(t *testing.T) {
	cfg := NotifyConfig{
		Enabled: true, EmailHost: "mail", EmailFrom: "a@b.rs", EmailTo: "x@b.rs", SMSEmail: "netmon@b.rs",
		NtfyURL: "https://n/x",
		Channels: &NotifyChannels{
			Email: ChannelOpts{On: true, Agents: true, Net: false},
			SMS:   ChannelOpts{On: true, Agents: false, Net: true},
			Ntfy:  ChannelOpts{On: false, Agents: true, Net: true},
		},
	}
	n := newNotifier(cfg, log.New(io.Discard, "", 0), nil)
	var mu sync.Mutex
	var got []string
	n.send = func(_ context.Context, c NotifyConfig, m notifyMessage) []channelResult {
		mu.Lock()
		defer mu.Unlock()
		res := sendAllWith(c, m, func(key string) error { got = append(got, m.Scope+":"+key); return nil })
		return res
	}
	n.sendSMS = func(_ context.Context, _ NotifyConfig, text string) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, "sms:"+text)
		return errors.New("nema veze")
	}
	ctx := context.Background()
	n.deliver(ctx, cfg, notifyMessage{Title: "Network Monitor: PC1 nije na vezi", Scope: scopeAgents})
	n.deliver(ctx, cfg, notifyMessage{Title: "x", Scope: scopeNet, SMS: "NetMon 09:00 PAD SW1"})
	if strings.Join(got, "|") != "agents:email|sms:NetMon 09:00 PAD SW1" {
		t.Fatalf("got %q", got)
	}
	st := n.currentStatus()
	if st.Channels[chEmail] == nil || st.Channels[chEmail].LastError != "" || st.Channels[chSMS].LastError != "nema veze" {
		t.Fatalf("stanje: %+v", st.Channels)
	}
	// Uspešan SMS briše staru grešku tog kanala.
	n.sendSMS = func(context.Context, NotifyConfig, string) error { return nil }
	n.deliver(ctx, cfg, notifyMessage{Scope: scopeNet, SMS: "ok"})
	if e := n.currentStatus().Channels[chSMS].LastError; e != "" {
		t.Fatalf("stara greška ostala: %q", e)
	}

	// Glavni prekidač isključuje sve.
	cfg.Enabled = false
	got = nil
	n.deliver(ctx, cfg, notifyMessage{Title: "x", Scope: scopeNet, SMS: "y"})
	n.deliver(ctx, cfg, notifyMessage{Title: "x", Scope: scopeAgents})
	if len(got) != 0 || cfg.anyFor(scopeNet) {
		t.Fatalf("glavni prekidač: %q", got)
	}
	// Proba jednog kanala radi i kada je isključen.
	n.deliver(ctx, cfg, notifyMessage{Title: "x", Channel: chNtfy})
	if len(got) != 1 || got[0] != ":ntfy" {
		t.Fatalf("proba: %q", got)
	}
}

// sendAllWith je sendAll sa lažnim slanjem (isti izbor kanala).
func sendAllWith(cfg NotifyConfig, m notifyMessage, send func(key string) error) []channelResult {
	var res []channelResult
	for _, key := range []string{chEmail, chTeams, chNtfy} {
		if m.Channel != "" && m.Channel != key || m.Channel == "" && !cfg.sends(key, m.Scope) {
			continue
		}
		res = append(res, newResult(key, send(key)))
	}
	return res
}

func TestNotifyTestChannelAPI(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	const ip = "10.0.0.5:1"
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	adm := []string{"admin", "admin-lozinka"}
	w := do(h, "POST", "/api/notify", ip, map[string]any{
		"enabled": false, "email_host": "mail.firma.local", "email_from": "netmon@firma.local", "sms_email": "netmon@firma.local",
		"channels": map[string]any{"sms": map[string]bool{"on": true, "net": true}},
	}, adm...)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sms":{"on":true,"agents":false,"net":true}`) {
		t.Fatalf("snimanje: %d %s", w.Code, w.Body)
	}
	var sent []string
	s.notify.sendSMS = func(_ context.Context, _ NotifyConfig, text string) error { sent = append(sent, text); return nil }
	w = do(h, "POST", "/api/notify/test", ip, map[string]string{"channel": "sms"}, adm...)
	if w.Code != http.StatusOK || len(sent) != 1 || !strings.HasPrefix(sent[0], "PROBA NetMon") || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("proba SMS: %d %s %q", w.Code, w.Body, sent)
	}
	if w := do(h, "POST", "/api/notify/test", ip, map[string]string{"channel": "teams"}, adm...); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "nije podešen") {
		t.Fatalf("Teams nije podešen: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/notify/test", ip, map[string]string{"channel": "x"}, adm...); w.Code != http.StatusBadRequest {
		t.Fatalf("nepoznat kanal: %d", w.Code)
	}
}
