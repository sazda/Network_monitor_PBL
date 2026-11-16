package console

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sazda/network-monitor/internal/protocol"
)

const testLocations = `
# komentar
Centrala = 10.0
VPN = 10.0.128
Jug: 10.18.
Sever = 10.1, 10.24.0.0/16
Kombinovana = 10.11.11; 10.11.3
`

func TestParseLocations(t *testing.T) {
	locs, err := ParseLocationsText(testLocations)
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 5 || locs[1].Name != "VPN" || locs[1].Prefixes[0] != "10.0.128.0/24" || locs[0].Prefixes[0] != "10.0.0.0/16" {
		t.Fatalf("pogrešno parsirano: %+v", locs)
	}
	// Tekst se vraća u čitljivom obliku i ponovo parsira u isto.
	again, err := ParseLocationsText(FormatLocationsText(locs))
	if err != nil || len(again) != len(locs) || again[3].Prefixes[1] != "10.24.0.0/16" {
		t.Fatalf("format/parse: %v %+v", err, again)
	}
	for _, bad := range []string{"bez znaka jednakosti", "X = 10.300", "X =", "X = 10.0\nx = 10.1", "= 10.0"} {
		if _, err := ParseLocationsText(bad); err == nil {
			t.Errorf("očekivana greška za %q", bad)
		}
	}
}

func TestLocate(t *testing.T) {
	locs, _ := ParseLocationsText(testLocations)
	l, err := newLocator(locs)
	if err != nil {
		t.Fatal(err)
	}
	agent := func(remote string, addrs ...string) AgentInfo {
		return AgentInfo{RemoteAddr: remote, Hello: protocol.Hello{Interfaces: []protocol.Interface{{Name: "eth", Addrs: addrs}}}}
	}
	cases := []struct {
		a    AgentInfo
		want string
	}{
		{agent("10.0.20.171:1"), "Centrala"},
		{agent("10.0.128.15:1"), "VPN"}, // uži opseg pobeđuje
		{agent("10.18.21.5:1"), "Jug"},  // 10.1 se ne meša sa 10.18
		{agent("10.1.2.3:1"), "Sever"},
		{agent("10.11.3.7:1"), "Kombinovana"},
		{agent("10.11.4.7:1"), UnknownLocation},
		{agent("192.168.1.5:1", "192.168.1.5/24", "10.24.5.5/24"), "Sever"}, // rezerva: adrese interfejsa
		{agent("[::ffff:10.18.0.1]:1"), "Jug"},
	}
	for _, c := range cases {
		if got := l.locate(c.a); got != c.want {
			t.Errorf("%s: %q, očekivano %q", c.a.RemoteAddr, got, c.want)
		}
	}
}

func TestLocationsAPI(t *testing.T) {
	s, fc := newTestServer(t)
	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	do(h, "POST", "/api/users", "10.0.0.5:1", userRequest{Name: "op", Password: "op-lozinka", Role: RoleOperator}, "admin", "admin-lozinka")

	body := map[string]string{"text": testLocations}
	if w := do(h, "POST", "/api/locations", "10.0.0.5:1", body, "op", "op-lozinka"); w.Code != http.StatusForbidden {
		t.Fatalf("operater menja lokacije: %d", w.Code)
	}
	if w := do(h, "POST", "/api/locations", "10.0.0.5:1", map[string]string{"text": "X = 10.999"}, "admin", "admin-lozinka"); w.Code != 400 {
		t.Fatalf("neispravne lokacije prihvaćene: %d", w.Code)
	}
	if w := do(h, "POST", "/api/locations", "10.0.0.5:1", body, "admin", "admin-lozinka"); w.Code != 200 {
		t.Fatalf("snimanje lokacija: %d %s", w.Code, w.Body.String())
	}
	w := do(h, "GET", "/api/locations", "10.0.0.5:1", nil, "op", "op-lozinka")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"VPN"`) {
		t.Fatalf("čitanje lokacija: %d %s", w.Code, w.Body.String())
	}
	fc2, _ := LoadFileConfig(fc.Path())
	if len(fc2.Locations) != 5 {
		t.Fatalf("lokacije nisu snimljene: %+v", fc2.Locations)
	}
}
