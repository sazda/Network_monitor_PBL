package console

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sazda/network-monitor/internal/nettest"
	"github.com/sazda/network-monitor/internal/protocol"
)

func TestNetdevAPI(t *testing.T) {
	s, fc := newTestServer(t)
	h := s.Handler()
	const ip = "10.0.0.5:1"
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	adm := []string{"admin", "admin-lozinka"}
	do(h, "POST", "/api/users", ip, userRequest{Name: "pera", Password: "pera-lozinka", Role: RoleOperator}, adm...)
	op := []string{"pera", "pera-lozinka"}
	do(h, "POST", "/api/locations", ip, map[string]string{"text": "Glavni Centar = 10.0\nSever = 10.24"}, adm...)

	s.net.mu.Lock()
	s.net.pingFn = func(context.Context, string) (float64, int, int) { return math.NaN(), 2, 0 }
	s.net.mu.Unlock()

	dev := NetDevice{Name: "HQ_DC_S-I", IP: "10.0.35.15", Type: "switch", Place: "Server sala", Active: true, SNMP: true, Notify: true}
	if w := do(h, "POST", "/api/netdev", ip, dev, op...); w.Code != http.StatusForbidden {
		t.Fatalf("operater ne sme da dodaje: %d", w.Code)
	}
	w := do(h, "POST", "/api/netdev", ip, dev, adm...)
	if w.Code != http.StatusOK {
		t.Fatalf("dodavanje: %d %s", w.Code, w.Body)
	}
	var created deviceView
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == "" || created.LocationName != "Glavni Centar" || created.Status != stUnknown {
		t.Fatalf("dodat: %+v", created)
	}
	if w := do(h, "POST", "/api/netdev", ip, dev, adm...); w.Code != http.StatusConflict {
		t.Fatalf("ista adresa dva puta: %d", w.Code)
	}
	bad := dev
	bad.IP = "10.0.35"
	if w := do(h, "POST", "/api/netdev", ip, bad, adm...); w.Code != http.StatusBadRequest {
		t.Fatalf("neispravna adresa: %d", w.Code)
	}

	// Uvoz: jedan novi, jedan postojeći (izmena), jedna greška.
	csv := "Lokacija;Mesto;Naziv;IP;Tip;Uloga;Aktivan\n" +
		"Sever;Server Sala;RK00SW00;10.24.35.1;switch;;da\n" +
		";Server sala;HQ_DC_S-I;10.0.35.15;switch;;da\n" +
		"Dolina;;X;nije-adresa;switch;;\n"
	w = do(h, "POST", "/api/netdev/import", ip, map[string]string{"text": csv}, adm...)
	var ir importResult
	json.Unmarshal(w.Body.Bytes(), &ir)
	if w.Code != http.StatusOK || ir.Added != 1 || ir.Updated != 1 || len(ir.Errors) != 1 {
		t.Fatalf("uvoz: %d %s", w.Code, w.Body)
	}

	// Operater vidi spisak i stanje lokacija.
	w = do(h, "GET", "/api/netdev", ip, nil, op...)
	var ov netdevResponse
	json.Unmarshal(w.Body.Bytes(), &ov)
	if w.Code != http.StatusOK || len(ov.Devices) != 2 || len(ov.Locations) != 2 || ov.SNMPConfigured {
		t.Fatalf("spisak: %d %s", w.Code, w.Body)
	}
	for _, d := range ov.Devices {
		if d.Name == "RK00SW00" && d.LocationName != "Sever" {
			t.Fatalf("lokacija iz uvoza: %+v", d)
		}
	}

	// Ping uređaja sa konzole: rezultat u istoriji pod nazivom uređaja.
	s.icmpPing = func(ctx context.Context, host string, count int, interval, timeout time.Duration, onProbe nettest.ProbeFunc) (protocol.LatencyResult, error) {
		res := protocol.LatencyResult{Target: host, Method: "icmp"}
		for i := 1; i <= count; i++ {
			res.Sent++
			res.Received++
			res.Samples = append(res.Samples, 1)
			onProbe(i, 1, true)
		}
		nettest.FillLatencyStats(&res)
		return res, nil
	}
	w = do(h, "POST", "/api/netdev/"+created.ID+"/ping", ip, PingRequest{DurationSec: 5, IntervalMS: 1000}, op...)
	if w.Code != http.StatusAccepted {
		t.Fatalf("ping: %d %s", w.Code, w.Body)
	}
	var st struct {
		ResultID string `json:"result_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	var res Result
	for i := 0; i < 100; i++ {
		if r, ok := s.store.Get(st.ResultID); ok {
			res = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if res.Hostname != "HQ_DC_S-I" || res.AgentID != netdevPrefix+created.ID || !res.OK {
		t.Fatalf("rezultat pinga: %+v", res)
	}
	if w := do(h, "GET", "/api/results/"+res.ID+"/report.html", ip, nil, op...); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "konzola → HQ_DC_S-I (10.0.35.15)") {
		t.Fatalf("izveštaj: %d", w.Code)
	}

	// Istorija odziva.
	s.net.pingRound()
	w = do(h, "GET", "/api/netdev/"+created.ID+"/history?series=rtt&range=1h", ip, nil, op...)
	var hist struct {
		Points [][]any `json:"points"`
	}
	json.Unmarshal(w.Body.Bytes(), &hist)
	if w.Code != http.StatusOK || len(hist.Points) == 0 || hist.Points[len(hist.Points)-1][1] != nil {
		t.Fatalf("istorija: %d %s", w.Code, w.Body)
	}
	// Proizvoljan period i izvoz u Excel.
	now := time.Now().Unix()
	w = do(h, "GET", fmt.Sprintf("/api/netdev/%s/history?series=rtt&range=custom&from=%d&to=%d", created.ID, now-600, now+60), ip, nil, op...)
	json.Unmarshal(w.Body.Bytes(), &hist)
	if w.Code != http.StatusOK || len(hist.Points) == 0 {
		t.Fatalf("proizvoljan period: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", fmt.Sprintf("/api/netdev/%s/history?series=rtt&range=custom&from=%d&to=%d", created.ID, now, now-600), ip, nil, op...); w.Code != http.StatusBadRequest {
		t.Fatalf("obrnut period: %d", w.Code)
	}
	w = do(h, "GET", "/api/netdev/"+created.ID+"/history?series=rtt&range=15m&format=xlsx", ip, nil, op...)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), `HQ_DC_S-I-odziv-`) {
		t.Fatalf("excel: %d %v", w.Code, w.Header())
	}
	sheet := xlsxSheet(t, w.Body.Bytes())
	if !strings.Contains(sheet, "HQ_DC_S-I – odziv") || !strings.Contains(sheet, "Odziv (ms)") || !strings.Contains(sheet, `s="1"`) {
		t.Fatalf("list: %s", sheet)
	}

	// SNMP podešavanja: lozinke se ne vraćaju, snimaju se u console.json.
	snmp := map[string]any{"version": "v3", "user": "netmon", "auth_proto": "SHA", "priv_proto": "AES",
		"auth_pass": "tajna-lozinka-1", "priv_pass": "tajna-lozinka-2"}
	if w := do(h, "POST", "/api/snmp", ip, snmp, op...); w.Code != http.StatusForbidden {
		t.Fatalf("operater i SNMP: %d", w.Code)
	}
	w = do(h, "POST", "/api/snmp", ip, snmp, adm...)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "tajna") || !strings.Contains(w.Body.String(), `"auth_pass_set":true`) {
		t.Fatalf("SNMP: %d %s", w.Code, w.Body)
	}
	if fc.SNMP == nil || fc.SNMP.PrivPass != "tajna-lozinka-2" {
		t.Fatalf("SNMP nije snimljen: %+v", fc.SNMP)
	}
	// Izmena bez lozinki ih zadržava.
	snmp = map[string]any{"version": "v3", "user": "netmon2", "auth_proto": "SHA", "priv_proto": "AES"}
	if w := do(h, "POST", "/api/snmp", ip, snmp, adm...); w.Code != http.StatusOK || fc.SNMP.AuthPass != "tajna-lozinka-1" || fc.SNMP.User != "netmon2" {
		t.Fatalf("izmena SNMP-a: %d %+v", w.Code, fc.SNMP)
	}

	// Izbor uplink-a: samo administrator; izmena uređaja ga ne briše.
	if w := do(h, "POST", "/api/netdev/"+created.ID+"/uplink", ip, map[string]string{"port": "Te1/1/1"}, op...); w.Code != http.StatusForbidden {
		t.Fatalf("operater i uplink: %d", w.Code)
	}
	if w := do(h, "POST", "/api/netdev/"+created.ID+"/uplink", ip, map[string]string{"port": "Te1/1/1"}, adm...); w.Code != http.StatusOK {
		t.Fatalf("uplink: %d %s", w.Code, w.Body)
	}
	dev.Note = "izmena"
	do(h, "POST", "/api/netdev/"+created.ID, ip, dev, adm...)
	if d, _ := s.net.find(created.ID); d.Uplink != "Te1/1/1" || d.Note != "izmena" {
		t.Fatalf("uplink posle izmene: %+v", d)
	}

	// Brisanje.
	if w := doDelete(h, "/api/netdev/"+created.ID, op...); w.Code != http.StatusForbidden {
		t.Fatalf("operater briše: %d", w.Code)
	}
	if w := doDelete(h, "/api/netdev/"+created.ID, adm...); w.Code != http.StatusNoContent {
		t.Fatalf("brisanje: %d", w.Code)
	}
	if _, ok := s.net.find(created.ID); ok {
		t.Fatal("uređaj nije obrisan")
	}
	s.Close(time.Second)
}
