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

// Posle restarta konzole uređaj koji je bio dole i za koji je poslato
// obaveštenje javlja povratak; lokacija na rezervnoj vezi javlja oporavak.
func TestRecoveryAfterConsoleRestart(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var sms []string
	newMon := func() *netMonitor {
		n := newNotifier(NotifyConfig{Enabled: true, EmailHost: "mail", EmailFrom: "a@b.rs", SMSEmail: "netmon@b.rs"},
			log.New(io.Discard, "", 0), nil)
		n.sendSMS = func(_ context.Context, _ NotifyConfig, text string) error {
			mu.Lock()
			sms = append(sms, text)
			mu.Unlock()
			return nil
		}
		m := newNetMonitor(dir, nil, log.New(io.Discard, "", 0), n, func(string) string { return "Sever" })
		return m
	}
	waitSMS := func(n int) []string {
		for i := 0; i < 200; i++ {
			mu.Lock()
			k := len(sms)
			mu.Unlock()
			if k >= n {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sms...)
	}
	devs := []NetDevice{
		{ID: "wan", Name: "MPLS", IP: "10.98.0.50", Role: "wan", Active: true, Notify: true},
		{ID: "sw", Name: "RK00SW00", IP: "10.24.35.1", Active: true, Notify: true},
		{ID: "acc", Name: "RK01SW01", IP: "10.24.35.2", Active: true, Notify: true},
	}
	m := newMon()
	if err := m.setDevices(append([]NetDevice(nil), devs...)); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	all := map[string]bool{"MPLS": true, "RK00SW00": true, "RK01SW01": true}
	round(m, t0, all)
	for i := 1; i <= 3; i++ {
		round(m, t0.Add(time.Duration(i)*30*time.Second), map[string]bool{"RK01SW01": true})
	}
	got := waitSMS(1)
	if len(got) != 1 || !strings.Contains(got[0], "MPLS ne radi") || !strings.Contains(got[0], "PAD RK00SW00") {
		t.Fatalf("pre restarta: %q", got)
	}
	m.hist.Close()

	// Restart: novo stanje iz fajla.
	m2 := newMon()
	defer m2.hist.Close()
	if st := m2.rt("sw"); st.status != stDown || !st.alerted {
		t.Fatalf("stanje posle restarta: %+v", st)
	}
	round(m2, t0.Add(5*time.Minute), all)
	got = waitSMS(2)
	if len(got) != 2 || !strings.Contains(got[1], "OK RK00SW00") || !strings.Contains(got[1], "MPLS) veza ponovo radi") {
		t.Fatalf("posle restarta: %q", got)
	}
	// Dolina više nije dole: fajl je prazan.
	m2.mu.Lock()
	saved := string(m2.stateSaved)
	m2.mu.Unlock()
	if saved != "{}" {
		t.Fatalf("stanje: %s", saved)
	}
}
