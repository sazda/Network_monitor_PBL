package console

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

func TestParseReloadSyslog(t *testing.T) {
	for _, c := range []struct{ msg, user, line, from, reason string }{
		{`<189>123: Nov 13 11:49:58.123: %SYS-5-RELOAD: Reload requested by FIRMA\pera on vty0 (10.0.36.50). Reload Reason: Reload Command.`,
			`FIRMA\pera`, "vty0", "10.0.36.50", "Reload Command"},
		{`<189>45: *Mar  1 00:10:00: %SYS-5-RELOAD: Reload requested by console. Reload Reason: Reload Command.`, "", "", "", ""},
		{`<189>45: %SYS-5-RELOAD: Reload requested by admin on console. Reload Reason: Reload command.`, "admin", "console", "", "Reload command"},
	} {
		r, ok := parseReloadSyslog(c.msg)
		if c.user == "" {
			if ok && r.User != "" && r.Line != "" {
				t.Errorf("%q: %+v", c.msg, r)
			}
			continue
		}
		if !ok || r.User != c.user || r.Line != c.line || r.From != c.from || r.Reason != c.reason {
			t.Errorf("%q: %+v %v", c.msg, r, ok)
		}
	}
	if _, ok := parseReloadSyslog("%LINK-3-UPDOWN: Interface Gi1/0/3, changed state to up"); ok {
		t.Error("druga poruka")
	}
	for in, want := range map[string]string{"power-on": "nestanak ili uključenje napajanja", "PowerOn": "nestanak ili uključenje napajanja",
		"Reload Command": "softverski restart (reload)", "Image Install": "restart posle instalacije softvera", "": ""} {
		if got := reloadReasonText(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// whySwitch: fakeSNMP koji daje i razlog restarta.
type whySwitch struct {
	*fakeSNMP
	why string
}

func (c whySwitch) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	if len(oids) == 1 && oids[0] == oidWhyReload {
		return []gosnmp.SnmpPDU{{Name: oidWhyReload, Type: gosnmp.OctetString, Value: []byte(c.why)}}, nil
	}
	return c.fakeSNMP.Get(oids)
}

func TestRebootReasonAndWho(t *testing.T) {
	m, sent := testMonitor(t, []NetDevice{{Name: "HQ_UZ_S-I", IP: "10.0.35.21", Type: "switch", Active: true, SNMP: true, Notify: true}}, nil)
	dev := &fakeSNMP{uptime: 5_000_000}
	why := "power-on" // C2960L posle reload-a javlja power-on
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) { return whySwitch{dev, why}, nil }
	now := time.Date(2026, 11, 13, 11, 40, 0, 0, time.Local)
	m.now = func() time.Time { return now }
	m.rt("HQ_UZ_S-I").status = stUp
	cfg := SNMPConfig{Version: "v2c", Community: "x"}
	m.pollSNMP(cfg, m.devices[0])

	// Syslog sa nepoznate adrese se ne prihvata; sa adrese switch-a da.
	m.syslogIn("10.9.9.9", `%SYS-5-RELOAD: Reload requested by uljez on vty0 (1.2.3.4). Reload Reason: Reload Command.`, now)
	m.syslogIn("10.0.35.21", `<189>12: %SYS-5-RELOAD: Reload requested by FIRMA\pera on vty0 (10.0.36.50). Reload Reason: Reload Command.`, now)
	now = now.Add(10 * time.Minute)
	dev.uptime = 6000 // 60 s
	m.pollSNMP(cfg, m.devices[0])
	evs := fmtEvents(m.recentEvents("", 10))
	if strings.Contains(evs, "uljez") || !strings.Contains(evs, `reload_req HQ_UZ_S-I: restart je zatražio FIRMA\pera sa 10.0.36.50`) ||
		!strings.Contains(evs, `reboot HQ_UZ_S-I je ponovo pokrenut (radi 1 min) – restart, pokrenuo FIRMA\pera sa 10.0.36.50`) {
		t.Fatalf("događaji:\n%s", evs)
	}
	msgs := waitSent(t, sent, 1)
	if !strings.Contains(msgs[0].SMS, `RESTART HQ_UZ_S-I (Bez lokacije) - restart, pokrenuo FIRMA\pera sa 10.0.36.50`) {
		t.Fatalf("SMS: %q", msgs[0].SMS)
	}

	// Nestanak struje: bez syslog poruke, razlog iz SNMP-a.
	why = "power-on"
	now = now.Add(time.Hour)
	dev.uptime = 9000
	m.pollSNMP(cfg, m.devices[0])
	now = now.Add(time.Minute)
	dev.uptime = 3000
	m.pollSNMP(cfg, m.devices[0])
	if evs := fmtEvents(m.recentEvents("", 1)); !strings.Contains(evs, "– nestanak ili uključenje napajanja\n") {
		t.Fatalf("struja: %s", evs)
	}
	// C2960L: „power-on” i posle reload-a – bez syslog poruke uzrok je nepoznat.
	if got := reloadSuffix("power-on", "WS-C2960L-8PS-LL", nil); got != " – uzrok nepoznat (nestanak struje ili reload)" {
		t.Fatalf("2960L: %q", got)
	}
	if got := reloadSuffix("power-on", "WS-C2960L-8PS-LL", &reloadRequest{User: "mika", From: "10.0.23.22"}); got != " – restart, pokrenuo mika sa 10.0.23.22" {
		t.Fatalf("2960L sa syslog-om: %q", got)
	}
	if got := reloadSuffix("Reload Command", "C9200L-48P-4X", nil); got != " – softverski restart (reload)" {
		t.Fatalf("9200L: %q", got)
	}
}

func TestSyslogListener(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{Name: "HQ_UZ_S-II", IP: "127.0.0.1", Type: "switch", Active: true}}, nil)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.syslogServe(pc); close(done) }()
	t.Cleanup(func() { m.stopOnce.Do(func() { close(m.stop) }); <-done })
	c, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("<189>7: %LINK-3-UPDOWN: Interface Gi1/0/3, changed state to up"))
	c.Write([]byte(`<189>8: %SYS-5-RELOAD: Reload requested by FIRMA\a on vty1 (10.0.36.51). Reload Reason: Reload Command.`))
	for i := 0; i < 200; i++ {
		if evs := fmtEvents(m.recentEvents("", 5)); strings.Contains(evs, `restart je zatražio FIRMA\a sa 10.0.36.51`) {
			if strings.Count(evs, "\n") != 1 {
				t.Fatalf("višak događaja: %s", evs)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("syslog poruka nije obrađena")
}
