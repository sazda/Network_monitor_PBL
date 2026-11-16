package console_test

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sazda/network-monitor/internal/agent"
	"github.com/sazda/network-monitor/internal/console"
	"github.com/sazda/network-monitor/internal/protocol"
)

// Regresija: posle testa brzine agent je delio tls.Config sa net/http
// klijentom, koji u njega upisuje ALPN „h2”. Pri sledećem povezivanju na
// TLS server sa HTTP/2 (podrazumevano kod Go servera) WebSocket nije radio,
// pa agent posle restarta konzole nikad nije ponovo bio na vezi.
func TestReconnectAfterSpeedTestWithHTTP2(t *testing.T) {
	srv, err := console.New(console.Config{
		AgentToken: testToken, AdminUser: testUser, AdminPass: testPass,
		DataDir: t.TempDir(), Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.EnableHTTP2 = true // kao konzola pre ispravke
	ts.StartTLS()
	defer ts.Close()

	a, err := agent.New(agent.Config{ConsoleURL: ts.URL, Token: testToken, AgentID: "h2-agent", Insecure: true, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
		waitState(t, srv, false, "h2-agent")
	}()
	waitState(t, srv, true, "h2-agent")

	// Test brzine (koristi net/http klijent agenta).
	var out protocol.SpeedTestResult
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	if err := srv.Hub().Call(cctx, "h2-agent", protocol.CmdSpeedTest, protocol.SpeedTestParams{Seconds: 1, Streams: 1}, &out); err != nil {
		t.Fatalf("test brzine: %v", err)
	}

	// Prekid svih veza (kao restart konzole) i čekanje ponovnog povezivanja.
	if !srv.Hub().Disconnect("h2-agent") {
		t.Fatal("agent nije bio povezan")
	}
	waitState(t, srv, false, "h2-agent")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ag, ok := srv.Hub().Agent("h2-agent"); ok && ag.Online {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("agent se posle testa brzine nije ponovo povezao")
}

func waitState(t *testing.T, srv *console.Server, want bool, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ag, ok := srv.Hub().Agent(id); ok && ag.Online == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent %s: stanje na vezi nije %v", id, want)
}

// Konzola ne nudi HTTP/2, pa se i agent koji (zbog stare greške) nudi „h2”
// povezuje preko HTTP/1.1.
func TestConsoleRefusesHTTP2(t *testing.T) {
	srv, err := console.New(console.Config{
		AgentToken: testToken, AdminUser: testUser, AdminPass: testPass,
		DataDir: t.TempDir(), Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, err := console.GenerateSelfSigned(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := console.NewHTTPServer("", srv.Handler(), &tls.Config{Certificates: []tls.Certificate{cert}})
	go hs.ServeTLS(ln, "", "")
	defer hs.Close()

	// Klijent kao stari agent: nudi h2 pa http/1.1.
	d := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}}}
	h := http.Header{}
	h.Set("X-Agent-Token", testToken)
	c, _, err := d.Dial("wss://"+ln.Addr().String()+"/agent/ws", h)
	if err != nil {
		t.Fatalf("stari agent ne može da se poveže: %v", err)
	}
	c.Close()
}
