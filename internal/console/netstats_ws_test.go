package console_test

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sazda/network-monitor/internal/console"
	"github.com/sazda/network-monitor/internal/protocol"
)

// Agent za računare šalje saobraćaj kartica; konzola ga pamti po MAC adresi.
func TestAgentNetStats(t *testing.T) {
	// Konzola upisuje stanje agenata i posle prekida veze (u pozadini), pa se
	// direktorijum briše tek kada se agent odjavi i konzola zatvori.
	dir, err := os.MkdirTemp("", "netstats")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := console.New(console.Config{
		AgentToken: testToken, AdminUser: testUser, AdminPass: testPass,
		DataDir: dir, Logger: log.New(io.Discard, "", 0),
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

	d := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	h := http.Header{}
	h.Set("X-Agent-Token", testToken)
	c, _, err := d.Dial("wss://"+ln.Addr().String()+"/agent/ws", h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		for i := 0; i < 100; i++ {
			if a, ok := srv.Hub().Agent("pc1"); !ok || !a.Online {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		hs.Close()
		srv.Close(time.Second)
		time.Sleep(50 * time.Millisecond)
		os.RemoveAll(dir)
	})
	mac := "38:CA:84:B5:85:AA"
	c.WriteJSON(protocol.Envelope{Type: protocol.TypeHello, Hello: &protocol.Hello{AgentID: "pc1", Hostname: "INF-PC", Protocol: protocol.Version,
		Release: protocol.AgentRelease, Interfaces: []protocol.Interface{{Name: "Wi-Fi", MAC: mac, Addrs: []string{"10.0.23.28/24"}}}}})
	if _, ok := srv.Hub().NetRate("pc1", mac); ok {
		t.Fatal("pre slanja")
	}
	c.WriteJSON(protocol.Envelope{Type: protocol.TypeNetStats, NetStats: []protocol.IfRate{{MAC: "38:ca:84:b5:85:aa", Recv: 2_000_000, Send: 120_000}}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := srv.Hub().NetRate("pc1", mac); ok {
			if r.Recv != 2_000_000 || r.Send != 120_000 {
				t.Fatalf("%+v", r)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("saobraćaj nije stigao")
}
