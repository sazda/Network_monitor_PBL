package console

import (
	"math"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// Prikaz merenja brzine uživo (skazaljka u web interfejsu).
//
// Test „Brzina” (agent <-> konzola): svi podaci prolaze kroz konzolu, pa ona
// sama meri trenutnu brzinu — radi i sa starijim agentima.
// LAN test (agent <-> agent): podaci ne prolaze kroz konzolu, pa trenutnu
// brzinu javlja agent (poruka „progress”, od nove verzije agenta).

const (
	liveSample    = 250 * time.Millisecond // korak merenja na konzoli
	liveWindow    = time.Second            // brzina = prosek poslednje sekunde
	liveMaxPoints = 400
	liveKeep      = 10 // koliko poslednjih testova se pamti
)

// liveProbe je jedno merenje latencije (jedan paket).
type liveProbe struct {
	Seq  int       `json:"seq"`
	RTT  float64   `json:"rtt_ms,omitempty"`
	Lost bool      `json:"lost,omitempty"`
	At   time.Time `json:"-"` // kada je konzola primila rezultat paketa

	// Samo u sažetom prikazu dugog pinga (grupa uzastopnih paketa):
	SeqEnd int     `json:"seq_end,omitempty"`
	MaxRTT float64 `json:"max_ms,omitempty"`
	LostN  int     `json:"lost_n,omitempty"`
}

const liveMaxProbes = 400 // najviše tačaka grafikona u odgovoru /api/live

type livePoint struct {
	T     float64 `json:"t"` // sekunde od početka testa
	Mbps  float64 `json:"mbps"`
	Phase string  `json:"phase"`
}

// liveView je stanje jednog testa za web interfejs.
type liveView struct {
	ID        string      `json:"id"`
	Kind      string      `json:"kind"` // speedtest | lan | latency | ping
	AgentID   string      `json:"agent_id"`
	Host      string      `json:"host"`
	PeerID    string      `json:"peer_id,omitempty"`
	PeerHost  string      `json:"peer_host,omitempty"`
	User      string      `json:"user,omitempty"`
	Started   time.Time   `json:"started"`
	Finished  time.Time   `json:"finished,omitzero"`
	Seconds   int         `json:"seconds"`
	Direction string      `json:"direction"`
	Phase     string      `json:"phase"` // start | latency | download | upload | done | error
	Mbps      float64     `json:"mbps"`
	Live      bool        `json:"live"` // stižu međurezultati
	Points    []livePoint `json:"points"`
	Download  float64     `json:"download_mbps,omitempty"`
	Upload    float64     `json:"upload_mbps,omitempty"`
	LatencyMS float64     `json:"latency_ms,omitempty"`
	JitterMS  float64     `json:"jitter_ms,omitempty"`
	LossPct   float64     `json:"loss_pct"`           // gubitak paketa pri merenju latencije
	Sent      int         `json:"sent,omitempty"`     // poslato / primljeno probnih paketa
	Received  int         `json:"received,omitempty"` //
	Error     string      `json:"error,omitempty"`
	// Request su parametri testa (za dugme „Ponovi test”).
	Request any `json:"request,omitempty"`

	// Samo za merenje latencije:
	Target  string      `json:"target,omitempty"` // cilj (posle merenja: stvarna adresa)
	Method  string      `json:"method,omitempty"` // icmp | tcp
	Count   int         `json:"count,omitempty"`  // planirani broj merenja
	Probes  []liveProbe `json:"probes,omitempty"`
	MinMS   float64     `json:"min_ms,omitempty"`
	MaxMS   float64     `json:"max_ms,omitempty"`
	LostSeq bool        `json:"lost_seq,omitempty"` // poznato je koji su paketi izgubljeni

	// Samo za ping u trajanju:
	DurationSec int       `json:"duration_sec,omitempty"`
	IntervalMS  int       `json:"interval_ms,omitempty"`
	ResultID    string    `json:"result_id,omitempty"` // za preuzimanje izveštaja
	Stats       *latStats `json:"stats,omitempty"`
	Stopping    bool      `json:"stopping,omitempty"` // zatraženo zaustavljanje
	Stopped     bool      `json:"stopped,omitempty"`  // zaustavljeno pre kraja
	CanStop     bool      `json:"can_stop,omitempty"`
	FromConsole bool      `json:"from_console,omitempty"` // ping šalje konzola (ka računaru)
	Continuous  bool      `json:"continuous,omitempty"`   // neprekidni ping (do zaustavljanja)
	Device      bool      `json:"device,omitempty"`       // ping mrežnog uređaja (switch, ruter...)
}

type liveTest struct {
	v  liveView
	ip string // adresa agenta (za brojanje bajtova testa „Brzina”)

	counters map[string]*atomic.Int64 // download/upload, broji konzola
	hist     []struct {
		t time.Time
		n int64
	}
	stop chan struct{}

	cancel func() // zaustavlja ping (nil = ne može se zaustaviti)
}

type liveTracker struct {
	mu    sync.Mutex
	tests []*liveTest // najnoviji poslednji
}

func newLiveTracker() *liveTracker { return &liveTracker{} }

// start beleži novi test. ip je adresa agenta kako je konzola vidi (samo za
// test „Brzina”, inače prazno).
func (lt *liveTracker) start(v liveView, ip string) *liveTest {
	if v.ID == "" {
		v.ID = newID()
	}
	v.Started = time.Now()
	v.Phase = "start"
	v.Points = []livePoint{}
	t := &liveTest{v: v, ip: ip, stop: make(chan struct{})}
	if ip != "" {
		t.counters = map[string]*atomic.Int64{"download": {}, "upload": {}}
		go lt.sample(t)
	}
	lt.mu.Lock()
	lt.tests = append(lt.tests, t)
	if len(lt.tests) > liveKeep {
		// Izbacuje najstariji završeni test.
		if i := slices.IndexFunc(lt.tests, func(x *liveTest) bool { return !x.v.Finished.IsZero() }); i >= 0 {
			lt.tests = slices.Delete(lt.tests, i, i+1)
		}
	}
	lt.mu.Unlock()
	return t
}

func (lt *liveTracker) addPoint(t *liveTest, phase string, mbps float64) {
	t.v.Phase, t.v.Mbps, t.v.Live = phase, mbps, true
	if phase == "download" || phase == "upload" {
		if len(t.v.Points) >= liveMaxPoints {
			t.v.Points = t.v.Points[1:]
		}
		t.v.Points = append(t.v.Points, livePoint{
			T: math.Round(time.Since(t.v.Started).Seconds()*100) / 100, Mbps: mbps, Phase: phase,
		})
	}
}

// progress obrađuje međurezultat koji je poslao agent.
func (lt *liveTracker) progress(t *liveTest, p protocol.Progress) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if !t.v.Finished.IsZero() {
		return
	}
	if p.Phase == "probe" {
		limit := protocol.MaxLatencyCount
		if t.v.Kind == "ping" {
			limit = protocol.MaxPingProbes
		}
		if (t.v.Kind == "latency" || t.v.Kind == "ping") && len(t.v.Probes) < limit {
			t.v.Phase, t.v.Live, t.v.LostSeq = "latency", true, true
			t.v.Probes = append(t.v.Probes, liveProbe{Seq: p.Seq, RTT: p.RTTMS, Lost: p.Lost, At: time.Now()})
		}
		return
	}
	if t.counters == nil {
		lt.addPoint(t, p.Phase, p.Mbps)
	}
}

// finishLatency beleži konačan rezultat merenja latencije. Konačni podaci
// agenta zamenjuju međurezultate (stariji agenti šalju samo njih).
func (lt *liveTracker) finishLatency(t *liveTest, res *protocol.LatencyResult, err error) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	t.v.Finished = time.Now()
	if err != nil {
		t.v.Phase, t.v.Error = "error", err.Error()
		return
	}
	t.v.Phase = "done"
	t.v.Target, t.v.Method = res.Target, res.Method
	t.v.Sent, t.v.Received, t.v.LossPct = res.Sent, res.Received, res.LossPct
	t.v.LatencyMS, t.v.JitterMS, t.v.MinMS, t.v.MaxMS = res.AvgMS, res.JitterMS, res.MinMS, res.MaxMS
	t.v.Stopped = res.Stopped
	// Redosled paketa: uspešni odgovori redom, izgubljeni na svojim mestima
	// (ako ih agent javlja; stariji agenti javljaju samo broj izgubljenih).
	// Vreme prijema iz međurezultata se zadržava (za izveštaj).
	at := map[int]time.Time{}
	for _, p := range t.v.Probes {
		at[p.Seq] = p.At
	}
	probes, lostSeq := probesFromResult(res, time.Time{})
	for i := range probes {
		probes[i].At = at[probes[i].Seq]
	}
	t.v.LostSeq = lostSeq
	t.v.Probes = probes
}

// probeTimes vraća vreme slanja svakog paketa (ms od početka testa), ako su
// stigli međurezultati za sve pakete; inače nil.
func (lt *liveTracker) probeTimes(t *liveTest) []int64 {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	out := make([]int64, 0, len(t.v.Probes))
	for _, p := range t.v.Probes {
		if p.At.IsZero() {
			return nil
		}
		out = append(out, p.At.Sub(t.v.Started).Milliseconds())
	}
	return out
}

// requestStop zaustavlja ping (vraća false ako test nije u toku).
func (lt *liveTracker) requestStop(id string, user string, admin bool) (bool, error) {
	lt.mu.Lock()
	var t *liveTest
	for _, x := range lt.tests {
		if x.v.ID == id {
			t = x
		}
	}
	if t == nil || !t.v.Finished.IsZero() || t.cancel == nil {
		lt.mu.Unlock()
		return false, nil
	}
	if !admin && t.v.User != user {
		lt.mu.Unlock()
		return false, errForbiddenStop
	}
	t.v.Stopping = true
	cancel := t.cancel
	lt.mu.Unlock()
	cancel()
	return true, nil
}

// counter vraća brojač bajtova za HTTP test brzine sa adrese ip (nil ako za
// tu adresu nema testa u toku). Prvi zahtev za smer prebacuje test u tu fazu.
func (lt *liveTracker) counter(ip, phase string) *atomic.Int64 {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	for _, t := range lt.tests {
		if t.ip == ip && t.counters != nil && t.v.Finished.IsZero() {
			if t.v.Phase != phase {
				t.v.Phase, t.v.Mbps, t.hist = phase, 0, nil
			}
			return t.counters[phase]
		}
	}
	return nil
}

// sample periodično računa brzinu testa „Brzina” iz bajtova koje broji konzola.
func (lt *liveTracker) sample(t *liveTest) {
	tk := time.NewTicker(liveSample)
	defer tk.Stop()
	for {
		select {
		case <-t.stop:
			return
		case now := <-tk.C:
			lt.mu.Lock()
			phase := t.v.Phase
			if c := t.counters[phase]; c != nil {
				n := c.Load()
				t.hist = append(t.hist, struct {
					t time.Time
					n int64
				}{now, n})
				for len(t.hist) > 1 && now.Sub(t.hist[0].t) > liveWindow {
					t.hist = t.hist[1:]
				}
				if len(t.hist) > 1 {
					first := t.hist[0]
					if secs := now.Sub(first.t).Seconds(); secs > 0 {
						lt.addPoint(t, phase, math.Round(float64(n-first.n)*8/secs/1e6*10)/10)
					}
				}
			}
			lt.mu.Unlock()
		}
	}
}

// finish beleži konačan rezultat testa.
func (lt *liveTracker) finish(t *liveTest, res *protocol.SpeedTestResult, err error) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if t.counters != nil {
		close(t.stop)
	}
	t.v.Finished = time.Now()
	t.v.Mbps = 0
	if err != nil {
		t.v.Phase, t.v.Error = "error", err.Error()
		return
	}
	t.v.Phase = "done"
	if res != nil {
		if res.Download != nil {
			t.v.Download = res.Download.Mbps
		}
		if res.Upload != nil {
			t.v.Upload = res.Upload.Mbps
		}
		if l := res.Latency; l != nil {
			t.v.Sent, t.v.Received, t.v.LossPct = l.Sent, l.Received, l.LossPct
			if l.Received > 0 {
				t.v.LatencyMS, t.v.JitterMS = l.AvgMS, l.JitterMS
			}
		}
	}
}

// snapshot vraća poslednje testove, najnoviji prvi.
func (lt *liveTracker) snapshot() []liveView {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	out := make([]liveView, 0, len(lt.tests))
	for i := len(lt.tests) - 1; i >= 0; i-- {
		t := lt.tests[i]
		v := t.v
		v.Points = slices.Clone(v.Points)
		if v.Kind == "latency" || v.Kind == "ping" {
			st := computeLatStats(v.Probes)
			v.Stats = &st
		}
		v.Probes = downsample(v.Probes, liveMaxProbes)
		v.CanStop = t.cancel != nil && v.Finished.IsZero()
		out = append(out, v)
	}
	return out
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tests": s.live.snapshot()})
}

// remoteIP vraća IP adresu iz host:port.
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
