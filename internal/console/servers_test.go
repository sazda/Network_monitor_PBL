package console

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// testServers: hub sa jednim agentom za servere i jednim računarom.
type smsLog struct {
	mu   sync.Mutex
	list []string
}

func testServers(t *testing.T) (*serverMonitor, *Hub, *smsLog, *time.Time) {
	t.Helper()
	hub := NewHub(log.New(io.Discard, "", 0))
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	hub.agents["s1"] = &AgentInfo{Hello: protocol.Hello{AgentID: "s1", Hostname: "SQL01", Role: protocol.RoleServer},
		Online: true, RemoteAddr: "10.0.36.10:5000", ConnectedAt: now}
	hub.agents["p1"] = &AgentInfo{Hello: protocol.Hello{AgentID: "p1", Hostname: "INF-PC"}, Online: true}
	n := newNotifier(NotifyConfig{Enabled: true, EmailHost: "mail", EmailFrom: "a@b.rs", SMSEmail: "netmon@b.rs",
		Channels: &NotifyChannels{SMS: ChannelOpts{On: true, Net: true}}}, log.New(io.Discard, "", 0), nil)
	sms := &smsLog{}
	n.sendSMS = func(_ context.Context, _ NotifyConfig, text string) error {
		sms.mu.Lock()
		sms.list = append(sms.list, text)
		sms.mu.Unlock()
		return nil
	}
	var h *tsdb
	s := newServerMonitor(t.TempDir(), hub, n, log.New(io.Discard, "", 0), func() *tsdb { return h })
	s.now = func() time.Time { return now }
	s.pingFn = func(context.Context, string) (float64, int, int) { return 1, 2, 2 }
	return s, hub, sms, &now
}

func waitSMSList(sms *smsLog, n int) []string {
	for i := 0; i < 200; i++ {
		sms.mu.Lock()
		k := len(sms.list)
		sms.mu.Unlock()
		if k >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // da se vide i neočekivane poruke
	sms.mu.Lock()
	defer sms.mu.Unlock()
	return append([]string(nil), sms.list...)
}

func TestServerOfflineAndBack(t *testing.T) {
	s, hub, sms, now := testServers(t)
	hub.agents["s1"].Online = false
	s.changed(*hub.agents["s1"])
	*now = now.Add(time.Minute)
	s.tick()
	if len(waitSMSList(sms, 0)) != 0 {
		t.Fatal("pre roka")
	}
	*now = now.Add(70 * time.Second)
	s.tick()
	got := waitSMSList(sms, 1)
	if len(got) != 1 || !strings.Contains(got[0], "SERVER SQL01 OFFLINE od 09:00 (odgovara na ping, ali agent ne radi)") {
		t.Fatalf("offline: %q", got)
	}
	s.tick() // ne ponavlja se
	*now = now.Add(3 * time.Minute)
	hub.agents["s1"].Online = true
	s.changed(*hub.agents["s1"])
	got = waitSMSList(sms, 2)
	if len(got) != 2 || !strings.Contains(got[1], "OK SERVER SQL01 ponovo online posle 5 min") {
		t.Fatalf("povratak: %q", got)
	}
	// Računar (ne server) se ne prati ovde.
	if l := s.list(nil); len(l.Servers) != 1 || l.Servers[0].Name != "SQL01" {
		t.Fatalf("spisak: %+v", l.Servers)
	}
}

func TestServerHighCPUAndReboot(t *testing.T) {
	s, _, sms, now := testServers(t)
	m := protocol.Metrics{UptimeSec: 86400, CPUPct: 95, MemTotal: 16 << 30, MemUsed: 4 << 30,
		Procs: []protocol.Proc{{Name: "sqlservr.exe", PID: 1234, CPUPct: 72, Mem: 3 << 30}}}
	s.metricsIn("s1", m)
	for i := 0; i < 20; i++ { // 10 minuta na 30 s
		*now = now.Add(30 * time.Second)
		m.UptimeSec += 30
		s.metricsIn("s1", m)
	}
	got := waitSMSList(sms, 1)
	if len(got) != 1 || !strings.Contains(got[0], "SERVER SQL01: CPU 95% duze od 10 min") {
		t.Fatalf("CPU: %q", got)
	}
	// Uz događaj se čuva spisak procesa iz tog trenutka (SMS ostaje isti).
	if ev := s.recentEvents("s1", 1); len(ev) != 1 || ev[0].Kind != "cpu_high" || len(ev[0].Procs) != 1 || ev[0].Procs[0].Name != "sqlservr.exe" {
		t.Fatalf("događaj: %+v", ev)
	}
	if strings.Contains(got[0], "sqlservr") {
		t.Fatalf("SMS ne sme da se menja: %q", got[0])
	}
	// Kratak pad ispod praga (u okviru histereze) ne poništava; pravi pad javlja oporavak.
	*now = now.Add(30 * time.Second)
	m.UptimeSec += 30
	m.CPUPct = 88
	s.metricsIn("s1", m)
	*now = now.Add(30 * time.Second)
	m.UptimeSec += 30
	m.CPUPct = 40
	s.metricsIn("s1", m)
	got = waitSMSList(sms, 2)
	if len(got) != 2 || !strings.Contains(got[1], "OK SERVER SQL01: CPU ponovo normalan (40%)") {
		t.Fatalf("oporavak: %q", got)
	}
	// Restart: uptime se smanjio.
	*now = now.Add(30 * time.Second)
	m.UptimeSec = 60
	s.metricsIn("s1", m)
	got = waitSMSList(sms, 3)
	if len(got) != 3 || !strings.Contains(got[2], "RESTART SERVER SQL01") {
		t.Fatalf("restart: %q", got)
	}
	l := s.list(nil)
	if l.Servers[0].Metrics == nil || *l.Servers[0].MemPct != 25 {
		t.Fatalf("prikaz: %+v", l.Servers[0])
	}
}

func TestAnswerServer(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 42, 0, 0, time.Local)
	srv := serversResponse{Servers: []serverView{
		{Name: "APP02", Online: false, Since: now.Add(-90 * time.Minute)},
		{Name: "SQL01", Online: true, CPUHigh: true, MemPct: ptr(61.0), Metrics: &protocol.Metrics{UptimeSec: 12*86400 + 4*3600, CPUPct: 93,
			MemTotal: 16 << 30, MemUsed: 9800 << 20, Disks: []protocol.Disk{{Name: "C:", Total: 100 << 30, Free: 28 << 30}, {Name: "D:", Total: 500 << 30, Free: 400 << 30}}}},
	}}
	cases := map[string]string{
		"server SQL01": "SERVER SQL01 10:42: online, uptime 12d 4h, CPU 93% (VISOK), RAM 60% (9.6 GB/16 GB), disk C: 72% (slobodno 28 GB), D: 20% (slobodno 400 GB)",
		"server 2":     "SERVER SQL01 10:42: online, uptime 12d 4h, CPU 93% (VISOK), RAM 60% (9.6 GB/16 GB), disk C: 72% (slobodno 28 GB), D: 20% (slobodno 400 GB)",
		"server 1":     "SERVER APP02 10:42: OFFLINE od 09:12 (1 h 30 min)",
		"server 3":     "SERVER: nema servera broj 3 (servera: 2). Spisak: server lista",
		"SERVER app02": "SERVER APP02 10:42: OFFLINE od 09:12 (1 h 30 min)",
		"server":       "SERVERI 10:42: 1/2 online; offline: APP02 od 09:12; visoko opterecenje: SQL01",
		"server sq":    `SERVER: nepoznat server "sq". Da li ste mislili: SQL01?`,
		"server xyz":   `SERVER: nepoznat server "xyz". Serveri: APP02, SQL01`,
	}
	for q, want := range cases {
		if got := answerQuery(netdevResponse{}, srv, q, now, "", ""); got != want {
			t.Errorf("%q:\n got %q\nwant %q", q, got, want)
		}
	}
}

func TestAnswerServerAliases(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 42, 0, 0, time.Local)
	srv := serversResponse{Servers: []serverView{
		{Name: "srv-monitor", Online: true},
		{Name: "SQL01", Online: false, Since: now.Add(-30 * time.Minute)},
	}}
	aliases := "SOL = srv-monitor\nBAZA = SQL01\nST = stari-server"
	cases := map[string]string{
		"server SOL":         "SERVER srv-monitor 10:42: online (stanje jos nije stiglo)",
		"server sol":         "SERVER srv-monitor 10:42: online (stanje jos nije stiglo)",
		"SERVER baza":        "SERVER SQL01 10:42: OFFLINE od 10:12 (30 min)",
		"server srv-monitor": "SERVER srv-monitor 10:42: online (stanje jos nije stiglo)",
		"server st":          "SERVER ST: server stari-server nije u konzoli",
		"server xyz":         `SERVER: nepoznat server "xyz". Skracenice: SOL, BAZA, ST`,
	}
	for q, want := range cases {
		if got := answerQuery(netdevResponse{}, srv, q, now, "", aliases); got != want {
			t.Errorf("%q:\n got %q\nwant %q", q, got, want)
		}
	}
	c := NotifyConfig{SMSServerAliases: "SOL srv-monitor"}
	if err := c.validateQueries(); err == nil || !strings.Contains(err.Error(), "skraćenice servera, red 1") {
		t.Fatalf("neispravne skraćenice servera: %v", err)
	}
}

func TestAgentRelease(t *testing.T) {
	s, hub, _, _ := testServers(t)
	if v := s.list(nil).Servers[0]; !v.Outdated || v.WantRelease != protocol.ServerAgentRelease {
		t.Fatalf("server bez izdanja: %+v", v)
	}
	hub.agents["s1"].Release = protocol.ServerAgentRelease
	if v := s.list(nil).Servers[0]; v.Outdated {
		t.Fatalf("server sa izdanjem: %+v", v)
	}
	// Agent za računare: nova oznaka build-a konzole ne traži nadogradnju.
	a := AgentInfo{Hello: protocol.Hello{Version: "build-20261101-0900", Release: protocol.AgentRelease}}
	if a.markRelease(); a.Outdated {
		t.Fatalf("računar sa trenutnim izdanjem: %+v", a)
	}
	a.Release = 0
	if a.markRelease(); !a.Outdated || a.WantRelease != protocol.AgentRelease {
		t.Fatalf("računar bez izdanja: %+v", a)
	}
}

func TestServerListQuery(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 42, 0, 0, time.Local)
	var srv serversResponse
	var want []string
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("srv-%02d", 39-i)
		srv.Servers = append(srv.Servers, serverView{Name: name, Online: i%3 == 0})
	}
	for i := 0; i < 40; i++ {
		want = append(want, fmt.Sprintf("%d srv-%02d", i+1, i))
	}
	exp := `SERVERI 10:42 (40):\n` + strings.Join(want, `\n`) + `\nIzvestaj: server <broj>`
	for _, q := range []string{"server list", "SERVER Lista"} {
		if got := answerQuery(netdevResponse{}, srv, q, now, "", ""); got != exp {
			t.Fatalf("%q:\n got %q\nwant %q", q, got, exp)
		}
	}
	if len(exp) <= queryReplyMax {
		t.Fatal("test treba duži spisak od jednog odgovora")
	}
	if got := answerQuery(netdevResponse{}, serversResponse{}, "server list", now, "", ""); got != "SERVERI: nema servera u konzoli" {
		t.Fatal(got)
	}
}

func TestFoldHeaderSubject(t *testing.T) {
	long := "[NETMON-SMS] #12 SERVERI 10:42 (40): " + strings.Repeat("inf-server-abc, ", 60) + "kraj"
	raw := buildEmail("netmon@b.rs", []string{"netmon@b.rs"}, notifyMessage{Title: long, Body: "x"})
	hdr, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	for _, l := range strings.Split(hdr, "\r\n") {
		if len(l) > 78 {
			t.Fatalf("red zaglavlja duži od 78: %d", len(l))
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || dec != long {
		t.Fatalf("naslov posle čitanja:\n%q\n%q (%v)", dec, long, err)
	}
	// Kratak naslov ostaje u jednom redu.
	if !strings.Contains(string(buildEmail("a@b.rs", []string{"a@b.rs"}, notifyMessage{Title: "kratko", Body: "x"})), "Subject: kratko\r\n") {
		t.Fatal("kratak naslov")
	}
}

// CPU i RAM na 5 s menjaju samo prikaz; istorija i obaveštenja ostaju na
// punom stanju.
func TestServerLive(t *testing.T) {
	s, _, _, now := testServers(t)
	s.metricsIn("s1", protocol.Metrics{UptimeSec: 1000, CPUPct: 20, MemTotal: 1000, MemUsed: 500})
	*now = now.Add(5 * time.Second)
	s.liveIn("s1", protocol.Live{CPUPct: 97, MemTotal: 1000, MemUsed: 900})
	v := s.list(nil).Servers[0]
	if v.Metrics == nil || v.Metrics.CPUPct != 97 || v.MemPct == nil || *v.MemPct != 90 || v.LiveAt.IsZero() || v.Metrics.UptimeSec != 1000 {
		t.Fatalf("uživo: %+v %v", v.Metrics, v.MemPct)
	}
	if st := s.st("s1"); !st.CPUSince.IsZero() {
		t.Fatal("brzo čitanje ne sme da pokrene praćenje visokog CPU-a")
	}
	// Zastarelo brzo čitanje se ne prikazuje.
	*now = now.Add(srvLiveFresh)
	if v := s.list(nil).Servers[0]; v.Metrics.CPUPct != 20 || !v.LiveAt.IsZero() {
		t.Fatalf("zastarelo: %+v", v.Metrics)
	}
	// Novije puno stanje ima prednost nad starijim brzim.
	s.liveIn("s1", protocol.Live{CPUPct: 50, MemTotal: 1000, MemUsed: 500})
	*now = now.Add(time.Second)
	s.metricsIn("s1", protocol.Metrics{UptimeSec: 1020, CPUPct: 30, MemTotal: 1000, MemUsed: 500})
	if v := s.list(nil).Servers[0]; v.Metrics.CPUPct != 30 {
		t.Fatalf("puno posle brzog: %+v", v.Metrics)
	}
}
