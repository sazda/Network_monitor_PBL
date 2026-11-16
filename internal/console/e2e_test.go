package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sazda/network-monitor/internal/agent"
	"github.com/sazda/network-monitor/internal/console"
	"github.com/sazda/network-monitor/internal/protocol"
)

const (
	testToken = "test-agent-token-0123456789"
	testUser  = "admin"
	testPass  = "admin-password"
)

func setup(t *testing.T) (*httptest.Server, *console.Server) {
	t.Helper()
	srv, err := console.New(console.Config{
		AgentToken: testToken, AdminUser: testUser, AdminPass: testPass,
		DataDir: t.TempDir(), Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

func startAgent(t *testing.T, srv *console.Server, url, id string) {
	t.Helper()
	a, err := agent.New(agent.Config{
		ConsoleURL: url, Token: testToken, AgentID: id,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	// Pri gašenju sačekamo da konzola registruje prekid (i snimi stanje)
	// pre nego što se obriše privremeni direktorijum.
	t.Cleanup(func() {
		cancel()
		<-done
		for i := 0; i < 250; i++ {
			if a, ok := srv.Hub().Agent(id); !ok || !a.Online {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func waitOnline(t *testing.T, srv *console.Server, ids ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, id := range ids {
			if a, ok := srv.Hub().Agent(id); !ok || !a.Online {
				all = false
			}
		}
		if all {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agenti %v se nisu povezali", ids)
}

func post(t *testing.T, ts *httptest.Server, path string, body any) (int, console.Result) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(b))
	req.SetBasicAuth(testUser, testPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r console.Result
	json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode, r
}

func TestEndToEnd(t *testing.T) {
	ts, srv := setup(t)
	startAgent(t, srv, ts.URL, "agent-a")
	startAgent(t, srv, ts.URL, "agent-b")
	waitOnline(t, srv, "agent-a", "agent-b")

	t.Run("latency", func(t *testing.T) {
		code, r := post(t, ts, "/api/agents/agent-a/latency", protocol.LatencyParams{Count: 3, IntervalMS: 1})
		if code != 200 || !r.OK {
			t.Fatalf("status %d, rezultat %+v", code, r)
		}
		var lr protocol.LatencyResult
		json.Unmarshal(r.Data, &lr)
		if lr.Sent != 3 || lr.Received != 3 || lr.AvgMS <= 0 {
			t.Fatalf("neočekivan rezultat: %+v", lr)
		}
	})

	t.Run("speedtest", func(t *testing.T) {
		code, r := post(t, ts, "/api/agents/agent-a/speedtest", protocol.SpeedTestParams{Seconds: 1, Streams: 2})
		if code != 200 || !r.OK {
			t.Fatalf("status %d, rezultat %+v", code, r)
		}
		var sr protocol.SpeedTestResult
		json.Unmarshal(r.Data, &sr)
		if sr.Download == nil || sr.Download.Bytes == 0 || sr.Upload == nil || sr.Upload.Bytes == 0 {
			t.Fatalf("neočekivan rezultat: %s", r.Data)
		}
	})

	t.Run("lan", func(t *testing.T) {
		// Oba agenta su na istom računaru, pa adresu servera zadajemo ručno.
		code, r := post(t, ts, "/api/lan-test", console.LANTestRequest{
			ClientID: "agent-a", ServerID: "agent-b", ServerAddr: "127.0.0.1", Seconds: 1, Streams: 2,
		})
		if code != 200 || !r.OK {
			t.Fatalf("status %d, rezultat %+v", code, r)
		}
		var sr protocol.SpeedTestResult
		json.Unmarshal(r.Data, &sr)
		if sr.Download == nil || sr.Download.Bytes == 0 || sr.Upload == nil || sr.Upload.Bytes == 0 {
			t.Fatalf("neočekivan rezultat: %s", r.Data)
		}
	})

	t.Run("live", func(t *testing.T) {
		// Posle oba testa brzine prikaz uživo ima međurezultate za oba smera:
		// za „Brzina” ih meri konzola, za LAN test ih šalje agent.
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/live", nil)
		req.SetBasicAuth(testUser, testPass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var live struct {
			Tests []struct {
				Kind, Phase  string
				Live         bool
				DownloadMbps float64 `json:"download_mbps"`
				UploadMbps   float64 `json:"upload_mbps"`
				Points       []struct{ Phase string }
				Probes       []struct {
					Seq  int
					RTT  float64 `json:"rtt_ms"`
					Lost bool
				}
				Sent    int
				LostSeq bool `json:"lost_seq"`
			}
		}
		json.NewDecoder(resp.Body).Decode(&live)
		seen := map[string]bool{}
		for _, lt := range live.Tests {
			if lt.Kind == "latency" {
				// Merenje latencije: svaki paket je zabeležen, redom.
				if lt.Phase != "done" || !lt.Live || !lt.LostSeq || lt.Sent != 3 || len(lt.Probes) != 3 || lt.Probes[2].Seq != 3 || lt.Probes[0].RTT <= 0 {
					t.Fatalf("latencija: neočekivano stanje prikaza uživo: %+v", lt)
				}
				seen[lt.Kind] = true
				continue
			}
			phases := map[string]bool{}
			for _, p := range lt.Points {
				phases[p.Phase] = true
			}
			if lt.Phase != "done" || !lt.Live || !phases["download"] || !phases["upload"] || lt.DownloadMbps <= 0 || lt.UploadMbps <= 0 {
				t.Fatalf("%s: neočekivano stanje prikaza uživo: %+v", lt.Kind, lt)
			}
			seen[lt.Kind] = true
		}
		if !seen["speedtest"] || !seen["lan"] || !seen["latency"] {
			t.Fatalf("nedostaju testovi u prikazu uživo: %+v", live.Tests)
		}
	})

	t.Run("busy", func(t *testing.T) {
		// Dok traje test brzine na agent-a, drugo merenje na njemu se odbija
		// (rezultati bi bili pogrešni), a merenje na agent-b radi normalno.
		done := make(chan int)
		go func() {
			code, _ := post(t, ts, "/api/agents/agent-a/speedtest", protocol.SpeedTestParams{Seconds: 2, Streams: 1})
			done <- code
		}()
		time.Sleep(300 * time.Millisecond)
		raw := func(path string, body any) (int, string) {
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(b))
			req.SetBasicAuth(testUser, testPass)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			msg, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(msg)
		}
		if code, msg := raw("/api/agents/agent-a/latency", protocol.LatencyParams{Count: 2, IntervalMS: 1}); code != http.StatusConflict || !bytes.Contains([]byte(msg), []byte("test brzine")) {
			t.Fatalf("latencija na zauzetom agentu: %d %s", code, msg)
		}
		if code, msg := raw("/api/lan-test", console.LANTestRequest{ClientID: "agent-b", ServerID: "agent-a", ServerAddr: "127.0.0.1", Seconds: 1}); code != http.StatusConflict {
			t.Fatalf("LAN test sa zauzetim agentom: %d %s", code, msg)
		}
		if code, r := post(t, ts, "/api/agents/agent-b/latency", protocol.LatencyParams{Count: 2, IntervalMS: 1}); code != 200 || !r.OK {
			t.Fatalf("latencija na slobodnom agentu: %d %+v", code, r)
		}
		if code := <-done; code != 200 {
			t.Fatalf("test brzine: %d", code)
		}
		// Posle završetka agent je ponovo slobodan.
		if code, r := post(t, ts, "/api/agents/agent-a/latency", protocol.LatencyParams{Count: 2, IntervalMS: 1}); code != 200 || !r.OK {
			t.Fatalf("latencija posle testa: %d %+v", code, r)
		}
	})

	t.Run("offline", func(t *testing.T) {
		code, r := post(t, ts, "/api/agents/nepostojeci/latency", protocol.LatencyParams{})
		if code != http.StatusConflict || r.OK {
			t.Fatalf("očekivan 409, dobijeno %d %+v", code, r)
		}
	})
}

func TestAuth(t *testing.T) {
	ts, _ := setup(t)

	// Bez lozinke.
	resp, _ := http.Get(ts.URL + "/api/agents")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("očekivan 401, dobijeno %d", resp.StatusCode)
	}

	// Pogrešan token agenta.
	req, _ := http.NewRequest("GET", ts.URL+"/agent/speed/download?seconds=1", nil)
	req.Header.Set("X-Agent-Token", "pogresan")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("očekivan 401, dobijeno %d", resp.StatusCode)
	}

	// POST bez JSON Content-Type-a (CSRF zaštita).
	req, _ = http.NewRequest("POST", ts.URL+"/api/lan-test", bytes.NewReader([]byte("x=1")))
	req.SetBasicAuth(testUser, testPass)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("očekivan 415, dobijeno %d", resp.StatusCode)
	}

	// Agent sa pogrešnim tokenom ne može da se poveže.
	ts2, srv2 := setup(t)
	a, _ := agent.New(agent.Config{ConsoleURL: ts2.URL, Token: "pogresan-token-xxxxxxxx", AgentID: "uljez", Logger: log.New(io.Discard, "", 0)})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	a.Run(ctx)
	if _, ok := srv2.Hub().Agent("uljez"); ok {
		t.Fatal("agent sa pogrešnim tokenom je prihvaćen")
	}
}
