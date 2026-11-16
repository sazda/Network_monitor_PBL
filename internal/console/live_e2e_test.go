package console_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/sazda/network-monitor/internal/agent"
	"github.com/sazda/network-monitor/internal/protocol"
)

// Pravi agent za servere šalje CPU i RAM češće od punog stanja; konzola ih
// prikazuje u spisku servera.
func TestServerAgentLive(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip()
	}
	ts, srv := setup(t)
	a, err := agent.New(agent.Config{ConsoleURL: ts.URL, Token: testToken, AgentID: "srv1", Role: protocol.RoleServer,
		MetricsEvery: time.Hour, LiveEvery: 100 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		for i := 0; i < 250; i++ {
			if a, ok := srv.Hub().Agent("srv1"); !ok || !a.Online {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	waitOnline(t, srv, "srv1")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/servers", nil)
		req.SetBasicAuth(testUser, testPass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Servers []struct {
				Release int       `json:"release"`
				LiveAt  time.Time `json:"live_at"`
				Metrics *struct {
					CPUPct   float64 `json:"cpu_pct"`
					MemTotal uint64  `json:"mem_total"`
				} `json:"metrics"`
			} `json:"servers"`
		}
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if len(r.Servers) == 1 && !r.Servers[0].LiveAt.IsZero() {
			s := r.Servers[0]
			if s.Release != protocol.ServerAgentRelease || s.Metrics == nil || s.Metrics.MemTotal == 0 {
				t.Fatalf("%+v", s)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("konzola nije dobila CPU i RAM uživo")
}
