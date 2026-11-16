package console

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSMSASCII(t *testing.T) {
	got := smsASCII("Čačak – Šabac, Đurđevo: „žuto” 😀")
	want := `Cacak - Sabac, Djurdjevo: "zuto" ?`
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestComposeNetSMS(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 12, 0, 0, time.Local)
	locs := []NetEvent{{Kind: "loc_backup", Location: "Sever"}}
	downs := []NetEvent{{Kind: "down", Device: "RK00SW00", Location: "Sever"}}
	got := composeNetSMS(downs, nil, locs, now)
	want := "NetMon 09:12 Sever: MPLS ne radi, radi preko backup veze; PAD RK00SW00 (Sever)"
	if got != want {
		t.Fatalf("got %q", got)
	}
	ups := []NetEvent{{Kind: "up", Device: "HQ_DC_S-I", Location: "Glavni Centar", DurationSec: 3900}}
	if got := composeNetSMS(nil, ups, nil, now); got != "NetMon 09:12 OK HQ_DC_S-I (Glavni Centar) posle 1 h 5 min" {
		t.Fatalf("up: %q", got)
	}
	loc := []NetEvent{{Kind: "loc_ok", Location: "Dolina", Text: "Lokacija Dolina: glavna (MPLS) veza ponovo radi (trajalo 12 min)"}}
	if got := composeNetSMS(nil, nil, loc, now); got != "NetMon 09:12 OK Dolina: glavna (MPLS) veza ponovo radi (trajalo 12 min)" {
		t.Fatalf("loc_ok: %q", got)
	}

	// Mnogo događaja: jedan SMS, ostatak kao „+N jos”.
	var many []NetEvent
	for i := 0; i < 20; i++ {
		many = append(many, NetEvent{Kind: "down", Device: "SWITCH-BROJ-" + string(rune('A'+i)), Location: "Jug"})
	}
	got = composeNetSMS(many, nil, nil, now)
	if len(got) > smsMaxLen || !strings.Contains(got, " jos") || !strings.HasPrefix(got, "NetMon 09:12 PAD SWITCH-BROJ-A") {
		t.Fatalf("mnogo: %d %q", len(got), got)
	}
	// Jedan predugačak događaj se skraćuje.
	long := []NetEvent{{Kind: "down", Device: strings.Repeat("X", 300)}}
	if got := composeNetSMS(long, nil, nil, now); len(got) != smsMaxLen || !strings.HasSuffix(got, "...") {
		t.Fatalf("dugačak: %d %q", len(got), got)
	}
	if composeNetSMS(nil, nil, nil, now) != "" {
		t.Fatal("prazno")
	}
}

func TestSMSCheckDue(t *testing.T) {
	cfg := NotifyConfig{Enabled: true, EmailHost: "mail", SMSEmail: "netmon@firma.rs", SMSCheckTime: "08:00"}
	cfg.normalize()
	at := func(h, m int) time.Time { return time.Date(2026, 11, 6, h, m, 0, 0, time.Local) }
	if _, ok := smsCheckDue(cfg, at(7, 59), ""); ok {
		t.Fatal("pre vremena")
	}
	day, ok := smsCheckDue(cfg, at(8, 0), "")
	if !ok || day != "2026-11-06" {
		t.Fatalf("u 08:00: %q %v", day, ok)
	}
	if _, ok := smsCheckDue(cfg, at(8, 3), day); ok {
		t.Fatal("dva puta istog dana")
	}
	if _, ok := smsCheckDue(cfg, at(9, 0), ""); ok {
		t.Fatal("konzola pokrenuta mnogo kasnije ne šalje")
	}
	cfg.SMSCheckTime = ""
	if _, ok := smsCheckDue(cfg, at(8, 0), ""); ok {
		t.Fatal("isključena provera")
	}
}

func TestNotifyValidateSMS(t *testing.T) {
	base := NotifyConfig{Enabled: true, EmailHost: "mail.firma.rs", EmailFrom: "netmon-konzola@firma.rs"}
	c := base
	c.SMSEmail = "netmon@firma.rs"
	c.SMSCheckTime = "08:00"
	if err := c.validate(); err != nil {
		t.Fatalf("samo SMS (bez običnih primalaca) treba da prođe: %v", err)
	}
	c.SMSCheckTime = "8h"
	if err := c.validate(); err == nil {
		t.Fatal("neispravno vreme")
	}
	c = base
	c.Channels = &NotifyChannels{Email: ChannelOpts{On: true}}
	if err := c.validate(); err == nil {
		t.Fatal("e-mail kanal bez primalaca")
	}
	c = NotifyConfig{Enabled: true, NtfyURL: "https://ntfy.example/x", SMSEmail: "netmon@firma.rs"}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "SMTP") {
		t.Fatalf("SMS bez SMTP-a: %v", err)
	}
}

func TestNetAlertSendsSMS(t *testing.T) {
	n := newNotifier(NotifyConfig{Enabled: true, EmailHost: "mail", EmailFrom: "a@b.rs", SMSEmail: "netmon@firma.rs"},
		log.New(io.Discard, "", 0), nil)
	var mu sync.Mutex
	var normal int
	var sms []string
	n.send = func(context.Context, NotifyConfig, notifyMessage) []channelResult {
		mu.Lock()
		normal++
		mu.Unlock()
		return nil
	}
	n.sendSMS = func(_ context.Context, _ NotifyConfig, text string) error {
		mu.Lock()
		sms = append(sms, text)
		mu.Unlock()
		return nil
	}
	m := newNetMonitor(t.TempDir(), nil, log.New(io.Discard, "", 0), n, func(string) string { return "Jug" })
	t.Cleanup(func() {
		if m.hist != nil {
			m.hist.Close()
		}
	})
	m.setDevices([]NetDevice{{ID: "sw", Name: "JG_SW1", IP: "10.9.0.2", Active: true, Notify: true}})
	t0 := time.Now()
	round(m, t0, map[string]bool{"JG_SW1": true})
	for i := 1; i <= 3; i++ {
		round(m, t0.Add(time.Duration(i)*30*time.Second), nil)
	}
	for i := 0; i < 100; i++ {
		mu.Lock()
		k := len(sms)
		mu.Unlock()
		if k > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sms) == 0 || !strings.Contains(sms[0], "PAD JG_SW1") {
		t.Fatalf("SMS: %q", sms)
	}
	if normal != 0 {
		t.Fatalf("bez običnih primalaca ne sme da ide običan e-mail: %d", normal)
	}
}

func TestSendSMSEmail(t *testing.T) {
	port, got := fakeSMTP(t)
	cfg := NotifyConfig{EmailHost: "127.0.0.1", EmailPort: port, EmailFrom: "Network Monitor <konzola@firma.local>",
		EmailTo: "admin@firma.local", SMSEmail: "netmon@firma.local"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sendSMSEmail(ctx, cfg, "NetMon 09:12 PAD JG_SW1 (Jug)"); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	if !strings.Contains(msg, "Subject: [NETMON-SMS] NetMon 09:12 PAD JG_SW1 (Jug)") ||
		!strings.Contains(msg, "To: <netmon@firma.local>") || strings.Contains(msg, "admin@firma.local") {
		t.Fatalf("poruka:\n%s", msg)
	}
}
