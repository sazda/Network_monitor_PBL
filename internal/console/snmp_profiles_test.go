package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gosnmp/gosnmp"
)

func TestSNMPProfileForDevice(t *testing.T) {
	c := SNMPConfig{Version: "v3", User: "netmon", AuthPass: "aaaaaaaa", PrivPass: "bbbbbbbb", Port: 161, Interval: 60, Timeout: 3,
		Profiles: []SNMPProfile{{Name: "operater CE ruteri", Version: "v2c", Community: "ce-tajna"}}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	d := c.forDevice("operater CE ruteri")
	if d.Version != "v2c" || d.Community != "ce-tajna" || d.User != "" || d.Port != 161 || d.Profiles != nil {
		t.Fatalf("CE nalog: %+v", d)
	}
	if d := c.forDevice(""); d.Version != "v3" || d.User != "netmon" {
		t.Fatalf("podrazumevani: %+v", d)
	}
	if d := c.forDevice("nepostojeći"); d.Version != "v3" {
		t.Fatalf("nepoznat nalog: %+v", d)
	}
	bad := c
	bad.Profiles = []SNMPProfile{{Name: "A", Version: "v2c", Community: "x"}, {Name: "a", Version: "v2c", Community: "y"}}
	if err := bad.validate(); err == nil {
		t.Fatal("dupli naziv")
	}
	bad.Profiles = []SNMPProfile{{Name: "A", Version: "v2c"}}
	if err := bad.validate(); err == nil || !strings.Contains(err.Error(), `"A"`) {
		t.Fatalf("v2c bez community-ja: %v", err)
	}
}

func TestSNMPProfilesAPI(t *testing.T) {
	s, fc := newTestServer(t)
	h := s.Handler()
	ip := "10.0.0.5:1"
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	adm := []string{"admin", "admin-lozinka"}

	base := map[string]any{"version": "v3", "user": "netmon", "auth_proto": "SHA", "priv_proto": "AES",
		"auth_pass": "tajna-lozinka-1", "priv_pass": "tajna-lozinka-2",
		"profiles": []map[string]any{{"name": "operater CE ruteri", "version": "v2c", "community": "ce-community-1"}}}
	w := do(h, "POST", "/api/snmp", ip, base, adm...)
	if w.Code != 200 || strings.Contains(w.Body.String(), "ce-community-1") || !strings.Contains(w.Body.String(), `"community_set":true`) {
		t.Fatalf("snimanje: %d %s", w.Code, w.Body)
	}
	if len(fc.SNMP.Profiles) != 1 || fc.SNMP.Profiles[0].Community != "ce-community-1" {
		t.Fatalf("console.json: %+v", fc.SNMP)
	}
	// Uređaj sa nepoznatim pa sa postojećim nalogom.
	dev := NetDevice{Name: "Firma_optika_FIL", IP: "10.11.21.2", Type: "router", Active: true, SNMP: true, SNMPProfile: "Nepostojeći"}
	if w := do(h, "POST", "/api/netdev", ip, dev, adm...); w.Code != http.StatusBadRequest {
		t.Fatalf("nepoznat nalog: %d", w.Code)
	}
	dev.SNMPProfile = "operater CE ruteri"
	w = do(h, "POST", "/api/netdev", ip, dev, adm...)
	var created deviceView
	json.Unmarshal(w.Body.Bytes(), &created)
	if w.Code != 200 || created.SNMPProfile != "operater CE ruteri" {
		t.Fatalf("uređaj: %d %s", w.Code, w.Body)
	}
	// Bez community-ja: ostaje stari; bez spiska naloga: nalozi ostaju.
	keep := map[string]any{"version": "v3", "user": "netmon", "auth_proto": "SHA", "priv_proto": "AES",
		"profiles": []map[string]any{{"name": "operater CE ruteri", "version": "v2c"}}}
	if w := do(h, "POST", "/api/snmp", ip, keep, adm...); w.Code != 200 || fc.SNMP.Profiles[0].Community != "ce-community-1" {
		t.Fatalf("bez promene community-ja: %d %+v", w.Code, fc.SNMP.Profiles)
	}
	delete(keep, "profiles")
	if w := do(h, "POST", "/api/snmp", ip, keep, adm...); w.Code != 200 || len(fc.SNMP.Profiles) != 1 {
		t.Fatalf("izostavljen spisak: %d %+v", w.Code, fc.SNMP.Profiles)
	}
	// Nalog koji koristi uređaj ne može da se ukloni.
	keep["profiles"] = []map[string]any{}
	if w := do(h, "POST", "/api/snmp", ip, keep, adm...); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Firma_optika_FIL") {
		t.Fatalf("uklanjanje korišćenog naloga: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/api/snmp", ip, nil, adm...)
	if !strings.Contains(w.Body.String(), `"devices":1`) {
		t.Fatalf("broj uređaja: %s", w.Body)
	}

	// Čitanje uređaja koristi njegov nalog.
	var mu sync.Mutex
	var used []SNMPConfig
	s.net.mu.Lock()
	s.net.dialSNMP = func(_ context.Context, c SNMPConfig, _ string) (snmpClient, error) {
		mu.Lock()
		used = append(used, c)
		mu.Unlock()
		return &fakeSNMP{}, nil
	}
	s.net.rt(created.ID).status = stUp
	s.net.mu.Unlock()
	s.net.snmpRound()
	mu.Lock()
	defer mu.Unlock()
	if len(used) == 0 || used[0].Version != "v2c" || used[0].Community != "ce-community-1" {
		t.Fatalf("nalog pri čitanju: %+v", used)
	}
}

func TestSNMPNotAuthenticText(t *testing.T) {
	if got := snmpErrorText(errString("incoming packet is not authentic, discarding")); !strings.Contains(got, "ne prihvata SNMP podatke") {
		t.Fatal(got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Za rutere se MAC tabela ne čita (nema lažnog upozorenja); susedi da.
func TestRouterSkipsFDB(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{Name: "Firma_optika_FIL", IP: "10.11.21.2", Type: "router", Active: true, SNMP: true}}, nil)
	dev := &fakeSNMP{uptime: 100}
	var walked []string
	var mu sync.Mutex
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) {
		return walkSpy{dev, func(o string) { mu.Lock(); walked = append(walked, o); mu.Unlock() }}, nil
	}
	m.pollSNMP(SNMPConfig{Version: "v2c", Community: "x"}, m.devices[0])
	v, _ := m.device("Firma_optika_FIL")
	if v.SNMPInfo == nil || !v.SNMPInfo.OK || v.SNMPInfo.FDBError != "" {
		t.Fatalf("ruter: %+v", v.SNMPInfo)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, o := range walked {
		if strings.HasPrefix(o, oidQFdbPort) || strings.HasPrefix(o, oidFdbPort) {
			t.Fatalf("ruter: čitana MAC tabela (%s)", o)
		}
	}
}

type walkSpy struct {
	snmpClient
	on func(string)
}

func (w walkSpy) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	w.on(oid)
	return w.snmpClient.BulkWalkAll(oid)
}
