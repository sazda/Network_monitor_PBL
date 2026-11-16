package console

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// Ping u trajanju: korisnik zada cilj, trajanje i interval; merenje teče na
// serveru u pozadini (ne zavisi od otvorenog pretraživača), može se zaustaviti,
// a po završetku se preuzima izveštaj (CSV za Excel ili HTML za štampu).

// PingRequest su parametri pinga.
type PingRequest struct {
	// Source: "" ili "console" = konzola pinguje taj računar (njegovu adresu);
	// "agent" = agent pinguje Target.
	Source      string `json:"source,omitempty"`
	Target      string `json:"target"`       // samo za source=agent: host ili IP; host:port za TCP; "gateway"
	Method      string `json:"method"`       // icmp | tcp | "" (automatski)
	IntervalMS  int    `json:"interval_ms"`  // 200 ms – 10 s (podrazumevano 1 s)
	DurationSec int    `json:"duration_sec"` // 5 s – 24 h
	// Continuous: neprekidni ping (kao „ping -t”) dok ga korisnik ne zaustavi;
	// iz bezbednosnih razloga se sam zaustavlja posle 24 h. DurationSec se ignoriše.
	Continuous bool `json:"continuous,omitempty"`
}

const pingLockKey = "ping:" // ključ zaključavanja za ping sa konzole

var errForbiddenStop = errors.New("ping može da zaustavi samo korisnik koji ga je pokrenuo ili administrator")

// pingTargetLabel je opis cilja za prikaz pre nego što agent javi stvarnu adresu.
func pingTargetLabel(target string, a AgentInfo) string {
	switch {
	case target == "":
		return "konzola"
	case strings.EqualFold(target, "gateway"):
		if a.Gateway != "" {
			return "gateway " + a.Gateway
		}
		return "gateway"
	}
	return target
}

// pingRequest čita i proverava parametre pinga (na grešku odgovara sam).
func (s *Server) pingRequest(w http.ResponseWriter, r *http.Request) (PingRequest, bool) {
	var req PingRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return req, false
	}
	req.Target = strings.TrimSpace(req.Target)
	if req.IntervalMS <= 0 {
		req.IntervalMS = 1000
	}
	if req.IntervalMS < 200 || req.IntervalMS > 10000 {
		writeErr(w, http.StatusBadRequest, errors.New("interval mora biti između 0,2 i 10 sekundi"))
		return req, false
	}
	if req.Continuous {
		req.DurationSec = protocol.MaxPingSeconds
	}
	if req.DurationSec < 5 || req.DurationSec > protocol.MaxPingSeconds {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("trajanje mora biti između 5 sekundi i %d sati", protocol.MaxPingSeconds/3600))
		return req, false
	}
	return req, true
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	req, ok := s.pingRequest(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	a, ok := s.hub.Agent(id)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("nepoznat računar"))
		return
	}
	if req.Source != "agent" {
		s.pingFromConsole(w, r, req, a)
		return
	}
	if !a.Online {
		writeErr(w, http.StatusConflict, ErrOffline)
		return
	}
	release := s.lockAgents(w, r, "ping", id)
	if release == nil {
		return
	}
	user := identityFrom(r).Name
	count := min(req.DurationSec*1000/req.IntervalMS, protocol.MaxPingProbes)
	params := protocol.LatencyParams{
		Target: req.Target, Method: req.Method, IntervalMS: req.IntervalMS, DurationSec: req.DurationSec,
		Count: min(count, protocol.MaxLatencyCount), // za starije agente
	}
	resID := newID()
	shown := count
	if !slices.Contains(a.Features, protocol.FeaturePing) {
		// Stariji agent ne zna za trajanje: meri najviše 100 puta (interval ≤ 2 s).
		shown = params.Count
	}
	lt := s.live.start(liveView{
		Kind: "ping", AgentID: id, Host: a.Hostname, User: user,
		Target: pingTargetLabel(req.Target, a), Method: req.Method, Count: shown,
		DurationSec: req.DurationSec, IntervalMS: req.IntervalMS, ResultID: resID, Request: req,
	}, "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.DurationSec)*time.Second+5*time.Minute)
	var cmdMu sync.Mutex
	var cmdID string
	s.live.setCancel(lt, func() {
		cmdMu.Lock()
		c := cmdID
		cmdMu.Unlock()
		if c != "" && slices.Contains(a.Features, protocol.FeaturePing) {
			// Agent prekida merenje i vraća dotadašnji rezultat; ako ne
			// odgovori, posle 20 s se odustaje od čekanja.
			if s.hub.Cancel(id, c) == nil {
				time.AfterFunc(20*time.Second, cancel)
				return
			}
		}
		cancel()
	})

	go func() {
		defer release()
		defer cancel()
		var out protocol.LatencyResult
		err := s.hub.callFull(ctx, id, protocol.CmdLatency, params, &out,
			func(p protocol.Progress) { s.live.progress(lt, p) },
			func(c string) { cmdMu.Lock(); cmdID = c; cmdMu.Unlock() })
		if err != nil && lt.isStopping(s.live) && errors.Is(err, context.Canceled) {
			err = errors.New("ping je zaustavljen pre kraja (agent nije vratio rezultat)")
		}
		if err == nil {
			out.TimesMS = s.live.probeTimes(lt)
		}
		s.live.finishLatency(lt, &out, err)
		raw, _ := json.Marshal(req)
		s.saveResult(Result{ID: resID, AgentID: id, Kind: "ping", Params: raw, Started: lt.v.Started}, user, out, err)
		s.log.Printf("ping %s -> %s (%s): poslato %d, primljeno %d, greška: %v", a.Hostname, req.Target, user, out.Sent, out.Received, err)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"live_id": lt.v.ID, "result_id": resID})
}

// pingFromConsole: konzola pinguje računar (ICMP) na adresu sa koje se agent
// povezuje. Radi i kada agent nije na vezi (računar može da radi i bez njega).
func (s *Server) pingFromConsole(w http.ResponseWriter, r *http.Request, req PingRequest, a AgentInfo) {
	ip := remoteIP(a.RemoteAddr)
	if ip == "" {
		writeErr(w, http.StatusConflict, errors.New("adresa računara nije poznata (agent se još nije povezao)"))
		return
	}
	s.consolePing(w, r, req, consolePingTarget{ID: a.AgentID, Host: a.Hostname, IP: ip})
}

// consolePingTarget je cilj pinga sa konzole: računar (agent) ili mrežni uređaj.
type consolePingTarget struct {
	ID     string // ID agenta ili netdevPrefix+ID uređaja (za rezultat i zaključavanje)
	Host   string
	IP     string
	Device bool
}

// consolePing pokreće ping sa konzole u pozadini i odmah odgovara (202).
func (s *Server) consolePing(w http.ResponseWriter, r *http.Request, req PingRequest, t consolePingTarget) {
	// Ping sa konzole ne opterećuje računar, pa ne blokira druga merenja na
	// njemu – samo drugi ping istog računara.
	release := s.lockAgents(w, r, "ping", pingLockKey+t.ID)
	if release == nil {
		return
	}
	ip := t.IP
	req.Source, req.Target, req.Method = "console", ip, "icmp"
	user := identityFrom(r).Name
	count := min(req.DurationSec*1000/req.IntervalMS, protocol.MaxPingProbes)
	resID := newID()
	lt := s.live.start(liveView{
		Kind: "ping", AgentID: t.ID, Host: t.Host, User: user, FromConsole: true, Continuous: req.Continuous, Device: t.Device,
		Target: ip, Method: "icmp", Count: count, DurationSec: req.DurationSec, IntervalMS: req.IntervalMS,
		ResultID: resID, Request: req,
	}, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.DurationSec)*time.Second+5*time.Minute)
	s.live.setCancel(lt, cancel)
	s.pings.Add(1)
	go func() {
		defer s.pings.Done()
		defer release()
		defer cancel()
		res, err := s.icmpPing(ctx, ip, count, time.Duration(req.IntervalMS)*time.Millisecond, 2*time.Second,
			func(seq int, rtt float64, ok bool) {
				s.live.progress(lt, protocol.Progress{Phase: "probe", Seq: seq, RTTMS: rtt, Lost: !ok})
			})
		if err == nil {
			res.Stopped = lt.isStopping(s.live)
			res.TimesMS = s.live.probeTimes(lt)
		}
		s.live.finishLatency(lt, &res, err)
		raw, _ := json.Marshal(req)
		s.saveResult(Result{ID: resID, AgentID: t.ID, Hostname: t.Host, Kind: "ping", Params: raw, Started: lt.v.Started}, user, res, err)
		s.log.Printf("ping konzola -> %s (%s, %s): poslato %d, primljeno %d, greška: %v", t.Host, ip, user, res.Sent, res.Received, err)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"live_id": lt.v.ID, "result_id": resID})
}

func (t *liveTest) isStopping(lt *liveTracker) bool {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return t.v.Stopping
}

func (lt *liveTracker) setCancel(t *liveTest, fn func()) {
	lt.mu.Lock()
	t.cancel = fn
	lt.mu.Unlock()
}

func (s *Server) handleStopLive(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	ok, err := s.live.requestStop(r.PathValue("id"), id.Name, id.Role == RoleAdmin)
	switch {
	case err != nil:
		writeErr(w, http.StatusForbidden, err)
	case !ok:
		writeErr(w, http.StatusNotFound, errors.New("ping nije u toku"))
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"stopping": true})
	}
}

// ---------- izveštaj ----------

type pingReport struct {
	Res        Result
	Title      string // npr. „konzola → INF-PC (10.0.0.5)”
	Source     string
	Continuous bool
	Data       protocol.LatencyResult
	Host       string
	Target     string
	Method     string
	Start      time.Time
	End        time.Time
	Interval   time.Duration
	Exact      bool // vremena paketa su izmerena (a ne procenjena)
	Probes     []liveProbe
	Stats      latStats
	Outages    []outage
	LostSeq    bool
}

func (s *Server) loadReport(id string) (*pingReport, error) {
	res, ok := s.store.Get(id)
	if !ok {
		return nil, errors.New("rezultat nije pronađen (izveštaj je dostupan za poslednjih 5000 merenja)")
	}
	if res.Kind != "ping" && res.Kind != "latency" {
		return nil, errors.New("izveštaj postoji samo za ping i merenje latencije")
	}
	if !res.OK {
		return nil, fmt.Errorf("merenje nije uspelo: %s", res.Error)
	}
	rep := &pingReport{Res: res, Host: res.Hostname, End: res.Time.Local(), Interval: time.Second}
	if err := json.Unmarshal(res.Data, &rep.Data); err != nil {
		return nil, err
	}
	var p struct {
		IntervalMS int    `json:"interval_ms"`
		Source     string `json:"source"`
		Continuous bool   `json:"continuous"`
	}
	json.Unmarshal(res.Params, &p)
	rep.Source, rep.Continuous = p.Source, p.Continuous
	if p.IntervalMS > 0 {
		rep.Interval = time.Duration(p.IntervalMS) * time.Millisecond
	} else if res.Kind == "latency" {
		rep.Interval = 200 * time.Millisecond // podrazumevano kod merenja latencije
	}
	rep.Start = res.Started.Local()
	if res.Started.IsZero() {
		rep.Start = rep.End.Add(-time.Duration(rep.Data.Sent) * rep.Interval)
	}
	rep.Exact = len(rep.Data.TimesMS) == rep.Data.Sent && rep.Data.Sent > 0
	rep.Probes, rep.LostSeq = probesFromResult(&rep.Data, rep.Start)
	if !rep.Exact {
		for i := range rep.Probes {
			rep.Probes[i].At = rep.Start.Add(time.Duration(i) * rep.Interval)
		}
	}
	rep.Stats = computeLatStats(rep.Probes)
	rep.Outages = outages(rep.Probes)
	if !rep.LostSeq {
		rep.Outages = nil // pozicije izgubljenih paketa nisu poznate (stariji agent)
		rep.Stats.Outages, rep.Stats.Longest = 0, nil
	}
	rep.Target, rep.Method = rep.Data.Target, strings.ToUpper(rep.Data.Method)
	if rep.Source == "console" {
		rep.Title = fmt.Sprintf("konzola → %s (%s)", rep.Host, rep.Target)
	} else {
		rep.Title = fmt.Sprintf("%s → %s", rep.Host, rep.Target)
	}
	return rep, nil
}

// stopNote opisuje kako se ping završio (prazno = isteklo zadato trajanje).
func (rep *pingReport) stopNote() string {
	switch {
	case rep.Continuous && rep.Data.Stopped:
		return "Neprekidni ping, zaustavljen ručno."
	case rep.Continuous:
		return fmt.Sprintf("Neprekidni ping je zaustavljen automatski posle %d h (najduže dozvoljeno trajanje).", protocol.MaxPingSeconds/3600)
	case rep.Data.Stopped:
		return "Ping je zaustavljen pre isteka zadatog trajanja."
	}
	return ""
}

// StopNote se koristi u HTML šablonu.
func (rep *pingReport) StopNote() string { return rep.stopNote() }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func reportFilename(rep *pingReport, ext string) string {
	host := unsafeName.ReplaceAllString(rep.Host, "_")
	return fmt.Sprintf("ping-%s-%s.%s", host, rep.Start.Format("20060102-1504"), ext)
}

// decimal formatira broj sa decimalnim zarezom (srpski Excel).
func decimal(v float64, prec int) string {
	return strings.Replace(strconv.FormatFloat(v, 'f', prec, 64), ".", ",", 1)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%d h %d min %d s", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%d min %d s", m, sec)
	}
	return fmt.Sprintf("%d s", sec)
}

func (s *Server) handleReportCSV(w http.ResponseWriter, r *http.Request) {
	rep, err := s.loadReport(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var b bytes.Buffer
	b.WriteString("\xef\xbb\xbf") // BOM: Excel prepoznaje UTF-8 (naša slova)
	cw := csv.NewWriter(&b)
	cw.Comma = ';' // srpska podešavanja Excel-a koriste ; kao separator
	st := rep.Stats
	rows := [][]string{
		{"Izveštaj pinga - Network Monitor"},
		{"Ping", rep.Title},
		{"Računar", rep.Host},
		{"Adresa", rep.Target},
		{"Metoda", rep.Method},
		{"Početak", rep.Start.Format("02.01.2006. 15:04:05")},
		{"Kraj", rep.End.Format("02.01.2006. 15:04:05")},
		{"Trajanje", fmtDur(rep.End.Sub(rep.Start))},
		{"Interval (s)", decimal(rep.Interval.Seconds(), 1)},
		{"Pokrenuo", rep.Res.User},
		{"Poslato", strconv.Itoa(st.Sent)},
		{"Primljeno", strconv.Itoa(st.Received)},
		{"Izgubljeno", strconv.Itoa(st.Lost)},
		{"Gubitak (%)", decimal(st.LossPct, 2)},
		{"Min (ms)", decimal(st.MinMS, 2)},
		{"Prosek (ms)", decimal(st.AvgMS, 2)},
		{"Max (ms)", decimal(st.MaxMS, 2)},
		{"Jitter (ms)", decimal(st.JitterMS, 2)},
		{"95% ispod (ms)", decimal(st.P95, 2)},
		{"Broj prekida", strconv.Itoa(st.Outages)},
	}
	if n := rep.stopNote(); n != "" {
		rows = append(rows, []string{"Napomena", n})
	}
	if !rep.Exact {
		rows = append(rows, []string{"Napomena", "vremena paketa su procenjena iz intervala (agent ne šalje međurezultate)"})
	}
	if !rep.LostSeq && st.Lost > 0 {
		rows = append(rows, []string{"Napomena", "starija verzija agenta ne javlja koji su paketi izgubljeni; izgubljeni su navedeni na kraju"})
	}
	rows = append(rows, []string{}, []string{"Redni broj", "Vreme", "Odgovor (ms)", "Status"})
	for _, p := range rep.Probes {
		row := []string{strconv.Itoa(p.Seq), p.At.Format("15:04:05.000"), "", "OK"}
		if p.Lost {
			row[3] = "izgubljen"
		} else {
			row[2] = decimal(p.RTT, 3)
		}
		rows = append(rows, row)
	}
	cw.WriteAll(rows)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+reportFilename(rep, "csv")+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

// handleReportJSON vraća izveštaj za prikaz u tabeli Rezultati (svim
// korisnicima, bez obzira ko je pokrenuo merenje).
func (s *Server) handleReportJSON(w http.ResponseWriter, r *http.Request) {
	rep, err := s.loadReport(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	outs := slices.Clone(rep.Outages)
	more := 0
	if len(outs) > 100 { // najduži prekidi; svi su u CSV-u
		slices.SortStableFunc(outs, func(a, b outage) int { return b.Count - a.Count })
		more = len(outs) - 100
		outs = outs[:100]
		slices.SortFunc(outs, func(a, b outage) int { return a.FromSeq - b.FromSeq })
	}
	st := rep.Stats
	writeJSON(w, http.StatusOK, map[string]any{
		"id": rep.Res.ID, "kind": rep.Res.Kind, "phase": "done", "title": rep.Title,
		"host": rep.Host, "target": rep.Target, "method": strings.ToLower(rep.Method), "user": rep.Res.User,
		"from_console": rep.Source == "console", "started": rep.Start, "finished": rep.End,
		"interval_ms": rep.Interval.Milliseconds(), "count": rep.Data.Sent, "stopped": rep.Data.Stopped,
		"continuous": rep.Continuous, "stop_note": rep.stopNote(),
		"exact": rep.Exact, "lost_seq": rep.LostSeq, "stats": &st,
		"probes": downsample(rep.Probes, liveMaxProbes), "outages": outs, "outages_more": more,
		"request": json.RawMessage(rep.Res.Params), "agent_id": rep.Res.AgentID,
	})
}

// pingVerdict je ista ocena kao u web interfejsu.
func pingVerdict(st latStats) (string, string) {
	switch {
	case st.Received == 0:
		return "Nema odgovora", "bad"
	case st.AvgMS < 20 && st.JitterMS < 5 && st.Lost == 0:
		return "Odlično", "ok"
	case st.AvgMS < 50 && st.JitterMS < 15 && st.LossPct < 1:
		return "Dobro", "ok"
	case st.AvgMS < 150 && st.JitterMS < 30 && st.LossPct < 3:
		return "Prihvatljivo", "warn"
	}
	return "Loše", "bad"
}

var reportTmpl = template.Must(template.New("r").Funcs(template.FuncMap{
	"dec":  func(v float64) string { return decimal(v, 2) },
	"time": func(t time.Time) string { return t.Format("15:04:05") },
	"dt":   func(t time.Time) string { return t.Format("02.01.2006. 15:04:05") },
	"dur":  fmtDur,
}).Parse(`<!doctype html>
<html lang="sr"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ping {{.R.Title}}</title>
<style>
:root { --fg:#1c2330; --muted:#6b7380; --border:#dfe3e8; --dl:#2563eb; --ok:#15803d; --warn:#b45309; --bad:#b91c1c; }
body { font: 14px/1.5 system-ui, sans-serif; color: var(--fg); background: #fff; max-width: 900px; margin: 24px auto; padding: 0 16px; }
h1 { font-size: 20px; margin: 0 0 4px; } h2 { font-size: 15px; margin: 22px 0 8px; }
.muted { color: var(--muted); }
table { border-collapse: collapse; width: 100%; }
td, th { text-align: left; padding: 5px 8px; border-bottom: 1px solid var(--border); font-variant-numeric: tabular-nums; }
th { color: var(--muted); font-weight: 600; font-size: 12px; }
.grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 8px; }
.tile { border: 1px solid var(--border); border-radius: 6px; padding: 6px 10px; }
.tile .k { font-size: 11px; color: var(--muted); } .tile .v { font-size: 17px; font-weight: 600; }
.verdict { font-size: 16px; font-weight: 700; } .ok { color: var(--ok); } .warn { color: var(--warn); } .bad { color: var(--bad); }
svg text { fill: var(--muted); font-size: 10px; }
.note { border-left: 3px solid var(--warn); padding: 4px 10px; margin: 8px 0; }
@media print { body { margin: 0; } .noprint { display: none; } }
</style></head><body>
<h1>Izveštaj pinga: {{.R.Title}}</h1>
<div class="muted">{{.R.Method}} · {{dt .R.Start}} – {{dt .R.End}} ({{dur .Dur}}) · interval {{.IntervalS}} s · pokrenuo {{.R.Res.User}}</div>
<p class="verdict {{.VerdictCls}}">Ocena: {{.Verdict}}</p>
{{with .R.StopNote}}<p class="note">{{.}}</p>{{end}}
{{if not .R.Exact}}<p class="note">Vremena paketa su procenjena iz intervala (agent ne šalje međurezultate).</p>{{end}}
<div class="grid">
<div class="tile"><div class="k">poslato / primljeno</div><div class="v">{{.S.Sent}} / {{.S.Received}}</div></div>
<div class="tile"><div class="k">gubitak</div><div class="v {{if .S.Lost}}bad{{end}}">{{dec .S.LossPct}}% ({{.S.Lost}})</div></div>
<div class="tile"><div class="k">prosek</div><div class="v">{{dec .S.AvgMS}} ms</div></div>
<div class="tile"><div class="k">jitter</div><div class="v">{{dec .S.JitterMS}} ms</div></div>
<div class="tile"><div class="k">min</div><div class="v">{{dec .S.MinMS}} ms</div></div>
<div class="tile"><div class="k">max</div><div class="v">{{dec .S.MaxMS}} ms</div></div>
<div class="tile"><div class="k">95% ispod</div><div class="v">{{dec .S.P95}} ms</div></div>
<div class="tile"><div class="k">prekida</div><div class="v {{if .S.Outages}}bad{{end}}">{{.S.Outages}}</div></div>
</div>
<h2>Vreme odgovora</h2>
<svg viewBox="0 0 860 220" width="100%" role="img" aria-label="Grafikon vremena odgovora">
{{range .Grid}}<line x1="50" x2="850" y1="{{.Y}}" y2="{{.Y}}" stroke="#dfe3e8"/><text x="44" y="{{.TY}}" text-anchor="end">{{.Label}}</text>{{end}}
<text x="4" y="10">ms</text>
<polyline fill="none" stroke="#2563eb" stroke-width="1.5" points="{{.Line}}"/>
{{range .LostX}}<line x1="{{.}}" x2="{{.}}" y1="10" y2="200" stroke="#b91c1c" stroke-opacity=".55"/>{{end}}
<text x="50" y="216">{{time .R.Start}}</text><text x="850" y="216" text-anchor="end">{{time .R.End}}</text>
</svg>
<div class="muted">Plava linija: vreme odgovora{{if .Grouped}} (prosek grupe od {{.Group}} paketa){{end}}. Crvene linije: izgubljeni paketi.</div>
<h2>Prekidi (uzastopni izgubljeni paketi)</h2>
{{if .R.Outages}}<table><tr><th>Od</th><th>Do</th><th>Paketa</th><th>Trajanje (približno)</th></tr>
{{range .OutRows}}<tr><td>{{time .From}}</td><td>{{time .To}}</td><td>{{.Count}}</td><td>{{.Dur}}</td></tr>{{end}}
</table>{{if .OutMore}}<p class="muted">… i još {{.OutMore}} kraćih prekida (svi su u CSV izveštaju).</p>{{end}}
{{else if .R.LostSeq}}<p>Nije bilo prekida.</p>{{else}}<p class="muted">Starija verzija agenta ne javlja koji su paketi izgubljeni.</p>{{end}}
<p class="muted noprint">Za PDF: Ctrl+P → „Sačuvaj kao PDF”. Svi pojedinačni pingovi su u CSV izveštaju.</p>
<p class="muted">Network Monitor · izveštaj napravljen {{dt .Now}}</p>
</body></html>`))

func (s *Server) handleReportHTML(w http.ResponseWriter, r *http.Request) {
	rep, err := s.loadReport(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	st := rep.Stats
	verdict, cls := pingVerdict(st)
	// Grafikon: najviše 800 tačaka (prosek grupe).
	pts := downsample(rep.Probes, 800)
	yMax := niceCeil(math.Max(st.MaxMS*1.1, 1))
	n := max(len(pts)-1, 1)
	x := func(i int) float64 { return 50 + 800*float64(i)/float64(n) }
	y := func(v float64) float64 { return 200 - 190*math.Min(v, yMax)/yMax }
	var line strings.Builder
	var lostX []string
	for i, p := range pts {
		if p.Lost || p.LostN > 0 {
			lostX = append(lostX, strconv.FormatFloat(x(i), 'f', 1, 64))
		}
		if !p.Lost {
			fmt.Fprintf(&line, "%.1f,%.1f ", x(i), y(p.RTT))
		}
	}
	type gridLine struct {
		Y, TY float64
		Label string
	}
	var grid []gridLine
	for _, f := range []float64{0, 0.5, 1} {
		v := yMax * f
		grid = append(grid, gridLine{Y: y(v), TY: y(v) + 3, Label: strconv.FormatFloat(v, 'f', -1, 64)})
	}
	type outRow struct {
		From, To time.Time
		Count    int
		Dur      string
	}
	outs := slices.Clone(rep.Outages)
	slices.SortStableFunc(outs, func(a, b outage) int { return b.Count - a.Count })
	more := 0
	if len(outs) > 50 {
		more = len(outs) - 50
		outs = outs[:50]
	}
	slices.SortFunc(outs, func(a, b outage) int { return a.FromSeq - b.FromSeq })
	var rows []outRow
	for _, o := range outs {
		rows = append(rows, outRow{From: o.From, To: o.To, Count: o.Count, Dur: fmtDur(time.Duration(o.Count) * rep.Interval)})
	}
	group := 1
	if len(rep.Probes) > 800 {
		group = (len(rep.Probes) + 799) / 800
	}
	var b bytes.Buffer
	err = reportTmpl.Execute(&b, map[string]any{
		"R": rep, "S": st, "Dur": rep.End.Sub(rep.Start), "IntervalS": decimal(rep.Interval.Seconds(), 1),
		"Verdict": verdict, "VerdictCls": cls, "Line": line.String(), "LostX": lostX, "Grid": grid,
		"OutRows": rows, "OutMore": more, "Grouped": group > 1, "Group": group, "Now": time.Now(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+reportFilename(rep, "html")+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

// niceCeil zaokružuje naviše na 1, 2 ili 5 × 10^n.
func niceCeil(v float64) float64 {
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

// slimLatencyData uklanja pojedinačne vrednosti iz dugih pingova u spisku
// rezultata (neprekidni ping ima do 172.800 vrednosti); sve su u izveštaju
// (/api/results/{id}/report.csv|json).
func slimLatencyData(r Result) json.RawMessage {
	if (r.Kind != "ping" && r.Kind != "latency") || len(r.Data) < 8<<10 {
		return r.Data
	}
	var d protocol.LatencyResult
	if json.Unmarshal(r.Data, &d) != nil || len(d.Samples) <= 200 {
		return r.Data
	}
	d.Samples, d.TimesMS, d.Lost = nil, nil, nil
	b, err := json.Marshal(d)
	if err != nil {
		return r.Data
	}
	return b
}

// Close uredno zaustavlja pingove u toku (npr. pri gašenju servisa) i čeka
// najviše timeout da se njihovi dotadašnji rezultati upišu u istoriju.
func (s *Server) Close(timeout time.Duration) {
	s.live.mu.Lock()
	var stops []func()
	for _, t := range s.live.tests {
		if t.cancel != nil && t.v.Finished.IsZero() {
			t.v.Stopping = true
			stops = append(stops, t.cancel)
		}
	}
	s.live.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
	done := make(chan struct{})
	go func() { s.pings.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	s.net.close()
}
