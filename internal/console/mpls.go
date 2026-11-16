package console

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// MPLS veze lokacija (operater): na lokaciji su dva CE rutera u VRRP-u –
// primarni i sekundarni (rezervni; optika ili radio-relejni). CE ruter je uređaj
// iz spiska mrežne opreme sa poljem MPLSLink; prikazuje se samo u kartici
// MPLS. Konzola preko SNMP-a čita njegov WAN interfejs (stanje, saobraćaj),
// BGP sesije ka operateru i VRRP stanje.

const (
	mplsPrimary   = "primary"
	mplsSecondary = "secondary"

	oidBgpPeerState  = ".1.3.6.1.2.1.15.3.1.2"   // BGP4-MIB: 6 = established
	oidVrrpOperState = ".1.3.6.1.2.1.68.1.3.1.3" // VRRP-MIB: 1 init, 2 backup, 3 master
	mplsDownAfter    = 2                         // uzastopna loša čitanja pre „ne radi”
	mplsIfEvery      = time.Hour                 // ponovno čitanje spiska interfejsa
	mplsEvery        = 30 * time.Second          // čitanje stanja linka (WAN, BGP, VRRP)
)

// mplsIface je interfejs rutera (za izbor WAN interfejsa).
type mplsIface struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
}

// mplsRuntime je stanje MPLS linka jednog CE rutera.
type mplsRuntime struct {
	ifaces          []mplsIface
	ifAt            time.Time
	ifIndex         int
	ifName, ifAlias string
	auto            bool // interfejs je izabran automatski (opis sadrži „WAN”)

	oper                   string // up, down, …
	bgpPeers               int
	bgpUp                  int
	vrrp                   string // master (sve grupe), partial (deo grupa), backup, init, ""
	vrrpGroups, vrrpMaster int
	inBps                  *float64
	outBps                 *float64
	prevIn                 *big.Int
	prevOut                *big.Int
	prevAt                 time.Time
	bad                    int
	linkUp                 *bool
	since                  time.Time
	reason                 string
	err                    string
	checkedAt              time.Time
}

// mplsSample je jedno čitanje sa rutera.
type mplsSample struct {
	ifaces          []mplsIface
	ifIndex         int
	ifName, ifAlias string
	auto            bool
	oper            int
	in, out         *big.Int
	bgp             []int
	vrrp            []int
}

// pickWANIface bira WAN interfejs: zadat naziv, inače interfejs čiji opis
// sadrži „L3VPN”, pa podinterfejs sa „WAN” u opisu, pa bilo koji sa „WAN”.
// Ako nijedan opis nema „WAN”, bira jedini podinterfejs fizičkog porta
// (npr. Gi0/0/1.2700, bez ServiceInstance); ako ih je više, ne pogađa.
func pickWANIface(ifaces []mplsIface, want string) (mplsIface, bool, error) {
	if want != "" {
		for _, f := range ifaces {
			if strings.EqualFold(f.Name, want) || strings.EqualFold(shortIfName(f.Name), shortIfName(want)) {
				return f, false, nil
			}
		}
		return mplsIface{}, false, fmt.Errorf("ruter nema interfejs %q", want)
	}
	score := func(f mplsIface) int {
		a := strings.ToUpper(f.Alias)
		switch {
		case strings.Contains(a, "L3VPN"):
			return 3
		case strings.Contains(a, "WAN") && strings.Contains(f.Name, "."):
			return 2
		case strings.Contains(a, "WAN"):
			return 1
		}
		return 0
	}
	best, bs := mplsIface{}, 0
	for _, f := range ifaces {
		if s := score(f); s > bs {
			best, bs = f, s
		}
	}
	if bs > 0 {
		return best, true, nil
	}
	var subs []mplsIface
	for _, f := range ifaces {
		n := shortIfName(f.Name)
		if strings.Contains(n, ".") && !strings.Contains(n, "serviceinstance") &&
			(strings.HasPrefix(n, "gi") || strings.HasPrefix(n, "te") || strings.HasPrefix(n, "fa") || strings.HasPrefix(n, "et")) {
			subs = append(subs, f)
		}
	}
	switch len(subs) {
	case 1:
		return subs[0], true, nil
	case 0:
		return mplsIface{}, true, errors.New("WAN interfejs nije pronađen (nijedan interfejs nema „WAN” u opisu, nema ni podinterfejsa) – izaberite ga u izmeni rutera")
	}
	var names []string
	for _, f := range subs {
		names = append(names, f.Name)
	}
	return mplsIface{}, true, fmt.Errorf("više mogućih WAN interfejsa (%s) – izaberite ga u izmeni rutera", strings.Join(names, ", "))
}

// shortIfName: „GigabitEthernet0/0/0.2804” → „gi0/0/0.2804” (poređenje).
func shortIfName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range [][2]string{{"tengigabitethernet", "te"}, {"gigabitethernet", "gi"}, {"fastethernet", "fa"}, {"ethernet", "et"}} {
		if strings.HasPrefix(s, p[0]) {
			return p[1] + s[len(p[0]):]
		}
	}
	return s
}

// readMPLS čita WAN interfejs, BGP i VRRP sa CE rutera. Spisak interfejsa se
// čita kada je needIfaces ili kada indeks još nije poznat.
func (m *netMonitor) readMPLS(ctx context.Context, cfg SNMPConfig, ip, want string, ifIndex int, needIfaces bool) (mplsSample, error) {
	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	c, err := dial(ctx, cfg, ip)
	if err != nil {
		return mplsSample{}, err
	}
	defer c.Close()
	var s mplsSample
	if needIfaces || ifIndex == 0 {
		names, err := c.BulkWalkAll(oidIfName)
		if err != nil {
			return s, err
		}
		alias := map[int]string{}
		if as, err := c.BulkWalkAll(oidIfAlias); err == nil {
			for _, p := range as {
				alias[lastIndex(p.Name)] = pduString(p)
			}
		}
		for _, p := range names {
			i := lastIndex(p.Name)
			s.ifaces = append(s.ifaces, mplsIface{Index: i, Name: pduString(p), Alias: alias[i]})
		}
		f, auto, err := pickWANIface(s.ifaces, want)
		if err != nil {
			return s, err // spisak interfejsa ipak ide u prikaz (izbor u izmeni rutera)
		}
		s.ifIndex, s.ifName, s.ifAlias, s.auto = f.Index, f.Name, f.Alias, auto
	} else {
		s.ifIndex = ifIndex
	}
	idx := fmt.Sprintf(".%d", s.ifIndex)
	vars, err := c.Get([]string{oidIfOper + idx, oidIfHCIn + idx, oidIfHCOut + idx})
	if err != nil {
		return s, err
	}
	for _, v := range vars {
		switch {
		case strings.HasPrefix(v.Name, oidIfOper+"."):
			s.oper = int(pduInt(v))
		case strings.HasPrefix(v.Name, oidIfHCIn+"."):
			s.in = pduBig(v)
		case strings.HasPrefix(v.Name, oidIfHCOut+"."):
			s.out = pduBig(v)
		}
	}
	if ps, err := c.BulkWalkAll(oidBgpPeerState); err == nil {
		for _, p := range ps {
			s.bgp = append(s.bgp, int(pduInt(p)))
		}
	}
	if ps, err := c.BulkWalkAll(oidVrrpOperState); err == nil {
		for _, p := range ps {
			s.vrrp = append(s.vrrp, int(pduInt(p)))
		}
	}
	return s, nil
}

// mplsLoop na mplsEvery čita stanje linka svih CE rutera koji odgovaraju na
// ping (češće od ostalog SNMP čitanja, da se prelazak na sekundarni link
// primeti brže).
func (m *netMonitor) mplsLoop() {
	defer m.wg.Done()
	select {
	case <-m.stop:
		return
	case <-time.After(10 * time.Second):
	}
	t := time.NewTicker(mplsEvery)
	defer t.Stop()
	for {
		m.mplsRound()
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
	}
}

func (m *netMonitor) mplsRound() {
	m.mu.Lock()
	cfg := m.snmpCfg
	var list []NetDevice
	if cfg.configured() {
		for _, d := range m.devices {
			if d.MPLSLink != "" && d.Active && d.SNMP && m.rt(d.ID).status == stUp {
				list = append(list, d)
			}
		}
	}
	var c SNMPConfig
	if cfg != nil {
		c = *cfg
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, d := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			m.pollMPLS(ctx, c.forDevice(d.SNMPProfile), d)
		}()
	}
	wg.Wait()
}

// pollMPLS čita MPLS stanje CE rutera.
func (m *netMonitor) pollMPLS(ctx context.Context, cfg SNMPConfig, d NetDevice) {
	m.mu.Lock()
	st := m.rt(d.ID)
	if st.mpls == nil {
		st.mpls = &mplsRuntime{}
	}
	mr := st.mpls
	ifIndex := mr.ifIndex
	need := m.now().Sub(mr.ifAt) > mplsIfEvery || !strings.EqualFold(mr.wantName(), d.MPLSIf)
	m.mu.Unlock()

	s, err := m.readMPLS(ctx, cfg, d.IP, d.MPLSIf, ifIndex, need)
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.ContainsFunc(m.devices, func(x NetDevice) bool { return x.ID == d.ID && x.MPLSLink != "" }) {
		return
	}
	mr = m.rt(d.ID).mpls
	if mr == nil {
		return
	}
	mr.checkedAt = now
	if s.ifaces != nil {
		mr.ifaces, mr.ifAt = s.ifaces, now
	}
	if err != nil {
		mr.err = snmpErrorText(err)
		return
	}
	mr.err = ""
	if s.ifaces != nil {
		mr.ifaces, mr.ifAt = s.ifaces, now
		if mr.ifIndex != s.ifIndex {
			mr.prevIn, mr.prevOut = nil, nil
		}
		mr.ifIndex, mr.ifName, mr.ifAlias, mr.auto = s.ifIndex, s.ifName, s.ifAlias, s.auto
	}
	mr.oper = operNames[s.oper]
	if mr.oper == "" {
		mr.oper = fmt.Sprint(s.oper)
	}
	mr.bgpPeers, mr.bgpUp = len(s.bgp), 0
	for _, b := range s.bgp {
		if b == 6 {
			mr.bgpUp++
		}
	}
	mr.vrrp, mr.vrrpGroups, mr.vrrpMaster = vrrpState(s.vrrp)
	// Saobraćaj.
	mr.inBps, mr.outBps = nil, nil
	if dt := now.Sub(mr.prevAt).Seconds(); mr.prevIn != nil {
		in, ok1 := octetRate(mr.prevIn, s.in, dt)
		out, ok2 := octetRate(mr.prevOut, s.out, dt)
		if ok1 && ok2 {
			mr.inBps, mr.outBps = ptr(math.Round(in)), ptr(math.Round(out))
			if m.hist != nil {
				// Isti raspored kao serija porta ([ulaz, izlaz, greške…]): skladište
				// za svaku vrstu serije očekuje tačan broj vrednosti.
				if err := m.hist.Add("mpls/"+d.ID, now, []float64{in, out, 0, 0, 0, 0}); err != nil {
					m.log.Printf("MPLS istorija %s: %v", d.Name, err)
				}
			}
		}
	}
	mr.prevIn, mr.prevOut, mr.prevAt = s.in, s.out, now

	// Stanje linka: WAN interfejs mora biti „up”, a BGP sesija sa
	// operaterom uspostavljena (ako ruter javlja BGP).
	reason := ""
	switch {
	case s.oper != 1:
		reason = fmt.Sprintf("WAN interfejs %s nije aktivan (%s)", shortIfName(mr.ifName), mr.oper)
	case mr.bgpPeers > 0 && mr.bgpUp == 0:
		reason = "BGP sesija sa operaterom nije uspostavljena"
	}
	loc := m.locationOf(d)
	label := mplsLabel(d.MPLSLink)
	if reason != "" {
		mr.bad++
		if mr.bad >= mplsDownAfter && (mr.linkUp == nil || *mr.linkUp) {
			wasKnown := mr.linkUp != nil
			mr.linkUp, mr.since, mr.reason = ptr(false), now, reason
			if wasKnown {
				ev := NetEvent{Time: now, Kind: "mpls_down", DeviceID: d.ID, Device: d.Name, Location: loc,
					Text: fmt.Sprintf("%s: %s MPLS link (%s) ne radi – %s", loc, label, d.Name, reason)}
				m.recordLocked([]NetEvent{ev})
				if d.Notify {
					m.mplsAlertLocked(ev, d, time.Time{})
				}
			}
		}
		return
	}
	mr.bad, mr.reason = 0, ""
	if mr.linkUp == nil || !*mr.linkUp {
		if mr.linkUp != nil {
			ev := NetEvent{Time: now, Kind: "mpls_up", DeviceID: d.ID, Device: d.Name, Location: loc,
				DurationSec: int64(now.Sub(mr.since).Seconds()),
				Text:        fmt.Sprintf("%s: %s MPLS link (%s) ponovo radi (nije radio %s)", loc, label, d.Name, fmtDuration(now.Sub(mr.since)))}
			m.recordLocked([]NetEvent{ev})
			downSince := mr.since
			mr.linkUp, mr.since = ptr(true), now
			if d.Notify {
				m.mplsAlertLocked(ev, d, downSince)
			}
			return
		}
		mr.linkUp, mr.since = ptr(true), now
	}
}

func (mr *mplsRuntime) wantName() string {
	if mr.auto {
		return ""
	}
	return mr.ifName
}

// vrrpState: ruter može imati više VRRP grupa (npr. po VLAN-u) i biti master
// samo za neke; „master” znači sve grupe, „partial” deo grupa.
func vrrpState(states []int) (string, int, int) {
	n, master, backup := len(states), 0, 0
	for _, v := range states {
		switch v {
		case 3:
			master++
		case 2:
			backup++
		}
	}
	switch {
	case n == 0:
		return "", 0, 0
	case master == n:
		return "master", n, master
	case master > 0:
		return "partial", n, master
	case backup > 0:
		return "backup", n, master
	}
	return "init", n, master
}

func mplsLabel(link string) string {
	if link == mplsSecondary {
		return "sekundarni"
	}
	return "primarni"
}

// mplsKinds: vrsta linka (samo za prikaz).
var mplsKinds = map[string]string{"": "", "optika": "optika", "rr": "radio-relejni"}

// mplsLinkOK: link CE rutera radi – ruter odgovara na ping i (ako je SNMP
// pročitan) WAN interfejs i BGP su u redu.
func mplsLinkOK(st *devRuntime) bool {
	if st.status != stUp {
		return false
	}
	return st.mpls == nil || st.mpls.linkUp == nil || *st.mpls.linkUp
}

// ---------- prikaz ----------

type mplsLinkView struct {
	DeviceID string    `json:"device_id"`
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	Status   string    `json:"status"` // ping: up, down, unknown, inactive
	RTT      *float64  `json:"rtt_ms,omitempty"`
	Loss     *float64  `json:"loss_pct,omitempty"`
	Since    time.Time `json:"since,omitzero"`

	LinkUp     *bool       `json:"link_up,omitempty"`
	LinkSince  time.Time   `json:"link_since,omitzero"`
	Reason     string      `json:"reason,omitempty"`
	IfIndex    int         `json:"if_index,omitempty"`
	IfName     string      `json:"if_name,omitempty"`
	IfAlias    string      `json:"if_alias,omitempty"`
	IfAuto     bool        `json:"if_auto,omitempty"`
	Oper       string      `json:"oper,omitempty"`
	BGPPeers   int         `json:"bgp_peers"`
	BGPUp      int         `json:"bgp_up"`
	VRRP       string      `json:"vrrp,omitempty"` // master, partial, backup, init
	VRRPGroups int         `json:"vrrp_groups,omitempty"`
	VRRPMaster int         `json:"vrrp_master,omitempty"`
	InBps      *float64    `json:"in_bps,omitempty"`  // ruter prima sa MPLS-a (ka lokaciji)
	OutBps     *float64    `json:"out_bps,omitempty"` // ruter šalje u MPLS (sa lokacije)
	SpeedMbps  int         `json:"speed_mbps,omitempty"`
	Kind       string      `json:"kind,omitempty"` // optika, radio-relejni
	Error      string      `json:"error,omitempty"`
	Checked    time.Time   `json:"checked,omitzero"`
	Ifaces     []mplsIface `json:"ifaces,omitempty"`

	SNMPProfile string `json:"snmp_profile,omitempty"`
	Model       string `json:"model,omitempty"`
	Version     string `json:"version,omitempty"`
}

type mplsSiteView struct {
	Location  string        `json:"location"`
	Status    string        `json:"status"` // stanje lokacije (ok, backup, down, partial…)
	Active    string        `json:"active,omitempty"`
	Primary   *mplsLinkView `json:"primary,omitempty"`
	Secondary *mplsLinkView `json:"secondary,omitempty"`
}

type mplsResponse struct {
	Sites  []mplsSiteView `json:"sites"`
	Events []NetEvent     `json:"events"`
}

func (m *netMonitor) mplsLinkViewLocked(d NetDevice, locs map[string]*locSummary) *mplsLinkView {
	dv := m.viewLocked(d, locs, false)
	v := &mplsLinkView{DeviceID: d.ID, Name: d.Name, IP: d.IP, Status: dv.Status, RTT: dv.RTT, Loss: dv.Loss, Since: dv.Since,
		SpeedMbps: d.MPLSSpeed, SNMPProfile: d.SNMPProfile, Kind: mplsKinds[d.MPLSKind]}
	if dv.SNMPInfo != nil {
		v.Model, v.Version = dv.SNMPInfo.Model, dv.SNMPInfo.Version
		if !dv.SNMPInfo.OK {
			v.Error = dv.SNMPInfo.Error
		}
	}
	if mr := m.rt(d.ID).mpls; mr != nil {
		v.LinkUp, v.LinkSince, v.Reason = mr.linkUp, mr.since, mr.reason
		v.IfIndex, v.IfName, v.IfAlias, v.IfAuto, v.Oper = mr.ifIndex, mr.ifName, mr.ifAlias, mr.auto, mr.oper
		v.BGPPeers, v.BGPUp, v.VRRP = mr.bgpPeers, mr.bgpUp, mr.vrrp
		v.VRRPGroups, v.VRRPMaster = mr.vrrpGroups, mr.vrrpMaster
		v.InBps, v.OutBps, v.Checked, v.Ifaces = mr.inBps, mr.outBps, mr.checkedAt, mr.ifaces
		if mr.err != "" {
			v.Error = mr.err
		}
	}
	// Ruter ne odgovara na ping: link se smatra neispravnim.
	if dv.Status == stDown {
		v.LinkUp = ptr(false)
		if v.Reason == "" {
			v.Reason = "ruter ne odgovara na ping"
		}
	}
	return v
}

func (m *netMonitor) mpls() mplsResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	locs := m.summarizeLocationsLocked()
	sites := map[string]*mplsSiteView{}
	for _, d := range m.devices {
		if d.MPLSLink == "" {
			continue
		}
		name := m.locationOf(d)
		s := sites[name]
		if s == nil {
			s = &mplsSiteView{Location: name}
			if l := locs[name]; l != nil {
				s.Status = l.Status
			}
			sites[name] = s
		}
		v := m.mplsLinkViewLocked(d, locs)
		if d.MPLSLink == mplsSecondary {
			s.Secondary = v
		} else {
			s.Primary = v
		}
	}
	out := mplsResponse{Sites: []mplsSiteView{}, Events: []NetEvent{}}
	for _, s := range sites {
		s.Active = mplsActive(s.Primary, s.Secondary)
		out.Sites = append(out.Sites, *s)
	}
	slices.SortFunc(out.Sites, func(a, b mplsSiteView) int { return strings.Compare(a.Location, b.Location) })
	for i := len(m.events) - 1; i >= 0 && len(out.Events) < 100; i-- {
		if strings.HasPrefix(m.events[i].Kind, "mpls_") {
			out.Events = append(out.Events, m.events[i])
		}
	}
	return out
}

// mplsActive: kojim linkom ide saobraćaj – po VRRP-u (master), inače po
// većem saobraćaju, inače primarni ako radi.
func mplsActive(p, s *mplsLinkView) string {
	for _, x := range []struct {
		v    *mplsLinkView
		name string
	}{{p, mplsPrimary}, {s, mplsSecondary}} {
		if x.v != nil && x.v.VRRP == "master" && (x.v.LinkUp == nil || *x.v.LinkUp) {
			return x.name
		}
	}
	traffic := func(v *mplsLinkView) float64 {
		if v == nil || v.InBps == nil || v.OutBps == nil {
			return -1
		}
		return *v.InBps + *v.OutBps
	}
	tp, ts := traffic(p), traffic(s)
	switch {
	case tp >= 0 && ts >= 0 && math.Max(tp, ts) > 50_000: // bar 50 kb/s, da šum ne odlučuje
		if tp >= ts {
			return mplsPrimary
		}
		return mplsSecondary
	case p != nil && p.LinkUp != nil && *p.LinkUp:
		return mplsPrimary
	case s != nil && s.LinkUp != nil && *s.LinkUp:
		return mplsSecondary
	}
	return ""
}

func (s *Server) handleMPLS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.net.mpls())
}

// mplsConflictLocked: na lokaciji može biti samo jedan primarni i jedan
// sekundarni CE ruter.
func (m *netMonitor) mplsConflict(d NetDevice) error {
	if d.MPLSLink == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	loc := m.locationOf(d)
	for _, x := range m.devices {
		if x.ID != d.ID && x.MPLSLink == d.MPLSLink && m.locationOf(x) == loc {
			return fmt.Errorf("lokacija %s već ima %s MPLS link (%s)", loc, mplsLabel(d.MPLSLink), x.Name)
		}
	}
	return nil
}

// pduBig: brojač kao veliki broj (nil ako ga uređaj nema).
func pduBig(v gosnmp.SnmpPDU) *big.Int {
	if v.Value == nil || v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance {
		return nil
	}
	return gosnmp.ToBigInt(v.Value)
}

// ---------- tabela uživo (5 s dok je kartica otvorena) ----------

// mplsLiveMinGap: ruter se ne čita češće (više gledalaca istovremeno).
const mplsLiveMinGap = 4 * time.Second

type mplsLiveState struct {
	in, out *big.Int
	at      time.Time
	view    mplsLiveView
}

// mplsLiveView: saobraćaj i stanje WAN interfejsa jednog CE rutera.
type mplsLiveView struct {
	T      int64    `json:"t"`
	InBps  *float64 `json:"in_bps,omitempty"` // nil kod prvog čitanja
	OutBps *float64 `json:"out_bps,omitempty"`
	Oper   string   `json:"oper,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// mplsLive čita brojače i stanje WAN interfejsa svih CE rutera koji
// odgovaraju na ping (jedan SNMP Get po ruteru).
func (m *netMonitor) mplsLive(ctx context.Context) map[string]mplsLiveView {
	type job struct {
		d   NetDevice
		cfg SNMPConfig
		idx int
	}
	m.mu.Lock()
	var jobs []job
	if m.snmpCfg.configured() {
		for _, d := range m.devices {
			st := m.rt(d.ID)
			if d.MPLSLink == "" || !d.Active || !d.SNMP || st.status != stUp || st.mpls == nil || st.mpls.ifIndex == 0 {
				continue
			}
			jobs = append(jobs, job{d, m.snmpCfg.forDevice(d.SNMPProfile), st.mpls.ifIndex})
		}
	}
	dial := m.dialSNMP
	m.mu.Unlock()

	out := map[string]mplsLiveView{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := m.mplsLiveOne(ctx, dial, j.d, j.cfg, j.idx)
			mu.Lock()
			out[j.d.ID] = v
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (m *netMonitor) mplsLiveOne(ctx context.Context, dial func(context.Context, SNMPConfig, string) (snmpClient, error), d NetDevice, cfg SNMPConfig, idx int) mplsLiveView {
	now := m.now()
	m.mplsLiveMu.Lock()
	prev := m.mplsLiveData[d.ID]
	if prev != nil && now.Sub(prev.at) < mplsLiveMinGap {
		v := prev.view
		m.mplsLiveMu.Unlock()
		return v
	}
	m.mplsLiveMu.Unlock()

	v := mplsLiveView{T: now.Unix()}
	c, err := dial(ctx, cfg, d.IP)
	if err != nil {
		v.Error = snmpErrorText(err)
		return v
	}
	defer c.Close()
	suffix := fmt.Sprintf(".%d", idx)
	vars, err := c.Get([]string{oidIfOper + suffix, oidIfHCIn + suffix, oidIfHCOut + suffix})
	if err != nil {
		v.Error = snmpErrorText(err)
		return v
	}
	var in, outB *big.Int
	for _, p := range vars {
		switch {
		case strings.HasPrefix(p.Name, oidIfOper+"."):
			v.Oper = operNames[int(pduInt(p))]
		case strings.HasPrefix(p.Name, oidIfHCIn+"."):
			in = pduBig(p)
		case strings.HasPrefix(p.Name, oidIfHCOut+"."):
			outB = pduBig(p)
		}
	}
	m.mplsLiveMu.Lock()
	defer m.mplsLiveMu.Unlock()
	if prev != nil {
		dt := now.Sub(prev.at).Seconds()
		ri, ok1 := octetRate(prev.in, in, dt)
		ro, ok2 := octetRate(prev.out, outB, dt)
		if ok1 && ok2 {
			v.InBps, v.OutBps = ptr(math.Round(ri)), ptr(math.Round(ro))
		}
	}
	if m.mplsLiveData == nil {
		m.mplsLiveData = map[string]*mplsLiveState{}
	}
	m.mplsLiveData[d.ID] = &mplsLiveState{in: in, out: outB, at: now, view: v}
	return v
}

func (s *Server) handleMPLSLive(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.net.mplsLive(ctx))
}
