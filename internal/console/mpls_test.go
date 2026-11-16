package console

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

// fakeCE glumi operater CE ruter (Cisco C1111) za MPLS čitanja.
type fakeCE struct {
	mu        sync.Mutex
	wan       string // naziv WAN podinterfejsa
	alias     string
	oper      int
	in, out   uint64
	bgp, vrrp []int
}

func (f *fakeCE) Close() {}

func (f *fakeCE) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gosnmp.SnmpPDU
	for _, o := range oids {
		switch {
		case strings.HasPrefix(o, oidIfOper+"."):
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.Integer, Value: f.oper})
		case strings.HasPrefix(o, oidIfHCIn+"."):
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.Counter64, Value: f.in})
		case strings.HasPrefix(o, oidIfHCOut+"."):
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.Counter64, Value: f.out})
		default: // sistemski podaci (readDevice)
			if o == oidSysName {
				out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.OctetString, Value: []byte("FIRMA_AD")})
			}
		}
	}
	return out, nil
}

func (f *fakeCE) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	str := func(i int, s string) gosnmp.SnmpPDU {
		return gosnmp.SnmpPDU{Name: oid + "." + strconv.Itoa(i), Type: gosnmp.OctetString, Value: []byte(s)}
	}
	num := func(vals []int) []gosnmp.SnmpPDU {
		var out []gosnmp.SnmpPDU
		for i, v := range vals {
			out = append(out, gosnmp.SnmpPDU{Name: oid + ".10.0.0." + strconv.Itoa(i+1), Type: gosnmp.Integer, Value: v})
		}
		return out
	}
	switch oid {
	case oidIfName:
		return []gosnmp.SnmpPDU{str(1, "Gi0/0/0"), str(2, "Gi0/1/0"), str(9, f.wan), str(12, "Vl3")}, nil
	case oidIfAlias:
		return []gosnmp.SnmpPDU{str(1, ""), str(2, "LAN"), str(9, f.alias), str(12, "LAN1")}, nil
	case oidBgpPeerState:
		return num(f.bgp), nil
	case oidVrrpOperState:
		return num(f.vrrp), nil
	}
	return nil, nil
}

func TestPickWANIface(t *testing.T) {
	opt := []mplsIface{{1, "Gi0/0/0", ""}, {9, "Gi0/0/0.2804", "WAN-L3VPN"}, {10, "Gi0/0/0.ServiceInstance.3000", ""}, {12, "Vl3", "LAN1"}}
	if f, auto, err := pickWANIface(opt, ""); err != nil || !auto || f.Index != 9 {
		t.Fatalf("optika: %+v %v %v", f, auto, err)
	}
	rr := []mplsIface{{1, "Gi0/0/0", "WAN"}, {9, "Gi0/0/0.2750", "WAN"}}
	if f, _, err := pickWANIface(rr, ""); err != nil || f.Index != 9 {
		t.Fatalf("RR: %+v", f)
	}
	if f, auto, err := pickWANIface(rr, "GigabitEthernet0/0/0.2750"); err != nil || auto || f.Index != 9 {
		t.Fatalf("zadat naziv: %+v %v", f, auto)
	}
	if _, _, err := pickWANIface(rr, "Gi0/0/9"); err == nil {
		t.Fatal("nepostojeći interfejs")
	}
	// Bez „WAN” u opisu: jedini podinterfejs fizičkog porta.
	noDesc := []mplsIface{{1, "Gi0/0/1", ""}, {5, "Gi0/0/1.2700", ""}, {6, "Gi0/0/0.ServiceInstance.3000", ""}, {7, "Vlan3000.ServiceInstance.3000", ""}, {12, "Vl3", "LAN1"}}
	if f, auto, err := pickWANIface(noDesc, ""); err != nil || !auto || f.Index != 5 {
		t.Fatalf("jedan podinterfejs: %+v %v", f, err)
	}
	two := append(noDesc, mplsIface{8, "Gi0/0/1.2701", ""})
	if _, _, err := pickWANIface(two, ""); err == nil || !strings.Contains(err.Error(), "Gi0/0/1.2700, Gi0/0/1.2701") {
		t.Fatalf("više mogućih: %v", err)
	}
	if _, _, err := pickWANIface([]mplsIface{{1, "Gi0/1/0", "LAN"}}, ""); err == nil {
		t.Fatal("bez WAN-a")
	}
}

func TestMPLSLinkStateAndLocation(t *testing.T) {
	devs := []NetDevice{
		{Name: "Firma_optika_FIL", IP: "10.11.21.2", Type: "router", Location: "Filijala", Active: true, SNMP: true, MPLSLink: mplsPrimary, MPLSSpeed: 30},
		{Name: "Firma_backup_RR_FIL", IP: "10.11.21.3", Type: "router", Location: "Filijala", Active: true, SNMP: true, MPLSLink: mplsSecondary, MPLSSpeed: 30},
		{Name: "Filijala-1", IP: "10.11.35.5", Type: "switch", Location: "Filijala", Active: true},
		// Stari operater uređaj sa ulogom „glavna veza”: ne odlučuje kada postoji MPLS.
		{Name: "MPLS ruter (operater)", IP: "172.16.100.11", Type: "router", Location: "Filijala", Role: "wan", Active: true},
	}
	m, _ := testMonitor(t, devs, nil)
	prim := &fakeCE{wan: "Gi0/0/0.2804", alias: "WAN-L3VPN", oper: 1, bgp: []int{6}, vrrp: []int{3}}
	sec := &fakeCE{wan: "Gi0/0/0.2750", alias: "WAN", oper: 1, bgp: []int{6}, vrrp: []int{2}}
	m.dialSNMP = func(_ context.Context, _ SNMPConfig, ip string) (snmpClient, error) {
		if ip == "10.11.21.2" {
			return prim, nil
		}
		return sec, nil
	}
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	m.now = func() time.Time { return now }
	for _, d := range m.devices {
		m.rt(d.ID).status = stUp
	}
	m.rt("MPLS ruter (operater)").status = stDown // u starom načinu bi lokacija bila „rezervna veza”
	cfg := SNMPConfig{Version: "v2c", Community: "x"}
	poll := func() {
		for _, d := range m.devices[:2] {
			m.pollMPLS(context.Background(), cfg, d)
		}
	}
	poll()
	now = now.Add(time.Minute)
	prim.in, prim.out = 60*1_500_000, 60*250_000 // 12 Mb/s ka lokaciji, 2 Mb/s sa lokacije
	poll()
	r := m.mpls()
	if len(r.Sites) != 1 {
		t.Fatalf("lokacije: %+v", r.Sites)
	}
	s := r.Sites[0]
	if s.Location != "Filijala" || s.Status != stPartial && s.Status != stOK || s.Active != mplsPrimary {
		t.Fatalf("stanje: %+v", s)
	}
	if p := s.Primary; p == nil || p.IfName != "Gi0/0/0.2804" || !p.IfAuto || p.BGPUp != 1 || p.VRRP != "master" ||
		p.InBps == nil || *p.InBps != 12_000_000 || *p.OutBps != 2_000_000 || p.LinkUp == nil || !*p.LinkUp {
		t.Fatalf("primarni: %+v", s.Primary)
	}
	if sv := s.Secondary; sv == nil || sv.IfName != "Gi0/0/0.2750" || sv.VRRP != "backup" {
		t.Fatalf("sekundarni: %+v", s.Secondary)
	}

	// BGP na primarnom pada: posle dva čitanja link ne radi, lokacija ide
	// preko rezervne veze, VRRP master prelazi na sekundarni.
	prim.mu.Lock()
	prim.bgp, prim.vrrp = []int{3}, []int{2}
	prim.mu.Unlock()
	sec.mu.Lock()
	sec.vrrp = []int{3}
	sec.mu.Unlock()
	now = now.Add(time.Minute)
	poll()
	if s := m.mpls().Sites[0]; s.Primary.LinkUp == nil || !*s.Primary.LinkUp {
		t.Fatal("jedno loše čitanje ne sme da obori link")
	}
	now = now.Add(time.Minute)
	poll()
	s = m.mpls().Sites[0]
	if s.Primary.LinkUp == nil || *s.Primary.LinkUp || !strings.Contains(s.Primary.Reason, "BGP") || s.Active != mplsSecondary || s.Status != stBackup {
		t.Fatalf("pad primarnog: %+v %+v", s, s.Primary)
	}
	if len(r.Events) != 0 {
		t.Fatalf("prvo čitanje ne pravi događaje: %+v", r.Events)
	}
	ev := m.mpls().Events
	if len(ev) != 1 || ev[0].Kind != "mpls_down" || !strings.Contains(ev[0].Text, "Filijala: primarni MPLS link (Firma_optika_FIL) ne radi – BGP") {
		t.Fatalf("događaj: %+v", ev)
	}
	// Povratak.
	prim.mu.Lock()
	prim.bgp = []int{6}
	prim.mu.Unlock()
	now = now.Add(5 * time.Minute)
	poll()
	ev = m.mpls().Events
	if len(ev) != 2 || ev[0].Kind != "mpls_up" || !strings.Contains(ev[0].Text, "ponovo radi (nije radio 5 min)") {
		t.Fatalf("povratak: %+v", ev)
	}
	// Ruter ne odgovara na ping: link ne radi.
	m.rt("Firma_optika_FIL").status = stDown
	if s := m.mpls().Sites[0]; *s.Primary.LinkUp || s.Primary.Reason == "" || s.Status != stBackup {
		t.Fatalf("ping: %+v", s.Primary)
	}
}

func TestMPLSConflict(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{ID: "a", Name: "A", IP: "10.11.21.2", Location: "Filijala", MPLSLink: mplsPrimary, Active: true}}, nil)
	if err := m.mplsConflict(NetDevice{ID: "b", Name: "B", IP: "10.11.21.4", Location: "Filijala", MPLSLink: mplsPrimary}); err == nil {
		t.Fatal("dva primarna na lokaciji")
	}
	if err := m.mplsConflict(NetDevice{ID: "b", Name: "B", IP: "10.11.21.3", Location: "Filijala", MPLSLink: mplsSecondary}); err != nil {
		t.Fatal(err)
	}
	if err := m.mplsConflict(NetDevice{ID: "a", Name: "A", IP: "10.11.21.2", Location: "Filijala", MPLSLink: mplsPrimary}); err != nil {
		t.Fatalf("isti uređaj: %v", err)
	}
}

func TestMPLSRoundUsesProfile(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{
		{Name: "Firma_optika_FIL", IP: "10.11.21.2", Type: "router", Location: "Filijala", Active: true, SNMP: true, MPLSLink: mplsPrimary, SNMPProfile: "CE"},
		{Name: "Filijala-1", IP: "10.11.35.5", Type: "switch", Location: "Filijala", Active: true, SNMP: true},
	}, nil)
	m.snmpCfg = &SNMPConfig{Version: "v3", User: "netmon", Profiles: []SNMPProfile{{Name: "CE", Version: "v2c", Community: "ce"}}}
	var mu sync.Mutex
	var seen []string
	m.dialSNMP = func(_ context.Context, c SNMPConfig, ip string) (snmpClient, error) {
		mu.Lock()
		seen = append(seen, ip+"/"+c.Version)
		mu.Unlock()
		return &fakeCE{wan: "Gi0/0/0.2804", alias: "WAN-L3VPN", oper: 1, bgp: []int{6}}, nil
	}
	for _, d := range m.devices {
		m.rt(d.ID).status = stUp
	}
	m.mplsRound()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "10.11.21.2/v2c" {
		t.Fatalf("čitanja: %v", seen)
	}
	if mr := m.rt("Firma_optika_FIL").mpls; mr == nil || mr.ifName != "Gi0/0/0.2804" || mr.bgpUp != 1 {
		t.Fatalf("stanje: %+v", mr)
	}
}

// Kada WAN interfejs nije pronađen, spisak interfejsa ipak ostaje za izbor.
func TestMPLSIfacesKeptOnError(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{Name: "RR", IP: "10.21.21.3", Type: "router", Location: "Zapad", Active: true, SNMP: true,
		MPLSLink: mplsSecondary, MPLSIf: "Gi9/9/9"}}, nil)
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) {
		return &fakeCE{wan: "Gi0/0/0.2751", oper: 1}, nil
	}
	m.pollMPLS(context.Background(), SNMPConfig{Version: "v2c", Community: "x"}, m.devices[0])
	v := m.mpls().Sites[0].Secondary
	if !strings.Contains(v.Error, `"Gi9/9/9"`) || len(v.Ifaces) != 4 {
		t.Fatalf("greška i interfejsi: %q %+v", v.Error, v.Ifaces)
	}
	// Bez zadatog naziva i bez „WAN” u opisu: jedini podinterfejs.
	m.devices[0].MPLSIf = ""
	m.pollMPLS(context.Background(), SNMPConfig{Version: "v2c", Community: "x"}, m.devices[0])
	if v := m.mpls().Sites[0].Secondary; v.Error != "" || v.IfName != "Gi0/0/0.2751" || !v.IfAuto {
		t.Fatalf("auto: %+v", v)
	}
}

// Saobraćaj WAN interfejsa se upisuje u istoriju (grafikon „Zauzeće WAN
// interfejsa”), i u sirovim zapisima i u agregatu.
func TestMPLSHistory(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{Name: "Firma_optika_FIL", IP: "10.11.21.2", Type: "router", Location: "Filijala", Active: true, SNMP: true,
		MPLSLink: mplsPrimary, MPLSSpeed: 30}}, nil)
	if m.hist == nil {
		t.Skip("istorija nije dostupna")
	}
	ce := &fakeCE{wan: "Gi0/0/0.2804", alias: "WAN-L3VPN", oper: 1, bgp: []int{6}}
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) { return ce, nil }
	now := time.Now().Add(-10 * time.Minute)
	m.now = func() time.Time { return now }
	cfg := SNMPConfig{Version: "v2c", Community: "x"}
	m.pollMPLS(context.Background(), cfg, m.devices[0])
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Second)
		ce.mu.Lock()
		ce.in += 30 * 1_000_000 // 8 Mb/s
		ce.out += 30 * 125_000  // 1 Mb/s
		ce.mu.Unlock()
		m.pollMPLS(context.Background(), cfg, m.devices[0])
	}
	pts, err := m.hist.Query("mpls/Firma_optika_FIL", time.Now().Add(-time.Hour), time.Now(), false)
	if err != nil || len(pts) != 3 || pts[0].V[0] != 8_000_000 || pts[0].V[1] != 1_000_000 {
		t.Fatalf("istorija: %v %+v", err, pts)
	}
}

func TestVRRPState(t *testing.T) {
	for _, c := range []struct {
		in        []int
		st        string
		n, master int
	}{
		{nil, "", 0, 0},
		{[]int{3}, "master", 1, 1},
		{[]int{3, 3, 2, 2, 2, 2, 2, 3}, "partial", 8, 3},
		{[]int{2, 2}, "backup", 2, 0},
		{[]int{1}, "init", 1, 0},
	} {
		st, n, master := vrrpState(c.in)
		if st != c.st || n != c.n || master != c.master {
			t.Errorf("%v: %s %d/%d", c.in, st, master, n)
		}
	}
	// Delimični master ne odlučuje: aktivni link po saobraćaju.
	up := true
	in, out, small := 5e6, 1e6, 1e3
	p := &mplsLinkView{VRRP: "partial", LinkUp: &up, InBps: &small, OutBps: &small}
	s := &mplsLinkView{VRRP: "partial", LinkUp: &up, InBps: &in, OutBps: &out}
	if a := mplsActive(p, s); a != mplsSecondary {
		t.Fatalf("aktivni: %s", a)
	}
}

func TestMPLSLiveAPI(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.net.setDevices([]NetDevice{
		{ID: "ce1", Name: "Firma_optika_FIL", IP: "10.11.21.2", Type: "router", Location: "Filijala", Active: true, SNMP: true, MPLSLink: mplsPrimary},
		{ID: "ce2", Name: "Firma_backup_FIL", IP: "10.11.21.3", Type: "router", Location: "Filijala", Active: true, SNMP: true, MPLSLink: mplsSecondary},
	}); err != nil {
		t.Fatal(err)
	}
	ce := &fakeCE{wan: "Gi0/0/0.2804", oper: 1}
	now := time.Now()
	s.net.mu.Lock()
	s.net.snmpCfg = &SNMPConfig{Version: "v2c", Community: "x"}
	s.net.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) { return ce, nil }
	s.net.now = func() time.Time { return now }
	s.net.rt("ce1").status = stUp
	s.net.rt("ce1").mpls = &mplsRuntime{ifIndex: 9}
	s.net.rt("ce2").status = stDown // ne odgovara na ping: ne čita se
	s.net.rt("ce2").mpls = &mplsRuntime{ifIndex: 9}
	s.net.mu.Unlock()

	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	get := func() map[string]mplsLiveView {
		w := do(h, "GET", "/api/mpls/live", "10.0.0.5:1", nil, "admin", "admin-lozinka")
		var r map[string]mplsLiveView
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
			t.Fatalf("live: %d %s", w.Code, w.Body)
		}
		return r
	}
	r := get()
	if v, ok := r["ce1"]; !ok || v.Oper != "up" || v.InBps != nil || len(r) != 1 {
		t.Fatalf("prvo čitanje: %+v", r)
	}
	// Ponovni zahtev u roku od 4 s: bez novog čitanja rutera.
	now = now.Add(2 * time.Second)
	if v := get()["ce1"]; v.T != r["ce1"].T {
		t.Fatalf("keš: %d != %d", v.T, r["ce1"].T)
	}
	now = now.Add(3 * time.Second)
	ce.mu.Lock()
	ce.in, ce.out, ce.oper = 5*1_000_000, 5*125_000, 2
	ce.mu.Unlock()
	r = get()
	if v := r["ce1"]; v.InBps == nil || *v.InBps != 8_000_000 || *v.OutBps != 1_000_000 || v.Oper != "down" {
		t.Fatalf("drugo čitanje: %+v", v)
	}
}
