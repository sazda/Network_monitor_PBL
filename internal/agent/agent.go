// Package agent implementira agenta koji se instalira na računare korisnika.
//
// Agent sam uspostavlja odlaznu WebSocket konekciju ka konzoli i čeka komande.
// Zbog toga na računaru korisnika ne treba otvarati dolazne portove i agent
// radi i iza NAT-a/rutera (konzola na VPS-u ne može direktno da dođe do
// računara u lokalnoj mreži, ali agent može da dođe do konzole).
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sazda/network-monitor/internal/nettest"
	"github.com/sazda/network-monitor/internal/protocol"
	"github.com/sazda/network-monitor/internal/sysinfo"
)

// Version agenta; postavlja se pri build-u preko -ldflags.
var Version = "dev"

// Config je konfiguracija agenta.
type Config struct {
	ConsoleURL string // npr. https://monitor.example.com
	Token      string // zajednički tajni token za agente
	AgentID    string // stabilan ID; prazno = učitaj/generiši u StateDir
	StateDir   string // gde se čuva agent ID
	CAFile     string // opcioni CA sertifikat (za self-signed konzolu)
	Insecure   bool   // preskoči proveru TLS sertifikata (SAMO za testiranje)
	Logger     *log.Logger
	// Role: "" = agent za računare (merenja); protocol.RoleServer = agent za
	// servere (šalje samo stanje servera, ne izvršava merenja).
	Role string
	// MetricsEvery: koliko često agent za servere šalje stanje (podrazumevano 30 s).
	MetricsEvery time.Duration
	// LiveEvery: koliko često agent za servere šalje samo CPU i RAM
	// (podrazumevano 5 s).
	LiveEvery time.Duration
}

// Agent drži stanje jednog agenta.
type Agent struct {
	cfg    Config
	base   *url.URL
	http   *http.Client
	dialer *websocket.Dialer
	log    *log.Logger

	writeMu sync.Mutex
	lanMu   sync.Mutex
	lan     *nettest.LANServer
	sys     sysinfo.Collector
}

// New kreira agenta.
func New(cfg Config) (*Agent, error) {
	u, err := url.Parse(cfg.ConsoleURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("neispravan URL konzole: %q", cfg.ConsoleURL)
	}
	u.Path = ""
	if cfg.Token == "" {
		return nil, errors.New("token nije zadat")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.AgentID == "" {
		id, err := loadOrCreateID(cfg.StateDir)
		if err != nil {
			return nil, err
		}
		cfg.AgentID = id
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.Insecure}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA fajl ne sadrži ispravan sertifikat")
		}
		tlsCfg.RootCAs = pool
	}
	if u.Scheme == "http" {
		cfg.Logger.Printf("UPOZORENJE: konzola se koristi preko nešifrovanog HTTP-a; token putuje u čistom tekstu")
	}
	// Zasebne kopije TLS podešavanja: net/http klijent (test brzine) u svoju
	// upisuje ALPN „h2”, a WebSocket mora da ostane na HTTP/1.1. Sa deljenom
	// kopijom agent posle testa brzine i prekida veze više nije mogao da se
	// poveže na konzolu sa HTTP/2.
	wsTLS := tlsCfg.Clone()
	wsTLS.NextProtos = []string{"http/1.1"}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg.Clone()
	tr.MaxIdleConnsPerHost = protocol.MaxStreams
	return &Agent{
		cfg:  cfg,
		base: u,
		http: &http.Client{Transport: tr},
		dialer: &websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 15 * time.Second,
			TLSClientConfig:  wsTLS,
		},
		log: cfg.Logger,
	}, nil
}

// ID vraća ID agenta.
func (a *Agent) ID() string { return a.cfg.AgentID }

// Run održava konekciju ka konzoli dok se ctx ne otkaže; posle prekida se
// ponovo povezuje sa eksponencijalnim čekanjem (do 60s).
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		a.log.Printf("veza sa konzolom prekinuta: %v; ponovni pokušaj za %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

func (a *Agent) wsURL() string {
	u := *a.base
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/agent/ws"
	return u.String()
}

func (a *Agent) session(ctx context.Context) error {
	hdr := http.Header{}
	hdr.Set(nettest.AgentTokenHeader, a.cfg.Token)
	conn, resp, err := a.dialer.DialContext(ctx, a.wsURL(), hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("%w (HTTP %d)", err, resp.StatusCode)
		}
		return err
	}
	defer conn.Close()
	a.log.Printf("povezan na konzolu %s kao %s", a.base.Host, a.cfg.AgentID)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	conn.SetReadLimit(1 << 20)
	conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
	// Konzola periodično šalje ping; odgovaramo pong-om i produžavamo rok za
	// čitanje. Ako ping-ovi prestanu, veza se smatra mrtvom i obnavlja se.
	// (WriteControl je bezbedan za istovremeni poziv sa drugim upisima.)
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})

	if err := a.send(conn, protocol.Envelope{Type: protocol.TypeHello, Hello: a.hello()}); err != nil {
		return err
	}
	if a.cfg.Role == protocol.RoleServer {
		go a.metricsLoop(ctx, conn)
	} else {
		go a.netStatsLoop(ctx, conn)
	}

	var cancelMu sync.Mutex
	cancels := map[string]context.CancelFunc{}
	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return err
		}
		if env.Type == protocol.TypeCancel {
			cancelMu.Lock()
			if c := cancels[env.ID]; c != nil {
				c()
			}
			cancelMu.Unlock()
			continue
		}
		if env.Type != protocol.TypeCommand {
			continue
		}
		cmdCtx, cmdCancel := context.WithCancel(ctx)
		cancelMu.Lock()
		cancels[env.ID] = cmdCancel
		cancelMu.Unlock()
		go func(ctx context.Context, env protocol.Envelope) {
			defer func() {
				cancelMu.Lock()
				delete(cancels, env.ID)
				cancelMu.Unlock()
				cmdCancel()
			}()
			progress := func(p protocol.Progress) {
				// Međurezultati su samo za prikaz; greška slanja nije bitna.
				a.send(conn, protocol.Envelope{Type: protocol.TypeProgress, ID: env.ID, Progress: &p})
			}
			data, err := a.execute(ctx, env.Command, env.Params, progress)
			out := protocol.Envelope{Type: protocol.TypeResult, ID: env.ID, OK: err == nil}
			if err != nil {
				out.Error = err.Error()
			} else {
				out.Data, _ = json.Marshal(data)
			}
			if err := a.send(conn, out); err != nil {
				a.log.Printf("slanje rezultata nije uspelo: %v", err)
			}
		}(cmdCtx, env)
	}
}

func (a *Agent) send(conn *websocket.Conn, env protocol.Envelope) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return conn.WriteJSON(env)
}

// netStatsEvery: koliko često agent za računare šalje saobraćaj kartica.
const netStatsEvery = 10 * time.Second

// netStatsLoop šalje saobraćaj mrežnih kartica na 10 s (brzina računara u
// detaljima switch-a).
func (a *Agent) netStatsLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(netStatsEvery)
	defer t.Stop()
	var r sysinfo.IfRates
	for {
		if c, err := sysinfo.IfCounters(); err == nil {
			if rates := r.Next(c, time.Now()); len(rates) > 0 {
				if err := a.send(conn, protocol.Envelope{Type: protocol.TypeNetStats, NetStats: rates}); err != nil {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// metricsLoop šalje stanje servera odmah po povezivanju i zatim periodično
// (puno stanje na MetricsEvery, samo CPU i RAM na LiveEvery).
func (a *Agent) metricsLoop(ctx context.Context, conn *websocket.Conn) {
	every := a.cfg.MetricsEvery
	if every <= 0 {
		every = 30 * time.Second
	}
	liveEvery := a.cfg.LiveEvery
	if liveEvery <= 0 {
		liveEvery = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	lt := time.NewTicker(liveEvery)
	defer lt.Stop()
	full := true
	for {
		if full {
			m, err := a.sys.Sample()
			if err != nil {
				a.log.Printf("stanje servera: %v", err)
			} else if err := a.send(conn, protocol.Envelope{Type: protocol.TypeMetrics, Metrics: &m}); err != nil {
				return
			}
			a.sys.Quick() // početak perioda za sledeće brzo čitanje
		} else if l, ok, err := a.sys.Quick(); err == nil && ok {
			if err := a.send(conn, protocol.Envelope{Type: protocol.TypeLive, Live: &l}); err != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			full = true
		case <-lt.C:
			full = false
		}
	}
}

func (a *Agent) hello() *protocol.Hello {
	host, _ := os.Hostname()
	gw, _ := nettest.DefaultGateway()
	if a.cfg.Role == protocol.RoleServer {
		return &protocol.Hello{AgentID: a.cfg.AgentID, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH,
			Version: Version, Protocol: protocol.Version, Interfaces: Interfaces(), Gateway: gw, Role: protocol.RoleServer,
			Release: protocol.ServerAgentRelease}
	}
	return &protocol.Hello{
		AgentID:    a.cfg.AgentID,
		Hostname:   host,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Version:    Version,
		Protocol:   protocol.Version,
		Interfaces: Interfaces(),
		Gateway:    gw,
		Features:   []string{protocol.FeatureLive, protocol.FeaturePing},
		Release:    protocol.AgentRelease,
	}
}

// execute izvršava jednu od dozvoljenih komandi.
// progress (može biti nil) javlja međurezultate merenja brzine.
func (a *Agent) execute(ctx context.Context, cmd string, raw json.RawMessage, progress func(protocol.Progress)) (any, error) {
	if a.cfg.Role == protocol.RoleServer {
		return nil, errors.New("agent za servere ne izvršava merenja")
	}
	if progress == nil {
		progress = func(protocol.Progress) {}
	}
	report := func(phase string) func(float64) {
		return func(mbps float64) { progress(protocol.Progress{Phase: phase, Mbps: mbps}) }
	}
	switch cmd {
	case protocol.CmdLatency:
		var p protocol.LatencyParams
		if err := unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return a.latency(ctx, p, func(seq int, rtt float64, ok bool) {
			progress(protocol.Progress{Phase: "probe", Seq: seq, RTTMS: rtt, Lost: !ok})
		})

	case protocol.CmdSpeedTest:
		var p protocol.SpeedTestParams
		if err := unmarshal(raw, &p); err != nil {
			return nil, err
		}
		secs := protocol.Clamp(p.Seconds, protocol.DefaultSeconds, protocol.MaxSeconds)
		streams := protocol.Clamp(p.Streams, protocol.DefaultStreams, protocol.MaxStreams)
		dir := protocol.NormalizeDirection(p.Direction)
		base := a.base.String()
		res := protocol.SpeedTestResult{Peer: a.base.Host}
		lat := nettest.MeasureLatency(ctx, a.consoleHostPort(), 5, 100*time.Millisecond, 3*time.Second)
		res.Latency = &lat
		var err error
		if dir != "upload" {
			res.Download, err = nettest.RunTransfer(ctx, "download", streams, time.Duration(secs)*time.Second,
				nettest.HTTPDownloadWorker(a.http, base, a.cfg.Token, secs))
			if err != nil {
				return nil, fmt.Errorf("download: %w", err)
			}
		}
		if dir != "download" {
			res.Upload, err = nettest.RunTransfer(ctx, "upload", streams, time.Duration(secs)*time.Second,
				nettest.HTTPUploadWorker(a.http, base, a.cfg.Token))
			if err != nil {
				return nil, fmt.Errorf("upload: %w", err)
			}
		}
		return res, nil

	case protocol.CmdLANServer:
		var p protocol.LANServerParams
		if err := unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.Port < 0 || p.Port > 65535 {
			return nil, errors.New("neispravan port")
		}
		ttl := time.Duration(protocol.Clamp(p.TTLSeconds, protocol.DefaultLANTTL, protocol.MaxLANTTL)) * time.Second
		a.lanMu.Lock()
		defer a.lanMu.Unlock()
		if a.lan != nil {
			a.lan.Close() // istovremeno je aktivan najviše jedan LAN server
		}
		srv, err := nettest.StartLANServer(p.Port, p.Token, ttl)
		if err != nil {
			return nil, err
		}
		a.lan = srv
		var addrs []string
		for _, ifc := range Interfaces() {
			addrs = append(addrs, ifc.Addrs...)
		}
		return protocol.LANServerResult{Port: srv.Port(), Addrs: addrs}, nil

	case protocol.CmdLANClient:
		var p protocol.LANClientParams
		if err := unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if _, _, err := net.SplitHostPort(p.Addr); err != nil {
			return nil, fmt.Errorf("neispravna adresa: %w", err)
		}
		secs := protocol.Clamp(p.Seconds, protocol.DefaultSeconds, protocol.MaxSeconds)
		streams := protocol.Clamp(p.Streams, protocol.DefaultStreams, protocol.MaxStreams)
		dir := protocol.NormalizeDirection(p.Direction)
		res := protocol.SpeedTestResult{Peer: p.Addr}
		progress(protocol.Progress{Phase: "latency"})
		lat := nettest.MeasureLatency(ctx, p.Addr, protocol.Clamp(p.LatencyCount, 5, protocol.MaxLatencyCount), 100*time.Millisecond, 3*time.Second)
		res.Latency = &lat
		if lat.Received == 0 {
			return nil, fmt.Errorf("drugi agent nije dostupan na %s (firewall ili druga mreža?)", p.Addr)
		}
		var err error
		if dir != "upload" {
			res.Download, err = nettest.RunTransferProgress(ctx, "download", streams, time.Duration(secs)*time.Second,
				nettest.LANWorker(p.Addr, p.Token, "down"), report("download"))
			if err != nil {
				return nil, fmt.Errorf("download: %w", err)
			}
		}
		if dir != "download" {
			res.Upload, err = nettest.RunTransferProgress(ctx, "upload", streams, time.Duration(secs)*time.Second,
				nettest.LANWorker(p.Addr, p.Token, "up"), report("upload"))
			if err != nil {
				return nil, fmt.Errorf("upload: %w", err)
			}
		}
		return res, nil
	}
	return nil, fmt.Errorf("nepoznata komanda %q", cmd)
}

// latency bira cilj i metodu merenja latencije.
func (a *Agent) latency(ctx context.Context, p protocol.LatencyParams, onProbe nettest.ProbeFunc) (protocol.LatencyResult, error) {
	count := protocol.Clamp(p.Count, protocol.DefaultLatencyCount, protocol.MaxLatencyCount)
	interval := time.Duration(protocol.Clamp(p.IntervalMS, 200, 2000)) * time.Millisecond
	if p.DurationSec > 0 {
		// Ping u trajanju: interval 200 ms–10 s, broj paketa iz trajanja.
		ms := p.IntervalMS
		if ms <= 0 {
			ms = 1000
		}
		ms = min(max(ms, 200), 10000)
		interval = time.Duration(ms) * time.Millisecond
		dur := min(p.DurationSec, protocol.MaxPingSeconds)
		count = min(max(dur*1000/ms, 1), protocol.MaxPingProbes)
	}
	res, err := a.measureLatency(ctx, p, count, interval, onProbe)
	if err == nil && ctx.Err() != nil {
		res.Stopped = true
	}
	return res, err
}

func (a *Agent) measureLatency(ctx context.Context, p protocol.LatencyParams, count int, interval time.Duration, onProbe nettest.ProbeFunc) (protocol.LatencyResult, error) {
	timeout := time.Duration(protocol.Clamp(p.TimeoutMS, 2000, 5000)) * time.Millisecond

	target := strings.TrimSpace(p.Target)
	method := p.Method
	switch {
	case target == "":
		target = a.consoleHostPort()
	case strings.EqualFold(target, "gateway"):
		gw, err := nettest.DefaultGateway()
		if err != nil {
			return protocol.LatencyResult{}, err
		}
		target = gw
	}
	_, _, splitErr := net.SplitHostPort(target)
	if method == "" {
		method = "icmp"
		if splitErr == nil {
			method = "tcp"
		}
	}
	switch method {
	case "tcp":
		if splitErr != nil {
			return protocol.LatencyResult{}, fmt.Errorf("za TCP merenje cilj mora biti u formatu host:port")
		}
		return nettest.MeasureLatencyProbe(ctx, target, count, interval, timeout, onProbe), nil
	case "icmp":
		if splitErr == nil {
			target, _, _ = net.SplitHostPort(target) // port se ignoriše
		}
		return nettest.MeasureICMPProbe(ctx, target, count, interval, timeout, onProbe)
	}
	return protocol.LatencyResult{}, fmt.Errorf("nepoznata metoda %q", method)
}

func (a *Agent) consoleHostPort() string {
	if a.base.Port() != "" {
		return a.base.Host
	}
	if a.base.Scheme == "https" {
		return net.JoinHostPort(a.base.Hostname(), "443")
	}
	return net.JoinHostPort(a.base.Hostname(), "80")
}

func unmarshal(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("neispravni parametri: %w", err)
	}
	return nil
}

// Interfaces vraća aktivne, ne-loopback interfejse sa adresama.
func Interfaces() []protocol.Interface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []protocol.Interface
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		pi := protocol.Interface{Name: ifc.Name, MAC: ifc.HardwareAddr.String()}
		for _, ad := range addrs {
			ipn, ok := ad.(*net.IPNet)
			if !ok || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			pi.Addrs = append(pi.Addrs, ipn.String())
		}
		if len(pi.Addrs) > 0 {
			out = append(out, pi)
		}
	}
	return out
}
