package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sazda/network-monitor/internal/nettest"
	"github.com/sazda/network-monitor/internal/protocol"
)

func TestPingFromConsole(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	const ip = "10.0.0.5:1"
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	adm := []string{"admin", "admin-lozinka"}
	do(h, "POST", "/api/users", ip, userRequest{Name: "pera", Password: "pera-lozinka", Role: RoleOperator}, adm...)
	op := []string{"pera", "pera-lozinka"}

	// Računar je poznat, ali agent nije na vezi: ping sa konzole i dalje radi.
	a := &AgentInfo{RemoteAddr: "10.12.21.20:50000"}
	a.AgentID, a.Hostname = "pc1", "FIN-SANDRAA"
	s.hub.restore(map[string]*AgentInfo{"pc1": a})

	var gotHost string
	s.icmpPing = func(ctx context.Context, host string, count int, interval, timeout time.Duration, onProbe nettest.ProbeFunc) (protocol.LatencyResult, error) {
		gotHost = host
		res := protocol.LatencyResult{Target: host, Method: "icmp"}
		for i := 1; i <= count; i++ {
			res.Sent++
			if i == 3 || i == 4 { // jedan prekid od 2 paketa
				res.Lost = append(res.Lost, i)
				onProbe(i, 0, false)
				continue
			}
			res.Received++
			res.Samples = append(res.Samples, float64(i))
			onProbe(i, float64(i), true)
		}
		nettest.FillLatencyStats(&res)
		return res, nil
	}

	w := do(h, "POST", "/api/agents/pc1/ping", ip, PingRequest{DurationSec: 5, IntervalMS: 500}, adm...)
	if w.Code != http.StatusAccepted {
		t.Fatalf("pokretanje: %d %s", w.Code, w.Body)
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
	if !res.OK || res.Kind != "ping" || gotHost != "10.12.21.20" {
		t.Fatalf("rezultat: %+v, adresa %q", res, gotHost)
	}

	// Izveštaj vide svi korisnici, i operater koji nije pokrenuo ping.
	w = do(h, "GET", "/api/results/"+st.ResultID+"/report.json", ip, nil, op...)
	var rep struct {
		Title   string
		Stats   latStats
		Outages []outage
	}
	json.Unmarshal(w.Body.Bytes(), &rep)
	if w.Code != 200 || rep.Title != "konzola → FIN-SANDRAA (10.12.21.20)" || rep.Stats.Sent != 10 || rep.Stats.Lost != 2 ||
		len(rep.Outages) != 1 || rep.Outages[0].Count != 2 || rep.Outages[0].FromSeq != 3 {
		t.Fatalf("izveštaj: %d %s", w.Code, w.Body)
	}
	for _, ext := range []string{"csv", "html"} {
		w = do(h, "GET", "/api/results/"+st.ResultID+"/report."+ext, ip, nil, op...)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "FIN-SANDRAA") {
			t.Fatalf("%s: %d", ext, w.Code)
		}
	}
}

func TestContinuousPing(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	const ip = "10.0.0.5:1"
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	adm := []string{"admin", "admin-lozinka"}
	a := &AgentInfo{RemoteAddr: "10.24.21.21:50000"}
	a.AgentID, a.Hostname = "pc1", "PRO-TAMARAC"
	s.hub.restore(map[string]*AgentInfo{"pc1": a})

	var gotCount int
	s.icmpPing = func(ctx context.Context, host string, count int, interval, timeout time.Duration, onProbe nettest.ProbeFunc) (protocol.LatencyResult, error) {
		gotCount = count
		res := protocol.LatencyResult{Target: host, Method: "icmp"}
		for i := 1; i <= count && ctx.Err() == nil; i++ {
			res.Sent++
			res.Received++
			res.Samples = append(res.Samples, 1)
			onProbe(i, 1, true)
			time.Sleep(5 * time.Millisecond)
		}
		nettest.FillLatencyStats(&res)
		return res, nil
	}
	w := do(h, "POST", "/api/agents/pc1/ping", ip, PingRequest{Continuous: true, IntervalMS: 1000}, adm...)
	if w.Code != http.StatusAccepted {
		t.Fatalf("pokretanje: %d %s", w.Code, w.Body)
	}
	var st struct {
		LiveID   string `json:"live_id"`
		ResultID string `json:"result_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)

	// Drugi ping istog računara se odbija, ali ostala merenja na njemu nisu blokirana.
	if w := do(h, "POST", "/api/agents/pc1/ping", ip, PingRequest{DurationSec: 10}, adm...); w.Code != http.StatusConflict {
		t.Fatalf("drugi ping: %d", w.Code)
	}
	release, err := s.locks.acquire([]string{"pc1"}, "admin", "latency", func(string) string { return "" })
	if err != nil {
		t.Fatalf("latencija je blokirana pingom sa konzole: %v", err)
	}
	release()

	time.Sleep(100 * time.Millisecond)
	if w := do(h, "POST", "/api/live/"+st.LiveID+"/stop", ip, map[string]any{}, adm...); w.Code != 200 {
		t.Fatalf("zaustavljanje: %d %s", w.Code, w.Body)
	}
	var res Result
	for i := 0; i < 100 && res.ID == ""; i++ {
		res, _ = s.store.Get(st.ResultID)
		time.Sleep(20 * time.Millisecond)
	}
	if gotCount != protocol.MaxPingSeconds || !res.OK {
		t.Fatalf("neprekidni ping: broj paketa %d (očekivano %d), rezultat %+v", gotCount, protocol.MaxPingSeconds, res)
	}
	w = do(h, "GET", "/api/results/"+st.ResultID+"/report.json", ip, nil, adm...)
	if !strings.Contains(w.Body.String(), "Neprekidni ping, zaustavljen ručno.") || !strings.Contains(w.Body.String(), `"continuous":true`) {
		t.Fatalf("izveštaj: %s", w.Body)
	}
}

func TestCloseSavesRunningPing(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	a := &AgentInfo{RemoteAddr: "10.24.21.21:50000"}
	a.AgentID, a.Hostname = "pc1", "PRO-TAMARAC"
	s.hub.restore(map[string]*AgentInfo{"pc1": a})
	s.icmpPing = func(ctx context.Context, host string, count int, interval, timeout time.Duration, onProbe nettest.ProbeFunc) (protocol.LatencyResult, error) {
		res := protocol.LatencyResult{Target: host, Method: "icmp"}
		for i := 1; ctx.Err() == nil; i++ {
			res.Sent++
			res.Received++
			res.Samples = append(res.Samples, 1)
			onProbe(i, 1, true)
			time.Sleep(5 * time.Millisecond)
		}
		nettest.FillLatencyStats(&res)
		return res, nil
	}
	w := do(h, "POST", "/api/agents/pc1/ping", "10.0.0.5:1", PingRequest{Continuous: true}, "admin", "admin-lozinka")
	var st struct {
		ResultID string `json:"result_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	time.Sleep(50 * time.Millisecond)
	s.Close(2 * time.Second) // gašenje servisa
	res, ok := s.store.Get(st.ResultID)
	if !ok || !res.OK {
		t.Fatalf("rezultat pinga nije sačuvan pri gašenju: %+v", res)
	}
}
