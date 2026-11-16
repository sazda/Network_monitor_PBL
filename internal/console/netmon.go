package console

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sazda/network-monitor/internal/nettest"
	"github.com/sazda/network-monitor/internal/protocol"
)

// Provera mrežne opreme: konzola na svakih pingEvery pinguje sve aktivne
// uređaje. Uređaj je „nije na vezi” tek posle downAfter uzastopnih provera bez
// odgovora (jedan izgubljen paket ne pravi lažnu uzbunu).
const (
	pingEvery   = 30 * time.Second
	downAfter   = 3
	lossWindow  = 20 // provera za računanje gubitka (×2 paketa)
	maxEvents   = 1000
	eventsLimit = 5 << 20 // veličina fajla događaja posle koje se skraćuje
)

// Stanja uređaja i lokacija.
const (
	stUnknown  = "unknown"  // još nije provereno
	stUp       = "up"       // na vezi
	stDown     = "down"     // nije na vezi
	stInactive = "inactive" // ne proverava se
	// lokacije
	stOK      = "ok"      // svi uređaji na vezi
	stPartial = "partial" // neki uređaji nisu na vezi
	stBackup  = "backup"  // glavna (MPLS) veza ne radi, lokacija radi preko rezervne
	stLocDown = "down"    // lokacija nedostupna
)

// NetEvent je zapis u istoriji događaja mrežne opreme.
type NetEvent struct {
	Time        time.Time `json:"time"`
	Kind        string    `json:"kind"` // down, up, loc_down, loc_backup, loc_ok, reboot, snmp_error, snmp_ok
	DeviceID    string    `json:"device_id,omitempty"`
	Device      string    `json:"device,omitempty"`
	Location    string    `json:"location,omitempty"`
	Text        string    `json:"text"`
	DurationSec int64     `json:"duration_sec,omitempty"`
	// Procs: procesi koji su najviše opterećivali server u trenutku događaja
	// (visok CPU/RAM na serveru).
	Procs []protocol.Proc `json:"procs,omitempty"`
}

type devRuntime struct {
	temp      tempAlertState // temperatura iznad praga (temp.go)
	reloadReq *reloadRequest // poslednji zahtev za restart iz syslog-a (reload.go)
	status    string
	since     time.Time
	misses    int
	firstMiss time.Time
	rtt       float64 // poslednji odziv (ms), NaN ako nije odgovorio
	lastCheck time.Time
	recent    []int // primljeno paketa po proveri (poslednjih lossWindow)
	alerted   bool  // poslato obaveštenje o padu
	snmp      *snmpRuntime
	mpls      *mplsRuntime // MPLS link (samo CE ruteri)
}

type snmpRuntime struct {
	ok      bool
	err     string
	checked time.Time
	lastOK  time.Time
	sys     snmpSystem
	fullAt  time.Time
	ports   []portView
	prev    map[int]portSample
	prevAt  time.Time
	// MAC tabela (ifIndex → MAC adrese), čita se na fdbEvery.
	fdb       map[int][]string
	fdbAt     time.Time
	fdbMethod string
	fdbErr    string
	nb        map[int][]neighbor // CDP/LLDP susedi po ifIndex-u
	trunks    map[int]bool       // portovi koji rade kao trunk (nil = uređaj ne javlja)
	wifi      *wifiTable         // Wi-Fi klijenti (samo WLC)
	temps     *tempInfo          // temperatura (samo switch-evi)
}

const fdbEvery = 5 * time.Minute

// attachedDev je uređaj na portu (iz MAC tabele switch-a i podataka agenata).
type attachedDev struct {
	MAC     string `json:"mac"`
	Name    string `json:"name,omitempty"` // naziv računara (agent); prazno = nepoznat uređaj
	AgentID string `json:"agent_id,omitempty"`
}

// maxAttached: port sa više MAC adresa je uplink/trunk ili ima switch iza
// sebe; tada se prikazuje samo broj.
const maxAttached = 4

// portView je port za prikaz.
type portView struct {
	Index    int      `json:"index"`
	Name     string   `json:"name"`
	Alias    string   `json:"alias,omitempty"`
	Admin    bool     `json:"admin_up"`
	Oper     string   `json:"oper"` // up, down, ...
	Speed    int64    `json:"speed_mbps"`
	InBps    *float64 `json:"in_bps,omitempty"`
	OutBps   *float64 `json:"out_bps,omitempty"`
	InErr    uint64   `json:"in_err"` // u poslednjem intervalu čitanja
	OutErr   uint64   `json:"out_err"`
	InDisc   uint64   `json:"in_disc"`
	OutDisc  uint64   `json:"out_disc"`
	ErrTotal uint64   `json:"err_total"` // ukupno od pokretanja uređaja (greške)
	Changed  int64    `json:"changed_sec,omitempty"`
	// Uređaji na portu (samo u detaljima, kada je MAC tabela pročitana).
	Attached  []attachedDev `json:"attached,omitempty"`
	MACCount  int           `json:"mac_count,omitempty"`
	Neighbors []neighbor    `json:"neighbors,omitempty"` // CDP/LLDP
	// Trunk: veza ka drugom switch-u (računari se ne navode). Shared:
	// pristupni port sa više od maxAttached uređaja (npr. mali switch ispod
	// stola) – navode se poznati računari, a ostali samo brojem (Unknown).
	Trunk   bool `json:"trunk,omitempty"`
	Shared  bool `json:"shared,omitempty"`
	Unknown int  `json:"unknown,omitempty"`
	// Behind: računari (agenti) iza manjeg switch-a koji nije u konzoli,
	// priključenog na ovaj (ne-uplink) trunk port; Via je naziv tog switch-a.
	Behind []attachedDev `json:"behind,omitempty"`
	Via    string        `json:"via,omitempty"`
	// WiFi: računari (agenti) na Wi-Fi-ju preko AP-a koji je na ovom portu,
	// po podacima sa WLC-a (vidi wifi.go).
	WiFi []wifiDev `json:"wifi,omitempty"`
}

var operNames = map[int]string{1: "up", 2: "down", 3: "testing", 4: "unknown", 5: "dormant", 6: "notPresent", 7: "lowerLayerDown"}

type locRuntime struct {
	status string
	since  time.Time
}

// netMonitor prati mrežnu opremu.
type netMonitor struct {
	log    *log.Logger
	file   deviceFile
	evPath string
	// statePath: padovi i lokacije koje ne rade (vidi netstate.go).
	statePath  string
	stateSaved []byte
	hist       *tsdb // može biti nil (istorija nedostupna)
	notify     *notifier
	locate     func(ip string) string
	macHosts   func() map[string]attachedDev // MAC → računar (agent)
	pingFn     func(ctx context.Context, ip string) (rtt float64, sent, recv int)
	dialSNMP   func(ctx context.Context, cfg SNMPConfig, ip string) (snmpClient, error)
	now        func() time.Time

	mu      sync.Mutex
	devices []NetDevice
	state   map[string]*devRuntime
	locs    map[string]*locRuntime
	events  []NetEvent
	snmpCfg *SNMPConfig
	// syslogAddr: adresa za prijem syslog-a („-” = isključeno).
	syslogAddr string
	// Tabela MPLS uživo (mpls.go).
	mplsLiveMu   sync.Mutex
	mplsLiveData map[string]*mplsLiveState

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	liveData liveReadings // očitavanja uživo (detalji uređaja otvoreni)
	hostData hostReadings // brojači svih portova za tabelu računara

	// Obaveštenja se šalju jedno po jedno, redom kojim su nastala.
	alertQ    chan alertJob
	alertOnce sync.Once
}

type alertJob struct {
	cfg NotifyConfig
	msg notifyMessage
}

func newNetMonitor(dataDir string, snmp *SNMPConfig, logger *log.Logger, n *notifier, locate func(string) string) *netMonitor {
	m := &netMonitor{
		log:    logger,
		file:   deviceFile{filepath.Join(dataDir, "netdevices.json")},
		evPath: filepath.Join(dataDir, "netdev-events.jsonl"),

		statePath: filepath.Join(dataDir, "netdev-state.json"),
		notify:    n,
		locate:    locate,
		pingFn:    icmpCheck,
		dialSNMP:  dialSNMP,
		now:       time.Now,
		state:     map[string]*devRuntime{},
		locs:      map[string]*locRuntime{},
		snmpCfg:   snmp,
		stop:      make(chan struct{}),
	}
	if list, err := m.file.load(); err != nil {
		logger.Printf("mrežna oprema: %v", err)
	} else {
		m.devices = list
	}
	m.events = loadEvents(m.evPath)
	m.loadStateLocked()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		logger.Printf("istorija mrežne opreme nije dostupna: %v", err)
	} else if h, err := openTSDB(dataDir, histRawDays, histAggDays); err != nil {
		logger.Printf("istorija mrežne opreme nije dostupna: %v", err)
	} else {
		m.hist = h
	}
	return m
}

// icmpCheck šalje dva pinga i vraća prosečan odziv (NaN ako nije bilo odgovora).
func icmpCheck(ctx context.Context, ip string) (float64, int, int) {
	res, err := nettest.MeasureICMP(ctx, ip, 2, 300*time.Millisecond, 1500*time.Millisecond)
	if err != nil || res.Received == 0 {
		return math.NaN(), max(res.Sent, 2), 0
	}
	return res.AvgMS, res.Sent, res.Received
}

func (m *netMonitor) start() {
	m.wg.Add(3)
	go m.pingLoop()
	go m.snmpLoop()
	go m.mplsLoop()
	if m.syslogAddr != "" && m.syslogAddr != "-" {
		m.wg.Add(1)
		go m.syslogLoop(m.syslogAddr)
	}
}

func (m *netMonitor) close() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	if m.hist != nil {
		m.hist.Close()
	}
}

func (m *netMonitor) pingLoop() {
	defer m.wg.Done()
	m.pingRound()
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.pingRound()
		}
	}
}

func (m *netMonitor) snmpLoop() {
	defer m.wg.Done()
	// Prvo čitanje posle prve provere pingom (da se ne čitaju uređaji koji ne rade).
	select {
	case <-m.stop:
		return
	case <-time.After(5 * time.Second):
	}
	for {
		m.snmpRound()
		m.mu.Lock()
		every := m.snmpCfg.interval()
		m.mu.Unlock()
		select {
		case <-m.stop:
			return
		case <-time.After(every):
		}
	}
}

// ---------- provera pingom ----------

func (m *netMonitor) rt(id string) *devRuntime {
	st := m.state[id]
	if st == nil {
		st = &devRuntime{status: stUnknown, rtt: math.NaN()}
		m.state[id] = st
	}
	return st
}

func (m *netMonitor) locationOf(d NetDevice) string {
	if d.Location != "" {
		return d.Location
	}
	if name := m.locate(d.IP); name != "" {
		return name
	}
	return "Bez lokacije"
}

type pingOutcome struct {
	rtt        float64
	sent, recv int
}

func (m *netMonitor) pingRound() {
	m.mu.Lock()
	var list []NetDevice
	for _, d := range m.devices {
		if d.Active {
			list = append(list, d)
		}
	}
	ping := m.pingFn
	m.mu.Unlock()

	results := make([]pingOutcome, len(list))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i, d := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rtt, sent, recv := ping(ctx, d.IP)
			results[i] = pingOutcome{rtt, sent, recv}
		}()
	}
	wg.Wait()
	now := m.now()
	if m.hist != nil {
		for i, d := range list {
			m.hist.Add("rtt/"+d.ID, now, []float64{results[i].rtt})
		}
	}
	m.apply(list, results, now)
}

// apply upisuje rezultate provere, beleži promene i šalje obaveštenja.
func (m *netMonitor) apply(list []NetDevice, results []pingOutcome, now time.Time) {
	var evs, downs, ups []NetEvent
	m.mu.Lock()
	for i, d := range list {
		if !slices.ContainsFunc(m.devices, func(x NetDevice) bool { return x.ID == d.ID && x.Active }) {
			continue // uklonjen ili isključen u međuvremenu
		}
		st := m.rt(d.ID)
		r := results[i]
		st.lastCheck = now
		st.recent = append(st.recent, r.recv)
		if len(st.recent) > lossWindow {
			st.recent = st.recent[len(st.recent)-lossWindow:]
		}
		loc := m.locationOf(d)
		if r.recv > 0 {
			st.rtt = r.rtt
			st.misses = 0
			if st.status != stUp {
				prev, since := st.status, st.since
				st.status, st.since = stUp, now
				if prev == stDown {
					down := now.Sub(since)
					ev := NetEvent{Time: now, Kind: "up", DeviceID: d.ID, Device: d.Name, Location: loc,
						Text: fmt.Sprintf("%s je ponovo na vezi (nije bio %s)", d.Name, fmtDuration(down)), DurationSec: int64(down.Seconds())}
					evs = append(evs, ev)
					if st.alerted {
						ups = append(ups, ev)
					}
				}
				st.alerted = false
			}
			continue
		}
		st.rtt = math.NaN()
		if st.misses == 0 {
			st.firstMiss = now
		}
		st.misses++
		if st.misses >= downAfter && st.status != stDown {
			st.status, st.since = stDown, st.firstMiss
			ev := NetEvent{Time: now, Kind: "down", DeviceID: d.ID, Device: d.Name, Location: loc,
				Text: fmt.Sprintf("%s nije na vezi (ne odgovara na ping od %s)", d.Name, st.firstMiss.Local().Format("15:04"))}
			evs = append(evs, ev)
			if d.Notify {
				downs = append(downs, ev)
			}
		}
	}
	locEvs := m.updateLocationsLocked(now)
	evs = append(evs, locEvs...)
	// Uređaji na nedostupnoj lokaciji se ne javljaju pojedinačno: stiže
	// jedno obaveštenje za celu lokaciju.
	downs = slices.DeleteFunc(downs, func(ev NetEvent) bool {
		if l := m.locs[ev.Location]; l != nil && l.status == stLocDown {
			return true
		}
		m.rt(ev.DeviceID).alerted = true
		return false
	})
	m.recordLocked(evs)
	m.saveStateLocked()
	m.mu.Unlock()
	m.sendAlerts(downs, ups, locEvs, now)
}

// locSummary je stanje lokacije za prikaz.
type locSummary struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Since  time.Time `json:"since,omitzero"`
	Total  int       `json:"total"`
	Up     int       `json:"up"`
	Down   int       `json:"down"`
	RTT    *float64  `json:"rtt_ms,omitempty"`
	Loss   *float64  `json:"loss_pct,omitempty"`
	WAN    string    `json:"wan,omitempty"`    // naziv uređaja glavne veze
	WANUp  *bool     `json:"wan_up,omitempty"` // da li glavna veza radi
	Backup string    `json:"backup,omitempty"`
}

func (st *devRuntime) lossPct() *float64 {
	if len(st.recent) == 0 {
		return nil
	}
	recv := 0
	for _, r := range st.recent {
		recv += r
	}
	v := 100 * (1 - float64(recv)/float64(2*len(st.recent)))
	v = math.Max(0, math.Round(v*10)/10)
	return &v
}

func ptr[T any](v T) *T { return &v }

// summarizeLocationsLocked računa stanje svih lokacija.
func (m *netMonitor) summarizeLocationsLocked() map[string]*locSummary {
	out := map[string]*locSummary{}
	type agg struct {
		wanKnown, wanUp, otherUp bool
		bestRTT, wanRTT          float64
		wanLoss                  *float64
	}
	tmp := map[string]*agg{}
	// Lokacije sa MPLS vezom u kartici MPLS: glavna/rezervna veza su CE
	// ruteri (stanje linka iz SNMP-a), a uloge ostalih uređaja se ne koriste.
	mplsLoc := map[string]bool{}
	for _, d := range m.devices {
		if d.Active && d.MPLSLink != "" {
			mplsLoc[m.locationOf(d)] = true
		}
	}
	for _, d := range m.devices {
		if !d.Active {
			continue
		}
		name := m.locationOf(d)
		s := out[name]
		if s == nil {
			s = &locSummary{Name: name}
			out[name] = s
			tmp[name] = &agg{bestRTT: math.NaN(), wanRTT: math.NaN()}
		}
		a := tmp[name]
		st := m.rt(d.ID)
		s.Total++
		switch st.status {
		case stUp:
			s.Up++
		case stDown:
			s.Down++
		}
		role, up := d.Role, st.status == stUp
		if mplsLoc[name] {
			role = map[string]string{mplsPrimary: "wan", mplsSecondary: "backup"}[d.MPLSLink]
			if d.MPLSLink != "" {
				up = mplsLinkOK(st)
			}
		}
		switch role {
		case "wan":
			s.WAN = d.Name
			if st.status != stUnknown {
				a.wanKnown = true
				a.wanUp = a.wanUp || up
				s.WANUp = ptr(a.wanUp)
			}
			a.wanRTT, a.wanLoss = st.rtt, st.lossPct()
		case "backup":
			s.Backup = d.Name
			if up {
				a.otherUp = true
			}
		default:
			if st.status == stUp {
				a.otherUp = true
				if math.IsNaN(a.bestRTT) || st.rtt < a.bestRTT {
					a.bestRTT = st.rtt
				}
			}
		}
	}
	for name, s := range out {
		a := tmp[name]
		switch {
		case s.Up+s.Down == 0:
			s.Status = stUnknown
		case a.wanKnown && !a.wanUp && a.otherUp:
			s.Status = stBackup
		case a.wanKnown && !a.wanUp:
			s.Status = stLocDown
		case s.Up == 0 && (s.Down >= 2 || a.wanKnown):
			s.Status = stLocDown
		case s.Down > 0:
			s.Status = stPartial
		default:
			s.Status = stOK
		}
		if !math.IsNaN(a.wanRTT) {
			s.RTT = ptr(math.Round(a.wanRTT*100) / 100)
		} else if !math.IsNaN(a.bestRTT) {
			s.RTT = ptr(math.Round(a.bestRTT*100) / 100)
		}
		s.Loss = a.wanLoss
		if l := m.locs[name]; l != nil {
			s.Since = l.since
		}
	}
	return out
}

// updateLocationsLocked beleži promene stanja lokacija (MPLS pao, rezervna
// veza, lokacija nedostupna) i vraća događaje.
func (m *netMonitor) updateLocationsLocked(now time.Time) []NetEvent {
	var evs []NetEvent
	sum := m.summarizeLocationsLocked()
	for name, s := range sum {
		l := m.locs[name]
		if l == nil {
			l = &locRuntime{status: s.Status, since: now}
			m.locs[name] = l
			// Prvo stanje posle pokretanja: javlja se samo ako lokacija ne radi.
			if s.Status != stLocDown && s.Status != stBackup {
				continue
			}
			l.status = stUnknown
		}
		if l.status == s.Status || s.Status == stUnknown {
			continue
		}
		prev, prevSince := l.status, l.since
		l.status, l.since = s.Status, now
		var ev *NetEvent
		switch {
		case s.Status == stLocDown:
			ev = &NetEvent{Kind: "loc_down", Text: fmt.Sprintf("Lokacija %s je nedostupna (nijedan uređaj ne odgovara)", name)}
		case s.Status == stBackup:
			ev = &NetEvent{Kind: "loc_backup", Text: fmt.Sprintf("Lokacija %s: glavna (MPLS) veza ne radi, lokacija radi preko rezervne veze", name)}
		case prev == stLocDown || prev == stBackup:
			d := now.Sub(prevSince)
			what := "ponovo dostupna"
			if prev == stBackup {
				what = "glavna (MPLS) veza ponovo radi"
			}
			ev = &NetEvent{Kind: "loc_ok", Text: fmt.Sprintf("Lokacija %s: %s (trajalo %s)", name, what, fmtDuration(d)), DurationSec: int64(d.Seconds())}
		}
		if ev != nil {
			ev.Time, ev.Location = now, name
			evs = append(evs, *ev)
		}
	}
	for name := range m.locs {
		if _, ok := sum[name]; !ok {
			delete(m.locs, name)
		}
	}
	return evs
}

// ---------- događaji ----------

func loadEvents(path string) []NetEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []NetEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev NetEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	if len(out) > maxEvents {
		out = out[len(out)-maxEvents:]
	}
	return out
}

func (m *netMonitor) recordLocked(evs []NetEvent) {
	if len(evs) == 0 {
		return
	}
	m.events = append(m.events, evs...)
	if len(m.events) > maxEvents {
		m.events = slices.Clone(m.events[len(m.events)-maxEvents:])
	}
	for _, ev := range evs {
		m.log.Printf("mrežna oprema: %s", ev.Text)
	}
	f, err := os.OpenFile(m.evPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		m.log.Printf("upis događaja: %v", err)
		return
	}
	w := bufio.NewWriter(f)
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		w.Write(append(b, '\n'))
	}
	w.Flush()
	info, _ := f.Stat()
	f.Close()
	if info != nil && info.Size() > eventsLimit {
		// Skraćivanje: ostaju samo događaji iz memorije.
		var b strings.Builder
		for _, ev := range m.events {
			j, _ := json.Marshal(ev)
			b.Write(j)
			b.WriteByte('\n')
		}
		tmp := m.evPath + ".tmp"
		if os.WriteFile(tmp, []byte(b.String()), 0o600) == nil {
			os.Rename(tmp, m.evPath)
		}
	}
}

// ---------- obaveštenja ----------

func (m *netMonitor) sendAlerts(downs, ups, locs []NetEvent, now time.Time) {
	m.sendAlertsWith(downs, ups, locs, now, nil)
}

// sendAlertsWith: kao sendAlerts; extra dopunjuje poruku (npr. HTML i slika
// za e-mail).
func (m *netMonitor) sendAlertsWith(downs, ups, locs []NetEvent, now time.Time, extra func(*notifyMessage)) {
	if m.notify == nil || len(downs)+len(ups)+len(locs) == 0 {
		return
	}
	cfg := m.notify.config()
	if !cfg.anyFor(scopeNet) {
		return
	}
	if cfg.SkipRecovery {
		ups = nil
		locs = slices.DeleteFunc(slices.Clone(locs), func(ev NetEvent) bool { return ev.Kind == "loc_ok" || ev.Kind == "temp_ok" || ev.Kind == "mpls_up" })
	}
	msg, ok := composeNetAlert(downs, ups, locs, now)
	if !ok {
		return
	}
	msg.Scope = scopeNet
	msg.SMS = composeNetSMS(downs, ups, locs, now)
	if extra != nil {
		extra(&msg)
	}
	m.alertOnce.Do(func() {
		m.alertQ = make(chan alertJob, 64)
		go func() {
			for j := range m.alertQ {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				m.notify.deliver(ctx, j.cfg, j.msg)
				cancel()
			}
		}()
	})
	select {
	case m.alertQ <- alertJob{cfg, msg}:
	default:
		m.log.Printf("obaveštenje %q nije poslato (previše obaveštenja u redu)", msg.Title)
	}
}

func composeNetAlert(downs, ups, locs []NetEvent, now time.Time) (notifyMessage, bool) {
	n := len(downs) + len(ups) + len(locs)
	if n == 0 {
		return notifyMessage{}, false
	}
	bad := len(downs) > 0
	for _, l := range locs {
		if l.Kind != "loc_ok" && l.Kind != "temp_ok" && l.Kind != "mpls_up" {
			bad = true
		}
	}
	var title string
	switch {
	case n == 1 && len(downs) == 1:
		title = downs[0].Device + " nije na vezi"
	case n == 1 && len(ups) == 1:
		title = ups[0].Device + " je ponovo na vezi"
	case n == 1 && locs[0].Kind == "loc_down":
		title = "Lokacija " + locs[0].Location + " nedostupna"
	case n == 1 && locs[0].Kind == "loc_backup":
		title = locs[0].Location + ": radi preko rezervne veze"
	case n == 1 && locs[0].Kind == "reboot":
		title = locs[0].Device + " je ponovo pokrenut"
	case n == 1 && (locs[0].Kind == "mpls_down" || locs[0].Kind == "mpls_up"):
		title = "MPLS " + locs[0].Text
	case n == 1 && locs[0].Kind == "temp_high":
		title = locs[0].Device + ": visoka temperatura"
	case n == 1 && locs[0].Kind == "temp_ok":
		title = locs[0].Device + ": temperatura je ponovo u granicama"
	case n == 1:
		title = locs[0].Location + ": veza ponovo radi"
	case bad:
		title = fmt.Sprintf("Mrežna oprema: %d promena (ima problema)", n)
	default:
		title = fmt.Sprintf("Mrežna oprema: %d uređaja ponovo radi", n)
	}
	var b strings.Builder
	line := func(ev NetEvent) {
		b.WriteString("• " + cleanText(ev.Text))
		if ev.Location != "" && ev.Device != "" {
			b.WriteString(" – " + cleanText(ev.Location))
		}
		b.WriteString("\n")
	}
	for _, ev := range locs {
		line(ev)
	}
	for _, ev := range downs {
		line(ev)
	}
	for _, ev := range ups {
		line(ev)
	}
	fmt.Fprintf(&b, "\nNetwork Monitor, %s", now.Local().Format("02.01.2006. 15:04"))
	return notifyMessage{Title: "Network Monitor: " + cleanText(title), Body: b.String(), Down: bad, Recovery: !bad}, true
}

// ---------- SNMP ----------

func (m *netMonitor) snmpRound() {
	m.mu.Lock()
	cfg := m.snmpCfg
	var list []NetDevice
	if cfg.configured() {
		for _, d := range m.devices {
			// Samo uređaji koji odgovaraju na ping (inače bi svaki pad
			// doneo i suvišnu SNMP grešku).
			if d.Active && d.SNMP && m.rt(d.ID).status == stUp {
				list = append(list, d)
			}
		}
	}
	m.mu.Unlock()
	if len(list) == 0 {
		return
	}
	c := *cfg
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, d := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			m.pollSNMP(c.forDevice(d.SNMPProfile), d)
		}()
	}
	wg.Wait()
}

// pollSNMP čita jedan uređaj i upisuje saobraćaj u istoriju.
func (m *netMonitor) pollSNMP(cfg SNMPConfig, d NetDevice) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	m.mu.Lock()
	st := m.rt(d.ID)
	if st.snmp == nil {
		st.snmp = &snmpRuntime{}
	}
	full := m.now().Sub(st.snmp.fullAt) > time.Hour
	fdbDue := m.now().Sub(st.snmp.fdbAt) > fdbEvery
	// Temperatura (samo switch-evi): radna kopija, upisuje se posle čitanja.
	var ti *tempInfo
	if d.Type == "switch" {
		ti = &tempInfo{}
		if old := st.snmp.temps; old != nil {
			*ti = *old
			ti.sensors = slices.Clone(old.sensors)
		}
	}
	m.mu.Unlock()

	sys, ports, err := m.readDevice(ctx, cfg, d.IP, full)
	// Razlog restarta (Cisco) se čita samo dok switch radi kratko.
	why := ""
	if err == nil && sys.Uptime < reloadReadUptime {
		if c, derr := m.dialFor(ctx, cfg, d.IP); derr == nil {
			why = readWhyReload(c)
			c.Close()
		}
	}
	var fdb fdbResult
	var fdbErr error
	var nb map[int][]neighbor
	var trunks map[int]bool
	var wifi map[string]wifiClient
	var aps int
	var wifiErr error
	if err == nil && fdbDue && d.Type == wlcType {
		// WLC nema MAC tabelu kao switch: čitaju se Wi-Fi klijenti.
		wifi, aps, wifiErr = m.readWLC(ctx, cfg, d.IP)
	} else if err == nil && fdbDue && !skipFDB(d) {
		fdb, fdbErr = m.readFDB(ctx, cfg, d.IP)
	}
	if err == nil && ti != nil {
		if c, derr := m.dialFor(ctx, cfg, d.IP); derr == nil {
			readTemps(c, ti, m.now())
			c.Close()
		} else {
			ti.err = snmpErrorText(derr)
		}
	}
	if err == nil && fdbDue && d.Type != wlcType {
		var raw map[int][]neighbor
		raw, trunks = m.readPortRoles(ctx, cfg, d.IP, ports)
		nb = filterNeighbors(raw, sys.SysName)
	}
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.ContainsFunc(m.devices, func(x NetDevice) bool { return x.ID == d.ID }) {
		return
	}
	sr := m.rt(d.ID).snmp
	if sr == nil { // podešavanja su promenjena u međuvremenu
		return
	}
	sr.checked = now
	loc := m.locationOf(d)
	if err != nil {
		if sr.ok || sr.lastOK.IsZero() && sr.err == "" {
			m.recordLocked([]NetEvent{{Time: now, Kind: "snmp_error", DeviceID: d.ID, Device: d.Name, Location: loc,
				Text: fmt.Sprintf("%s: SNMP čitanje nije uspelo – %s", d.Name, snmpErrorText(err))}})
		}
		sr.ok, sr.err = false, snmpErrorText(err)
		return
	}
	if !sr.ok && sr.err != "" {
		m.recordLocked([]NetEvent{{Time: now, Kind: "snmp_ok", DeviceID: d.ID, Device: d.Name, Location: loc,
			Text: fmt.Sprintf("%s: SNMP čitanje ponovo radi", d.Name)}})
	}
	if full {
		sr.fullAt = now
	} else {
		// Model, serijski broj i verzija iz prethodnog punog čitanja.
		sys.Model, sys.Serial = sr.sys.Model, sr.sys.Serial
		if sys.Version == "" {
			sys.Version = sr.sys.Version
		}
	}
	// Restart uređaja: uptime je manji nego u prethodnom čitanju (sysUpTime se
	// posle ~497 dana vraća na nulu, pa se taj slučaj ne računa).
	if !sr.lastOK.IsZero() && sys.Uptime < sr.sys.Uptime && sr.sys.Uptime < 496*86400 {
		rt := m.rt(d.ID)
		req := rt.reloadReq
		if req != nil && now.Sub(req.At) > reloadReqFor {
			req = nil
		}
		rt.reloadReq = nil
		ev := NetEvent{Time: now, Kind: "reboot", DeviceID: d.ID, Device: d.Name, Location: loc,
			Text: fmt.Sprintf("%s je ponovo pokrenut (radi %s)%s", d.Name, fmtDuration(time.Duration(sys.Uptime)*time.Second), reloadSuffix(why, sys.Model, req))}
		m.recordLocked([]NetEvent{ev})
		if d.Notify {
			m.sendAlerts(nil, nil, []NetEvent{ev}, now)
		}
		sr.prev = nil // brojači su resetovani
	}
	dt := now.Sub(sr.prevAt).Seconds()
	views := make([]portView, 0, len(ports))
	next := make(map[int]portSample, len(ports))
	for _, p := range ports {
		v := portView{Index: p.Index, Name: p.Name, Alias: p.Alias, Admin: p.Admin == 1, Oper: operNames[p.Oper],
			Speed: p.SpeedMbps, ErrTotal: p.InErr + p.OutErr}
		if v.Oper == "" {
			v.Oper = strconv.Itoa(p.Oper)
		}
		if p.LastChange > 0 && sys.Uptime > 0 {
			if ch := sys.Uptime - p.LastChange/100; ch >= 0 {
				v.Changed = ch
			}
		}
		next[p.Index] = p
		if prev, ok := sr.prev[p.Index]; ok && dt > 0 {
			in, ok1 := octetRate(prev.InOctets, p.InOctets, dt)
			out, ok2 := octetRate(prev.OutOctets, p.OutOctets, dt)
			ie, ok3 := counterDelta(prev.InErr, p.InErr)
			oe, ok4 := counterDelta(prev.OutErr, p.OutErr)
			id, ok5 := counterDelta(prev.InDisc, p.InDisc)
			od, ok6 := counterDelta(prev.OutDisc, p.OutDisc)
			if ok1 && ok2 && ok3 && ok4 && ok5 && ok6 {
				v.InBps, v.OutBps = ptr(math.Round(in)), ptr(math.Round(out))
				v.InErr, v.OutErr, v.InDisc, v.OutDisc = ie, oe, id, od
				if m.hist != nil {
					m.hist.Add(fmt.Sprintf("if/%s/%d", d.ID, p.Index), now,
						[]float64{in, out, float64(ie), float64(oe), float64(id), float64(od)})
				}
			}
		}
		views = append(views, v)
	}
	sr.ok, sr.err, sr.lastOK, sr.sys, sr.ports = true, "", now, sys, views
	sr.prev, sr.prevAt = next, now
	if ti != nil {
		sr.temps = ti
		if maxC, _, _ := tempSummary(ti.sensors); maxC != nil && ti.err == "" && m.hist != nil {
			if err := m.hist.Add("temp/"+d.ID, now, []float64{*maxC}); err != nil {
				m.log.Printf("istorija temperature %s: %v", d.Name, err)
			}
		}
		if ti.err == "" {
			if evs := m.checkTempLocked(d, m.rt(d.ID), ti, now); len(evs) > 0 {
				m.recordLocked(evs)
				if d.Notify {
					m.sendAlerts(nil, nil, evs, now)
				}
			}
		}
	}
	if fdbDue && d.Type == wlcType {
		if sr.wifi == nil {
			sr.wifi = &wifiTable{}
		}
		sr.wifi.at, sr.wifi.err = now, ""
		if wifiErr != nil {
			sr.wifi.err = snmpErrorText(wifiErr)
		} else {
			sr.wifi.clients, sr.wifi.aps = wifi, aps
		}
	}
	if fdbDue {
		sr.fdbAt, sr.fdbMethod, sr.fdbErr = now, fdb.method, ""
		if fdbErr != nil {
			sr.fdbErr = snmpErrorText(fdbErr)
		}
		if len(fdb.ports) > 0 || fdbErr == nil {
			sr.fdb = fdb.ports
		}
		sr.nb, sr.trunks = nb, trunks
	}
}

// deviceMatcherLocked vraća funkciju koja suseda povezuje sa uređajem iz
// konzole (po IP adresi ili nazivu), za link na njegove detalje.
func (m *netMonitor) deviceMatcherLocked() func(neighbor) string {
	byIP, byName := map[string]string{}, map[string]string{}
	for _, d := range m.devices {
		byIP[d.IP] = d.ID
		byName[strings.ToLower(d.Name)] = d.ID
	}
	return func(n neighbor) string {
		if id, ok := byIP[n.IP]; ok && n.IP != "" {
			return id
		}
		return byName[strings.ToLower(n.Name)]
	}
}

// readFDB čita MAC tabelu uređaja (po potrebi kroz VLAN kontekste).
func (m *netMonitor) readFDB(ctx context.Context, cfg SNMPConfig, ip string) (fdbResult, error) {
	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	return readFDB(func(contextName string) (snmpClient, error) {
		c := cfg
		c.contextName = contextName
		return dial(ctx, c, ip)
	})
}

// skipFDB: MAC tabela se ne čita za rutere (računari se vide na
// switch-evima, a ruteri je često ne daju preko SNMP-a).
func skipFDB(d NetDevice) bool { return d.Type == "router" }

// readWLC čita Wi-Fi klijente sa WLC-a.
func (m *netMonitor) readWLC(ctx context.Context, cfg SNMPConfig, ip string) (map[string]wifiClient, int, error) {
	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	c, err := dial(ctx, cfg, ip)
	if err != nil {
		return nil, 0, err
	}
	defer c.Close()
	return readWLC(c)
}

// readNeighbors čita CDP/LLDP susede uređaja.
func (m *netMonitor) readNeighbors(ctx context.Context, cfg SNMPConfig, ip string, ports []portSample) map[int][]neighbor {
	nb, _ := m.readPortRoles(ctx, cfg, ip, ports)
	return nb
}

// readPortRoles čita CDP/LLDP susede i koji portovi rade kao trunk.
func (m *netMonitor) readPortRoles(ctx context.Context, cfg SNMPConfig, ip string, ports []portSample) (map[int][]neighbor, map[int]bool) {
	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	c, err := dial(ctx, cfg, ip)
	if err != nil {
		return nil, nil
	}
	defer c.Close()
	return readNeighbors(c, ports), readTrunks(c)
}

// trunkFallback: za uređaje koji ne javljaju trunk status – port je veza ka
// mreži ako ima suseda koji je mrežna oprema, ako je uplink ili ako se kroz
// njega vidi mnogo adresa.
const trunkFallbackMACs = 30

// attachPorts dopunjava portove (nova kopija) susedima iz CDP/LLDP-a i
// računarima iz MAC tabele. Na portu sa više od maxAttached MAC adresa
// (uplink/trunk) računari se ne navode: kroz njega se vide svi uređaji iza
// drugog switch-a. Kada port ima suseda (npr. telefon), nepoznate MAC adrese
// su najverovatnije sam sused, pa se za njih ne piše „nepoznat uređaj”.
func attachPorts(ports []portView, fdb map[int][]string, nb map[int][]neighbor, trunks map[int]bool, uplink int,
	hosts map[string]attachedDev, devID func(neighbor) string) []portView {
	out := make([]portView, len(ports))
	copy(out, ports)
	for i := range out {
		p := &out[i]
		netNeighbor := false
		for _, n := range nb[p.Index] {
			n.DeviceID = devID(n)
			p.Neighbors = append(p.Neighbors, n)
			if !n.isPhoneOrAP() {
				netNeighbor = true
			}
		}
		macs := fdb[p.Index]
		p.MACCount = len(macs)
		if trunks != nil {
			p.Trunk = trunks[p.Index] || p.Index == uplink
		} else {
			p.Trunk = netNeighbor || p.Index == uplink || len(macs) > trunkFallbackMACs
		}
		if len(macs) == 0 {
			continue
		}
		if p.Trunk {
			behindSwitch(p, macs, hosts, uplink)
			continue
		}
		p.Shared = len(macs) > maxAttached
		sorted := slices.Clone(macs)
		sort.Strings(sorted)
		skipUnknown := len(p.Neighbors)
		for _, mac := range sorted {
			a, known := hosts[mac]
			if !known && skipUnknown > 0 {
				skipUnknown--
				continue
			}
			if !known && p.Shared {
				p.Unknown++ // na zajedničkom portu nepoznati samo brojem
				continue
			}
			a.MAC = mac
			p.Attached = append(p.Attached, a)
		}
	}
	return out
}

// behindMaxMACs: do ovoliko MAC adresa na trunk portu smatra se da je iza
// njega mali switch (kancelarija), a ne deo mreže sa mnogo uređaja.
const behindMaxMACs = 30

// behindSwitch navodi računare (agente) iza manjeg switch-a na trunk portu:
// samo ako port nije uplink, ima najviše behindMaxMACs adresa i switch iza
// njega nije dodat u konzolu (tada se računari vide u njegovim detaljima).
func behindSwitch(p *portView, macs []string, hosts map[string]attachedDev, uplink int) {
	if p.Index == uplink || len(macs) > behindMaxMACs {
		return
	}
	for _, n := range p.Neighbors {
		if n.isPhoneOrAP() || n.Indirect {
			continue
		}
		if n.DeviceID != "" {
			return // switch je u konzoli
		}
		if p.Via == "" {
			p.Via = n.Name
		}
	}
	sorted := slices.Clone(macs)
	sort.Strings(sorted)
	for _, mac := range sorted {
		if a, ok := hosts[mac]; ok && a.Name != "" {
			a.MAC = mac
			p.Behind = append(p.Behind, a)
		}
	}
	if len(p.Behind) == 0 {
		p.Via = ""
	}
}

func (m *netMonitor) dialFor(ctx context.Context, cfg SNMPConfig, ip string) (snmpClient, error) {
	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	return dial(ctx, cfg, ip)
}

func (m *netMonitor) readDevice(ctx context.Context, cfg SNMPConfig, ip string, full bool) (snmpSystem, []portSample, error) {
	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	c, err := dial(ctx, cfg, ip)
	if err != nil {
		return snmpSystem{}, nil, err
	}
	defer c.Close()
	sys, err := readSystem(c, full)
	if err != nil {
		return sys, nil, err
	}
	ports, err := readPorts(c)
	return sys, ports, err
}

// ---------- pregled ----------

// deviceView je uređaj sa trenutnim stanjem (za web interfejs).
type deviceView struct {
	NetDevice
	LocationName string     `json:"location_name"`
	Status       string     `json:"status"`
	Since        time.Time  `json:"since,omitzero"`
	RTT          *float64   `json:"rtt_ms,omitempty"`
	Loss         *float64   `json:"loss_pct,omitempty"`
	LastCheck    time.Time  `json:"last_check,omitzero"`
	Misses       int        `json:"misses,omitempty"`
	LocStatus    string     `json:"loc_status,omitempty"`
	SNMPInfo     *snmpView  `json:"snmp_info,omitempty"`
	Ports        []portView `json:"ports,omitempty"`
	// Uplink port (izabran ili automatski prepoznat), samo u detaljima.
	UplinkIndex int    `json:"uplink_index,omitempty"`
	UplinkName  string `json:"uplink_name,omitempty"`
	UplinkAuto  bool   `json:"uplink_auto,omitempty"`
}

// pickUplink vraća uplink port: zadat po nazivu, a inače automatski – port
// čiji opis sadrži „uplink”, zatim najbrži aktivni port (pri istoj brzini
// onaj sa najviše saobraćaja).
func pickUplink(ports []portView, name string) (portView, bool, bool) {
	if name != "" {
		for _, p := range ports {
			if strings.EqualFold(p.Name, name) {
				return p, false, true
			}
		}
	}
	var best portView
	found := false
	score := func(p portView) (int, int64, float64) {
		alias := 0
		if strings.Contains(strings.ToLower(p.Alias), "uplink") {
			alias = 1
		}
		var bps float64
		if p.InBps != nil && p.OutBps != nil {
			bps = *p.InBps + *p.OutBps
		}
		return alias, p.Speed, bps
	}
	for _, p := range ports {
		if p.Oper != "up" {
			continue
		}
		if !found {
			best, found = p, true
			continue
		}
		a1, s1, b1 := score(p)
		a2, s2, b2 := score(best)
		if a1 > a2 || (a1 == a2 && (s1 > s2 || (s1 == s2 && b1 > b2))) {
			best = p
		}
	}
	return best, true, found
}

type snmpView struct {
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked,omitzero"`
	LastOK  time.Time `json:"last_ok,omitzero"`
	snmpSystem
	PortsTotal int `json:"ports_total"`
	PortsUp    int `json:"ports_up"`
	// MAC tabela: način čitanja, greška i vreme poslednjeg čitanja.
	FDBMethod string    `json:"fdb_method,omitempty"`
	FDBError  string    `json:"fdb_error,omitempty"`
	FDBAt     time.Time `json:"fdb_at,omitzero"`
	// WiFi: Wi-Fi klijenti (samo WLC, u detaljima).
	WiFi *wifiView `json:"wifi,omitempty"`
	// Temp: temperatura (switch-evi; senzori samo u detaljima).
	Temp *tempView `json:"temp,omitempty"`
}

func (m *netMonitor) viewLocked(d NetDevice, locs map[string]*locSummary, withPorts bool) deviceView {
	st := m.rt(d.ID)
	v := deviceView{NetDevice: d, LocationName: m.locationOf(d), Status: st.status, Since: st.since,
		LastCheck: st.lastCheck, Misses: st.misses}
	if !d.Active {
		v.Status = stInactive
	}
	if !math.IsNaN(st.rtt) && st.status == stUp {
		v.RTT = ptr(math.Round(st.rtt*100) / 100)
	}
	v.Loss = st.lossPct()
	if l := locs[v.LocationName]; l != nil {
		v.LocStatus = l.Status
	}
	if sr := st.snmp; sr != nil && d.SNMP && !sr.checked.IsZero() {
		sv := &snmpView{OK: sr.ok, Error: sr.err, Checked: sr.checked, LastOK: sr.lastOK, snmpSystem: sr.sys, PortsTotal: len(sr.ports)}
		for _, p := range sr.ports {
			if p.Oper == "up" {
				sv.PortsUp++
			}
		}
		v.SNMPInfo = sv
		sv.FDBMethod, sv.FDBError, sv.FDBAt = sr.fdbMethod, sr.fdbErr, sr.fdbAt
		if d.Type == "switch" {
			sv.Temp = newTempView(sr.temps, st.temp, withPorts)
		}
		if withPorts && sr.wifi != nil && d.Type == wlcType {
			hosts := map[string]attachedDev{}
			if m.macHosts != nil {
				hosts = m.macHosts()
			}
			sv.WiFi = newWiFiView(sr.wifi, hosts)
		}
		if withPorts {
			v.Ports = sr.ports
			if up, auto, ok := pickUplink(sr.ports, d.Uplink); ok {
				v.UplinkIndex, v.UplinkName, v.UplinkAuto = up.Index, up.Name, auto
			}
			if sr.fdb != nil || sr.nb != nil {
				hosts := map[string]attachedDev{}
				if m.macHosts != nil {
					hosts = m.macHosts()
				}
				v.Ports = attachPorts(sr.ports, sr.fdb, sr.nb, sr.trunks, v.UplinkIndex, hosts, m.deviceMatcherLocked())
				attachWiFi(v.Ports, m.wifiClientsLocked(), hosts)
			}
		}
	}
	return v
}

type netdevResponse struct {
	Devices        []deviceView      `json:"devices"`
	Locations      []*locSummary     `json:"locations"`
	Types          map[string]string `json:"types"`
	SNMPConfigured bool              `json:"snmp_configured"`
	IntervalSec    int               `json:"interval_sec"`
}

func (m *netMonitor) overview() netdevResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	locs := m.summarizeLocationsLocked()
	resp := netdevResponse{Types: deviceTypes, SNMPConfigured: m.snmpCfg.configured(), IntervalSec: int(pingEvery.Seconds()),
		Devices: make([]deviceView, 0, len(m.devices)), Locations: []*locSummary{}}
	for _, d := range m.devices {
		resp.Devices = append(resp.Devices, m.viewLocked(d, locs, false))
	}
	for _, l := range locs {
		resp.Locations = append(resp.Locations, l)
	}
	sort.Slice(resp.Locations, func(i, j int) bool { return resp.Locations[i].Name < resp.Locations[j].Name })
	return resp
}

func (m *netMonitor) device(id string) (deviceView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.devices, func(d NetDevice) bool { return d.ID == id })
	if i < 0 {
		return deviceView{}, false
	}
	return m.viewLocked(m.devices[i], m.summarizeLocationsLocked(), true), true
}

func (m *netMonitor) find(id string) (NetDevice, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.devices, func(d NetDevice) bool { return d.ID == id })
	if i < 0 {
		return NetDevice{}, false
	}
	return m.devices[i], true
}

// recentEvents vraća najnovije događaje (najnoviji prvi).
func (m *netMonitor) recentEvents(deviceID string, limit int) []NetEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []NetEvent{}
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		if deviceID == "" || m.events[i].DeviceID == deviceID {
			out = append(out, m.events[i])
		}
	}
	return out
}

// ---------- izmene spiska ----------

// setDevices zamenjuje spisak (posle izmene ili uvoza) i snima ga.
func (m *netMonitor) setDevices(list []NetDevice) error {
	if err := m.file.save(list); err != nil {
		return fmt.Errorf("snimanje spiska uređaja: %w", err)
	}
	m.mu.Lock()
	old := map[string]NetDevice{}
	for _, d := range m.devices {
		old[d.ID] = d
	}
	for _, d := range list {
		// Promenjena adresa ili aktivnost: dosadašnje stanje više ne važi.
		if o, ok := old[d.ID]; ok && (o.IP != d.IP || o.Active != d.Active) {
			delete(m.state, d.ID)
		}
		delete(old, d.ID)
	}
	for id := range old { // uklonjeni
		delete(m.state, id)
	}
	m.devices = list
	m.mu.Unlock()
	if m.hist != nil {
		for id := range old {
			go m.hist.DeletePrefix("rtt/" + id)
			go m.hist.DeletePrefix("if/" + id + "/")
			go m.hist.DeletePrefix("mpls/" + id)
		}
	}
	return nil
}

func (m *netMonitor) list() []NetDevice {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.devices)
}

func (m *netMonitor) setSNMP(c *SNMPConfig) {
	m.mu.Lock()
	m.snmpCfg = c
	for _, st := range m.state {
		st.snmp = nil
	}
	m.mu.Unlock()
}

func (m *netMonitor) snmpConfig() *SNMPConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snmpCfg == nil {
		return nil
	}
	c := *m.snmpCfg
	return &c
}

// ipLocator vraća funkciju koja određuje lokaciju po IP adresi.
func (s *Server) ipLocator() func(string) string {
	return func(ip string) string {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			return ""
		}
		_, loc := s.currentLocations()
		return loc.find(a)
	}
}
