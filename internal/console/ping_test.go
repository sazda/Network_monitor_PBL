package console_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sazda/network-monitor/internal/console"
)

func apiReq(t *testing.T, method, url string, body any) (int, []byte, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.SetBasicAuth(testUser, testPass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

type liveState struct {
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	Stopped  bool   `json:"stopped"`
	ResultID string `json:"result_id"`
	Stats    struct {
		Sent, Received int
	} `json:"stats"`
}

func waitLive(t *testing.T, base, id string, timeout time.Duration) liveState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, b, _ := apiReq(t, "GET", base+"/api/live", nil)
		var l struct{ Tests []liveState }
		json.Unmarshal(b, &l)
		for _, x := range l.Tests {
			if x.ID == id && (x.Phase == "done" || x.Phase == "error") {
				return x
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("ping %s se nije završio na vreme", id)
	return liveState{}
}

func TestPing(t *testing.T) {
	ts, srv := setup(t)
	startAgent(t, srv, ts.URL, "agent-a")
	waitOnline(t, srv, "agent-a")

	// Ping od 5 s na 200 ms ka konzoli (TCP): ~25 paketa, u pozadini.
	code, b, _ := apiReq(t, "POST", ts.URL+"/api/agents/agent-a/ping", console.PingRequest{Source: "agent", DurationSec: 5, IntervalMS: 200})
	if code != http.StatusAccepted {
		t.Fatalf("pokretanje: %d %s", code, b)
	}
	var started struct{ LiveID, ResultID string }
	json.Unmarshal(b, &struct {
		LiveID   *string `json:"live_id"`
		ResultID *string `json:"result_id"`
	}{&started.LiveID, &started.ResultID})

	// Dok traje, drugo merenje na istom računaru se odbija.
	if code, b, _ := apiReq(t, "POST", ts.URL+"/api/agents/agent-a/latency", map[string]int{"count": 2}); code != http.StatusConflict || !strings.Contains(string(b), "ping") {
		t.Fatalf("zauzet agent: %d %s", code, b)
	}
	l := waitLive(t, ts.URL, started.LiveID, 20*time.Second)
	if l.Phase != "done" || l.Stats.Sent < 20 || l.Stats.Sent > 26 || l.Stats.Received != l.Stats.Sent || l.ResultID != started.ResultID {
		t.Fatalf("ping: %+v", l)
	}

	// Izveštaji.
	code, b, h := apiReq(t, "GET", ts.URL+"/api/results/"+started.ResultID+"/report.csv", nil)
	csv := string(b)
	if code != 200 || !strings.HasPrefix(csv, "\xef\xbb\xbf") || !strings.Contains(csv, "Redni broj;Vreme;Odgovor (ms);Status") ||
		strings.Count(csv, ";OK") != l.Stats.Sent || !strings.Contains(h.Get("Content-Disposition"), ".csv") {
		t.Fatalf("CSV: %d %q", code, csv[:min(len(csv), 600)])
	}
	code, b, _ = apiReq(t, "GET", ts.URL+"/api/results/"+started.ResultID+"/report.html", nil)
	if code != 200 || !strings.Contains(string(b), "Izveštaj pinga") || !strings.Contains(string(b), "<polyline") {
		t.Fatalf("HTML: %d %s", code, b[:min(len(b), 400)])
	}

	// Zaustavljanje dugog pinga: agent vraća dotadašnji rezultat.
	code, b, _ = apiReq(t, "POST", ts.URL+"/api/agents/agent-a/ping", console.PingRequest{Source: "agent", DurationSec: 600, IntervalMS: 200})
	if code != http.StatusAccepted {
		t.Fatalf("pokretanje dugog pinga: %d %s", code, b)
	}
	var long struct {
		LiveID string `json:"live_id"`
	}
	json.Unmarshal(b, &long)
	time.Sleep(1500 * time.Millisecond)
	if code, b, _ := apiReq(t, "POST", ts.URL+"/api/live/"+long.LiveID+"/stop", map[string]any{}); code != 200 {
		t.Fatalf("zaustavljanje: %d %s", code, b)
	}
	l = waitLive(t, ts.URL, long.LiveID, 10*time.Second)
	if l.Phase != "done" || !l.Stopped || l.Stats.Sent < 3 || l.Stats.Sent > 20 {
		t.Fatalf("zaustavljen ping: %+v", l)
	}

	// Neispravni parametri.
	for _, p := range []console.PingRequest{{DurationSec: 2}, {DurationSec: 99999}, {DurationSec: 10, IntervalMS: 50}} {
		if code, _, _ := apiReq(t, "POST", ts.URL+"/api/agents/agent-a/ping", p); code != http.StatusBadRequest {
			t.Fatalf("%+v: očekivano 400, dobijeno %d", p, code)
		}
	}
}
