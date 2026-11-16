package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// Serveri: agent za servere (cmd/serveragent) javlja konzoli da je server na
// vezi i šalje stanje (uptime, CPU, RAM, diskovi) na 30 s. Konzola prati:
//   - pad: agent se ne javlja duže od zadatog vremena (podrazumevano 2 min);
//     tada se server i pinguje, da se zna da li je pao server ili samo agent;
//   - dugo visok CPU ili RAM (podrazumevano iznad 90% duže od 10 min);
//   - restart (uptime se smanjio).
// Obaveštenja idu kanalima koji imaju uključenu kolonu „Serveri”.

const (
	scopeServers   = "servers"
	srvHysteresis  = 5.0 // za „ponovo normalno” vrednost mora pasti ovoliko ispod praga
	srvSeriesPref  = "srv/"
	srvEventsFile  = "servers-events.jsonl"
	srvStateFile   = "servers-state.json"
	srvTickEvery   = 15 * time.Second
	srvMetricsLate = 3 * time.Minute // stanje starije od ovoga se ne prikazuje kao trenutno
)

func (c *NotifyConfig) serverDelay() time.Duration {
	if c.ServerDelayMinutes <= 0 {
		return 2 * time.Minute
	}
	return time.Duration(c.ServerDelayMinutes) * time.Minute
}

func (c *NotifyConfig) serverCPU() float64 { return pctOr(c.ServerCPUPct, 90) }
func (c *NotifyConfig) serverMem() float64 { return pctOr(c.ServerMemPct, 90) }
func (c *NotifyConfig) serverHigh() time.Duration {
	if c.ServerHighMinutes <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(c.ServerHighMinutes) * time.Minute
}

func pctOr(v, def int) float64 {
	if v <= 0 || v > 100 {
		return float64(def)
	}
	return float64(v)
}

func (c *NotifyConfig) validateServers() error {
	if c.ServerDelayMinutes < 0 || c.ServerDelayMinutes > 1440 {
		return errors.New("serveri: vreme do obaveštenja mora biti 1–1440 minuta")
	}
	for _, v := range []int{c.ServerCPUPct, c.ServerMemPct} {
		if v < 0 || v > 100 {
			return errors.New("serveri: prag zauzeća mora biti između 1 i 100%")
		}
	}
	if c.ServerHighMinutes < 0 || c.ServerHighMinutes > 1440 {
		return errors.New("serveri: trajanje visokog zauzeća mora biti 1–1440 minuta")
	}
	return nil
}

// srvState je stanje praćenja jednog servera (čuva se na disku).
type srvState struct {
	DownSince time.Time `json:"down_since,omitzero"`
	Alerted   bool      `json:"alerted,omitempty"` // poslato obaveštenje o padu
	CPUSince  time.Time `json:"cpu_since,omitzero"`
	CPUAlert  bool      `json:"cpu_alert,omitempty"`
	MemSince  time.Time `json:"mem_since,omitzero"`
	MemAlert  bool      `json:"mem_alert,omitempty"`
	Uptime    uint64    `json:"uptime,omitempty"` // poslednji uptime (za prepoznavanje restarta)
	UptimeAt  time.Time `json:"uptime_at,omitzero"`
}

type serverMonitor struct {
	log    *log.Logger
	hub    *Hub
	notify *notifier
	hist   func() *tsdb
	pingFn func(ctx context.Context, ip string) (float64, int, int)
	now    func() time.Time
	evPath string
	stPath string

	mu      sync.Mutex
	state   map[string]*srvState
	events  []NetEvent
	metrics map[string]srvMetrics
	live    map[string]srvLive // CPU i RAM na 5 s (samo za prikaz)
	saved   []byte
}

type srvLive struct {
	l  protocol.Live
	at time.Time
}

// srvLiveFresh: brzo stanje starije od ovoga se ne prikazuje (agent ga više
// ne šalje ili je veza prekinuta) – tada se prikazuje puno stanje.
const srvLiveFresh = 15 * time.Second

type srvMetrics struct {
	m  protocol.Metrics
	at time.Time
}

func newServerMonitor(dataDir string, hub *Hub, n *notifier, logger *log.Logger, hist func() *tsdb) *serverMonitor {
	s := &serverMonitor{
		log: logger, hub: hub, notify: n, hist: hist, pingFn: icmpCheck, now: time.Now,
		evPath: filepath.Join(dataDir, srvEventsFile), stPath: filepath.Join(dataDir, srvStateFile),
		state: map[string]*srvState{}, metrics: map[string]srvMetrics{}, live: map[string]srvLive{},
	}
	s.events = loadEvents(s.evPath)
	if b, err := os.ReadFile(s.stPath); err == nil && json.Unmarshal(b, &s.state) == nil {
		s.saved = b
	}
	if s.state == nil {
		s.state = map[string]*srvState{}
	}
	return s
}

func (s *serverMonitor) st(id string) *srvState {
	st := s.state[id]
	if st == nil {
		st = &srvState{}
		s.state[id] = st
	}
	return st
}

func (s *serverMonitor) saveLocked() {
	b, err := json.Marshal(s.state)
	if err != nil || string(b) == string(s.saved) {
		return
	}
	tmp := s.stPath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, s.stPath) == nil {
		s.saved = b
	}
}

func (s *serverMonitor) recordLocked(evs ...NetEvent) {
	if len(evs) == 0 {
		return
	}
	s.events = append(s.events, evs...)
	if len(s.events) > maxEvents {
		s.events = slices.Clone(s.events[len(s.events)-maxEvents:])
	}
	f, err := os.OpenFile(s.evPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, ev := range evs {
		s.log.Printf("serveri: %s", ev.Text)
		b, _ := json.Marshal(ev)
		f.Write(append(b, '\n'))
	}
}

// changed se poziva kada se agent za servere poveže ili izgubi vezu.
func (s *serverMonitor) changed(a AgentInfo) {
	now := s.now()
	s.mu.Lock()
	st := s.st(a.AgentID)
	var ev *NetEvent
	if a.Online {
		if !st.DownSince.IsZero() {
			d := now.Sub(st.DownSince)
			if st.Alerted {
				ev = &NetEvent{Time: now, Kind: "up", DeviceID: a.AgentID, Device: a.Hostname, DurationSec: int64(d.Seconds()),
					Text: fmt.Sprintf("%s je ponovo na vezi (nije bio %s)", a.Hostname, fmtDuration(d))}
				s.recordLocked(*ev)
			}
			st.DownSince, st.Alerted = time.Time{}, false
		}
	} else if st.DownSince.IsZero() {
		st.DownSince = now
	}
	s.saveLocked()
	s.mu.Unlock()
	if ev != nil {
		s.alert(*ev, fmt.Sprintf("OK SERVER %s ponovo online posle %s", a.Hostname, fmtDuration(time.Duration(ev.DurationSec)*time.Second)), false)
	}
}

// startup: serveri koji su bili na vezi pre restarta konzole, a ne vrate se
// u roku, javljaju se kao pali.
func (s *serverMonitor) startup(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if st := s.st(id); st.DownSince.IsZero() {
			st.DownSince = s.now()
		}
	}
	s.saveLocked()
}

// metricsIn obrađuje stanje koje je poslao agent.
// liveIn prima CPU i RAM na 5 s: samo za prikaz u tabeli i detaljima;
// istorija i obaveštenja koriste puno stanje (metricsIn, na 30 s).
func (s *serverMonitor) liveIn(id string, l protocol.Live) {
	now := s.now()
	s.mu.Lock()
	s.live[id] = srvLive{l: l, at: now}
	s.mu.Unlock()
}

func (s *serverMonitor) metricsIn(id string, m protocol.Metrics) {
	a, ok := s.hub.Agent(id)
	if !ok {
		return
	}
	now := s.now()
	cfg := s.notify.config()
	memPct := 0.0
	if m.MemTotal > 0 {
		memPct = 100 * float64(m.MemUsed) / float64(m.MemTotal)
	}
	if h := s.hist(); h != nil {
		h.Add(srvSeriesPref+id, now, []float64{m.CPUPct, memPct})
	}
	type pending struct {
		ev  NetEvent
		sms string
		bad bool
	}
	var out []pending
	s.mu.Lock()
	s.metrics[id] = srvMetrics{m: m, at: now}
	st := s.st(id)
	// Restart: uptime je manji nego što bi trebalo.
	if st.Uptime > 0 && !st.UptimeAt.IsZero() {
		expect := st.Uptime + uint64(now.Sub(st.UptimeAt).Seconds())
		if m.UptimeSec+120 < expect {
			ev := NetEvent{Time: now, Kind: "reboot", DeviceID: id, Device: a.Hostname,
				Text: fmt.Sprintf("%s je ponovo pokrenut (radi %s)", a.Hostname, fmtDuration(time.Duration(m.UptimeSec)*time.Second))}
			out = append(out, pending{ev, fmt.Sprintf("RESTART SERVER %s (radi %s)", a.Hostname, fmtDuration(time.Duration(m.UptimeSec)*time.Second)), true})
		}
	}
	st.Uptime, st.UptimeAt = m.UptimeSec, now
	check := func(name, kind string, v, limit float64, since *time.Time, alerted *bool) {
		switch {
		case v >= limit:
			if since.IsZero() {
				*since = now
			}
			if !*alerted && now.Sub(*since) >= cfg.serverHigh() {
				*alerted = true
				ev := NetEvent{Time: now, Kind: kind + "_high", DeviceID: id, Device: a.Hostname,
					Text:  fmt.Sprintf("%s: %s %.0f%% duže od %s (prag %.0f%%)", a.Hostname, name, v, fmtDuration(cfg.serverHigh()), limit),
					Procs: slices.Clone(m.Procs)}
				out = append(out, pending{ev, fmt.Sprintf("SERVER %s: %s %.0f%% duze od %s", a.Hostname, name, v, fmtDuration(cfg.serverHigh())), true})
			}
		case v < limit-srvHysteresis:
			if *alerted {
				ev := NetEvent{Time: now, Kind: kind + "_ok", DeviceID: id, Device: a.Hostname, DurationSec: int64(now.Sub(*since).Seconds()),
					Text: fmt.Sprintf("%s: %s ponovo normalan (%.0f%%)", a.Hostname, name, v)}
				out = append(out, pending{ev, fmt.Sprintf("OK SERVER %s: %s ponovo normalan (%.0f%%)", a.Hostname, name, v), false})
			}
			*since, *alerted = time.Time{}, false
		}
	}
	check("CPU", "cpu", m.CPUPct, cfg.serverCPU(), &st.CPUSince, &st.CPUAlert)
	check("RAM", "mem", memPct, cfg.serverMem(), &st.MemSince, &st.MemAlert)
	for _, p := range out {
		s.recordLocked(p.ev)
	}
	s.saveLocked()
	s.mu.Unlock()
	for _, p := range out {
		s.alert(p.ev, p.sms, p.bad)
	}
}

// tick proverava servere koji se ne javljaju.
func (s *serverMonitor) tick() {
	cfg := s.notify.config()
	now := s.now()
	type cand struct {
		a     AgentInfo
		since time.Time
	}
	var due []cand
	s.mu.Lock()
	for _, a := range s.hub.Agents() {
		if a.Role != protocol.RoleServer || a.Online {
			continue
		}
		st := s.st(a.AgentID)
		if st.DownSince.IsZero() {
			st.DownSince = now
		}
		if !st.Alerted && now.Sub(st.DownSince) >= cfg.serverDelay() {
			st.Alerted = true
			due = append(due, cand{a, st.DownSince})
		}
	}
	s.saveLocked()
	s.mu.Unlock()
	for _, c := range due {
		why := "ne odgovara ni na ping"
		if ip := serverIP(c.a); ip != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, _, recv := s.pingFn(ctx, ip); recv > 0 {
				why = "odgovara na ping, ali agent ne radi"
			}
			cancel()
		}
		ev := NetEvent{Time: now, Kind: "down", DeviceID: c.a.AgentID, Device: c.a.Hostname,
			Text: fmt.Sprintf("%s nije na vezi od %s (%s)", c.a.Hostname, c.since.Local().Format("15:04"), why)}
		s.mu.Lock()
		s.recordLocked(ev)
		s.mu.Unlock()
		s.alert(ev, fmt.Sprintf("SERVER %s OFFLINE od %s (%s)", c.a.Hostname, c.since.Local().Format("15:04"), why), true)
	}
}

func (s *serverMonitor) run() {
	t := time.NewTicker(srvTickEvery)
	defer t.Stop()
	for range t.C {
		s.tick()
	}
}

// alert šalje obaveštenje kanalima sa uključenim „Serveri”.
func (s *serverMonitor) alert(ev NetEvent, sms string, bad bool) {
	cfg := s.notify.config()
	if !bad && cfg.SkipRecovery {
		return
	}
	if !cfg.anyFor(scopeServers) {
		return
	}
	now := s.now()
	msg := notifyMessage{
		Title: "Network Monitor: " + ev.Text, Body: "• " + ev.Text + "\n\nNetwork Monitor, " + now.Local().Format("02.01.2006. 15:04"),
		Down: bad, Recovery: !bad, Scope: scopeServers,
		SMS: truncSMS(smsASCII("NetMon " + now.Local().Format("15:04") + " " + sms)),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.notify.deliver(ctx, cfg, msg)
	}()
}

// serverIP: adresa sa koje se agent javio (za ping), inače prva adresa interfejsa.
func serverIP(a AgentInfo) string {
	if h, _, err := net.SplitHostPort(a.RemoteAddr); err == nil && h != "" && !strings.HasPrefix(h, "127.") {
		return h
	}
	for _, ifc := range a.Interfaces {
		for _, ad := range ifc.Addrs {
			if ip, _, err := net.ParseCIDR(ad); err == nil && ip.To4() != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return ""
}

// forget briše server (istoriju, stanje).
func (s *serverMonitor) forget(id string) {
	s.mu.Lock()
	delete(s.state, id)
	delete(s.metrics, id)
	s.saveLocked()
	s.mu.Unlock()
	if h := s.hist(); h != nil {
		h.DeletePrefix(srvSeriesPref + id)
	}
}

// ---------- prikaz ----------

type serverView struct {
	AgentID  string    `json:"agent_id"`
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	Since    time.Time `json:"since,omitzero"` // na vezi od / nije na vezi od
	LastSeen time.Time `json:"last_seen,omitzero"`
	IP       string    `json:"ip,omitempty"`
	Location string    `json:"location,omitempty"`
	Version  string    `json:"version,omitempty"`
	// Izdanje agenta i da li je potrebna nadogradnja (vidi protocol.WantRelease).
	Release     int               `json:"release"`
	WantRelease int               `json:"want_release"`
	Outdated    bool              `json:"outdated"`
	Metrics     *protocol.Metrics `json:"metrics,omitempty"`
	MetricsAt   time.Time         `json:"metrics_at,omitzero"`
	LiveAt      time.Time         `json:"live_at,omitzero"` // CPU i RAM su sa brzog čitanja (5 s)
	MemPct      *float64          `json:"mem_pct,omitempty"`
	CPUHigh     bool              `json:"cpu_high,omitempty"`
	MemHigh     bool              `json:"mem_high,omitempty"`
}

type serversResponse struct {
	Servers   []serverView `json:"servers"`
	CPUPct    float64      `json:"cpu_pct"`
	MemPct    float64      `json:"mem_pct"`
	HighMin   int          `json:"high_min"`
	DelayMin  int          `json:"delay_min"`
	Notifying bool         `json:"notifying"` // bar jedan kanal šalje za servere
}

func (s *serverMonitor) list(locate func(AgentInfo) string) serversResponse {
	cfg := s.notify.config()
	resp := serversResponse{Servers: []serverView{}, CPUPct: cfg.serverCPU(), MemPct: cfg.serverMem(),
		HighMin: int(cfg.serverHigh() / time.Minute), DelayMin: int(cfg.serverDelay() / time.Minute), Notifying: cfg.anyFor(scopeServers)}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.hub.Agents() {
		if a.Role != protocol.RoleServer {
			continue
		}
		v := serverView{AgentID: a.AgentID, Name: a.Hostname, Online: a.Online, LastSeen: a.LastSeen, IP: serverIP(a), Version: a.Version,
			Release: a.Release, WantRelease: protocol.ServerAgentRelease, Outdated: a.Release < protocol.ServerAgentRelease}
		if locate != nil {
			v.Location = locate(a)
		}
		st := s.st(a.AgentID)
		if a.Online {
			v.Since = a.ConnectedAt
		} else {
			v.Since = st.DownSince
			if v.Since.IsZero() {
				v.Since = a.LastSeen
			}
		}
		if mm, ok := s.metrics[a.AgentID]; ok && a.Online && now.Sub(mm.at) < srvMetricsLate {
			m := mm.m
			v.Metrics, v.MetricsAt = &m, mm.at
			if lv, ok := s.live[a.AgentID]; ok && lv.at.After(mm.at) && now.Sub(lv.at) < srvLiveFresh {
				m.CPUPct, m.MemTotal, m.MemUsed = lv.l.CPUPct, lv.l.MemTotal, lv.l.MemUsed
				v.LiveAt = lv.at
			}
			if m.MemTotal > 0 {
				v.MemPct = ptr(math.Round(1000*float64(m.MemUsed)/float64(m.MemTotal)) / 10)
			}
		}
		v.CPUHigh, v.MemHigh = st.CPUAlert, st.MemAlert
		resp.Servers = append(resp.Servers, v)
	}
	sort.Slice(resp.Servers, func(i, j int) bool {
		return strings.ToLower(resp.Servers[i].Name) < strings.ToLower(resp.Servers[j].Name)
	})
	return resp
}

func (s *serverMonitor) recentEvents(id string, n int) []NetEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []NetEvent{}
	for i := len(s.events) - 1; i >= 0 && len(out) < n; i-- {
		if id == "" || s.events[i].DeviceID == id {
			out = append(out, s.events[i])
		}
	}
	return out
}

// ---------- HTTP ----------

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	_, loc := s.currentLocations()
	writeJSON(w, http.StatusOK, s.srv.list(loc.locate))
}

func (s *Server) handleServerEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.srv.recentEvents(r.URL.Query().Get("id"), 300))
}

// handleServerHistory: CPU i RAM (%) za period (?range=15m|1h|6h|24h|7d|30d
// ili range=custom&from=&to=; format=xlsx za Excel).
func (s *Server) handleServerHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.hub.Agent(id)
	if !ok || a.Role != protocol.RoleServer {
		writeErr(w, http.StatusNotFound, errors.New("server nije pronađen"))
		return
	}
	q := r.URL.Query()
	from, to, agg, err := historyWindow(q, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	h := s.net.hist
	if h == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("istorija nije dostupna"))
		return
	}
	pts, err := h.Query(srvSeriesPref+id, from, to, agg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if q.Get("format") == "xlsx" {
		historyExport(w, a.Hostname, "CPU i RAM", seriesSrv, from, to, agg, pts)
		return
	}
	out := make([][]any, 0, len(pts))
	for _, p := range pts {
		row := []any{p.T}
		for _, v := range p.V {
			if math.IsNaN(v) {
				row = append(row, nil)
			} else {
				row = append(row, math.Round(v*10)/10)
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from.Unix(), "to": to.Unix(), "agg": agg, "points": out})
}

func (s *Server) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.hub.Agent(id)
	if !ok || a.Role != protocol.RoleServer {
		writeErr(w, http.StatusNotFound, errors.New("server nije pronađen"))
		return
	}
	if err := s.hub.Forget(id); err != nil {
		writeErr(w, http.StatusConflict, errors.New("server je na vezi; prvo deinstalirajte agenta za servere"))
		return
	}
	s.srv.forget(id)
	w.WriteHeader(http.StatusNoContent)
}
