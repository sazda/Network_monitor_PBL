package console

import (
	"context"
	"io"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

// netSentLog beleži poslata obaveštenja (šalju se iz pozadinske gorutine).
type netSentLog struct {
	mu   sync.Mutex
	msgs []notifyMessage
}

func (l *netSentLog) snapshot() []notifyMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]notifyMessage(nil), l.msgs...)
}

// testMonitor pravi netMonitor sa lažnim slanjem obaveštenja, bez pozadinskih petlji.
func testMonitor(t *testing.T, devs []NetDevice, locate func(string) string) (*netMonitor, *netSentLog) {
	t.Helper()
	n := newNotifier(NotifyConfig{Enabled: true, NtfyURL: "https://ntfy.example/x"}, log.New(io.Discard, "", 0), nil)
	sent := &netSentLog{}
	n.send = func(_ context.Context, _ NotifyConfig, m notifyMessage) []channelResult {
		sent.mu.Lock()
		sent.msgs = append(sent.msgs, m)
		sent.mu.Unlock()
		return []channelResult{{Channel: "ntfy", OK: true}}
	}
	if locate == nil {
		locate = func(string) string { return "" }
	}
	m := newNetMonitor(t.TempDir(), nil, log.New(io.Discard, "", 0), n, locate)
	t.Cleanup(func() {
		if m.hist != nil {
			m.hist.Close()
		}
	})
	for i := range devs {
		if devs[i].ID == "" {
			devs[i].ID = devs[i].Name
		}
	}
	m.devices = devs
	return m, sent
}

// round simulira jednu proveru: up[ime] = da li uređaj odgovara.
func round(m *netMonitor, now time.Time, up map[string]bool) {
	var list []NetDevice
	var res []pingOutcome
	for _, d := range m.devices {
		if !d.Active {
			continue
		}
		list = append(list, d)
		if up[d.Name] {
			res = append(res, pingOutcome{rtt: 2, sent: 2, recv: 2})
		} else {
			res = append(res, pingOutcome{rtt: math.NaN(), sent: 2})
		}
	}
	m.apply(list, res, now)
}

func waitSent(t *testing.T, sent *netSentLog, n int) []notifyMessage {
	t.Helper()
	for i := 0; i < 100; i++ {
		if len(sent.snapshot()) >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sent.snapshot()
}

func TestNetDeviceDownAfterThreeMisses(t *testing.T) {
	m, sent := testMonitor(t, []NetDevice{{Name: "HQ_DC_S-I", IP: "10.0.35.15", Active: true, Notify: true}}, nil)
	t0 := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	round(m, t0, map[string]bool{"HQ_DC_S-I": true})
	if st := m.rt("HQ_DC_S-I"); st.status != stUp {
		t.Fatalf("posle odgovora: %s", st.status)
	}
	// Dve provere bez odgovora: i dalje „na vezi”.
	round(m, t0.Add(30*time.Second), nil)
	round(m, t0.Add(60*time.Second), nil)
	if st := m.rt("HQ_DC_S-I"); st.status != stUp {
		t.Fatalf("posle 2 neodgovora: %s", st.status)
	}
	round(m, t0.Add(90*time.Second), nil)
	st := m.rt("HQ_DC_S-I")
	if st.status != stDown || !st.since.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("posle 3 neodgovora: %s od %v", st.status, st.since)
	}
	msgs := waitSent(t, sent, 1)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Title, "HQ_DC_S-I nije na vezi") {
		t.Fatalf("obaveštenje o padu: %+v", msgs)
	}
	round(m, t0.Add(120*time.Second), map[string]bool{"HQ_DC_S-I": true})
	msgs = waitSent(t, sent, 2)
	if len(msgs) != 2 || !strings.Contains(msgs[1].Title, "ponovo na vezi") {
		t.Fatalf("obaveštenje o povratku: %+v", msgs)
	}
	evs := m.recentEvents("", 10)
	if len(evs) != 2 || evs[0].Kind != "up" || evs[1].Kind != "down" {
		t.Fatalf("događaji: %+v", evs)
	}
}

func TestNetDeviceWithoutNotifyAndInactive(t *testing.T) {
	m, sent := testMonitor(t, []NetDevice{
		{Name: "A", IP: "10.0.35.1", Active: true, Notify: false},
		{Name: "B", IP: "10.0.35.2", Active: false, Notify: true},
	}, nil)
	t0 := time.Now()
	for i := 0; i < 4; i++ {
		round(m, t0.Add(time.Duration(i)*30*time.Second), nil)
	}
	if m.rt("A").status != stDown {
		t.Fatalf("A: %s", m.rt("A").status)
	}
	if _, ok := m.state["B"]; ok {
		t.Fatal("neaktivan uređaj ne sme da se proverava")
	}
	time.Sleep(50 * time.Millisecond)
	if msgs := sent.snapshot(); len(msgs) != 0 {
		t.Fatalf("bez obaveštenja: %+v", msgs)
	}
	for _, d := range m.overview().Devices {
		if d.Name == "B" && d.Status != stInactive {
			t.Fatalf("B: %s", d.Status)
		}
	}
}

func locStatus(r netdevResponse, name string) string {
	for _, l := range r.Locations {
		if l.Name == name {
			return l.Status
		}
	}
	return ""
}

func anyText(msgs []notifyMessage, s string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Title, s) || strings.Contains(m.Body, s) {
			return true
		}
	}
	return false
}

func TestLocationBackupAndDown(t *testing.T) {
	loc := func(ip string) string {
		if strings.HasPrefix(ip, "10.24.") {
			return "Sever"
		}
		return "Glavni Centar"
	}
	m, sent := testMonitor(t, []NetDevice{
		{Name: "CE-SU", IP: "10.24.0.1", Role: "wan", Active: true, Notify: true},
		{Name: "RK00SW00", IP: "10.24.35.1", Active: true, Notify: true},
		{Name: "RK01SW01", IP: "10.24.35.2", Active: true, Notify: true},
		{Name: "HQ_DC_S-I", IP: "10.0.35.15", Active: true, Notify: true},
	}, loc)
	t0 := time.Now()
	all := map[string]bool{"CE-SU": true, "RK00SW00": true, "RK01SW01": true, "HQ_DC_S-I": true}
	round(m, t0, all)
	// MPLS pada: ruter ne odgovara, switch-evi rade (rezervna veza).
	noWAN := map[string]bool{"RK00SW00": true, "RK01SW01": true, "HQ_DC_S-I": true}
	for i := 1; i <= 3; i++ {
		round(m, t0.Add(time.Duration(i)*30*time.Second), noWAN)
	}
	if s := locStatus(m.overview(), "Sever"); s != stBackup {
		t.Fatalf("Sever: %s", s)
	}
	if msgs := waitSent(t, sent, 1); !anyText(msgs, "rezervne veze") {
		t.Fatalf("obaveštenje o rezervnoj vezi: %+v", msgs)
	}
	// Pada i rezerva: cela lokacija nedostupna – jedno obaveštenje za lokaciju,
	// bez posebnih za switch-eve.
	n := len(sent.snapshot())
	only := map[string]bool{"HQ_DC_S-I": true}
	for i := 4; i <= 6; i++ {
		round(m, t0.Add(time.Duration(i)*30*time.Second), only)
	}
	if s := locStatus(m.overview(), "Sever"); s != stLocDown {
		t.Fatalf("Sever: %s", s)
	}
	msgs := waitSent(t, sent, n+1)
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Title, "Lokacija Sever nedostupna") || strings.Contains(last.Body, "RK00SW00 nije na vezi") {
		t.Fatalf("obaveštenje o lokaciji: %+v", last)
	}
	// Sve se vraća.
	n = len(sent.snapshot())
	round(m, t0.Add(7*30*time.Second), all)
	if s := locStatus(m.overview(), "Sever"); s != stOK {
		t.Fatalf("Sever: %s", s)
	}
	msgs = waitSent(t, sent, n+1)
	if !strings.Contains(msgs[len(msgs)-1].Body, "ponovo dostupna") {
		t.Fatalf("povratak lokacije: %+v", msgs[len(msgs)-1])
	}
}

func TestParseDeviceCSV(t *testing.T) {
	text := "BOMLokacija;Mesto;Naziv;IP;Tip;Uloga;Aktivan;Napomena\n" +
		"Glavni Centar;Server Sala;HQ_DC_S-I;10.0.35.15;switch;;da;test\n" +
		"Sever;Server Sala;RK00SW00;10.24.35.31;sw;;;\n" +
		"Sever;;CE;10.24.0.1;ruter;mpls;da;\n" +
		"Sever;Održavanje transporta;RK15SW01;10.24.35.15;switch;;ne;Ugašen\n" +
		"Dolina;;Loš;10.18.35.999;switch;;;\n"
	text = strings.Replace(text, "BOM", "\ufeff", 1)
	devs, errs := parseDeviceCSV(text)
	if len(devs) != 4 || len(errs) != 1 || !strings.Contains(errs[0], "red 6") {
		t.Fatalf("uređaji %d, greške %v", len(devs), errs)
	}
	if devs[0].Location != "Glavni Centar" || devs[0].Place != "Server Sala" || devs[0].Note != "test" || !devs[0].Active || !devs[0].SNMP {
		t.Fatalf("prvi: %+v", devs[0])
	}
	if devs[1].Type != "switch" || devs[2].Type != "router" || devs[2].Role != "wan" || devs[3].Active {
		t.Fatalf("tipovi/uloge: %+v", devs)
	}
	// Bez zaglavlja: podrazumevani redosled.
	devs, errs = parseDeviceCSV("Jug,,ZE_Fin,10.12.35.251\n")
	if len(devs) != 1 || len(errs) != 0 || devs[0].Name != "ZE_Fin" || devs[0].Location != "Jug" {
		t.Fatalf("bez zaglavlja: %+v %v", devs, errs)
	}
	// Uvoz postojeće adrese menja uređaj (ID ostaje).
	list := []NetDevice{{ID: "x1", Name: "staro", IP: "10.12.35.251"}}
	list, res := mergeDevices(list, devs, func() string { return "novi" })
	if res.Updated != 1 || res.Added != 0 || list[0].ID != "x1" || list[0].Name != "ZE_Fin" {
		t.Fatalf("spajanje: %+v %+v", res, list)
	}
}

func TestTSDBRawAndAggregate(t *testing.T) {
	db, err := openTSDB(t.TempDir(), 2, 31)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// xx:58 pre sat vremena (relativno u odnosu na sada: starije se automatski briše).
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-62 * time.Minute)
	for i := 0; i < 40; i++ { // 40 minuta, preko granice sata i 15-minutnih intervala
		ts := t0.Add(time.Duration(i) * time.Minute)
		if err := db.Add("if/d1/5", ts, []float64{float64(1000 * i), 500, 1, 0, 0, 2}); err != nil {
			t.Fatal(err)
		}
		v := 2.0
		if i%10 == 0 {
			v = math.NaN()
		}
		db.Add("rtt/d1", ts, []float64{v})
	}
	pts, err := db.Query("if/d1/5", t0, t0.Add(40*time.Minute), false)
	if err != nil || len(pts) != 40 || pts[3].V[0] != 3000 || pts[39].V[5] != 2 {
		t.Fatalf("raw: %d %v", len(pts), err)
	}
	if pts, _ := db.Query("if/d1/5", t0.Add(10*time.Minute), t0.Add(12*time.Minute), false); len(pts) != 3 {
		t.Fatalf("raw opseg: %d", len(pts))
	}
	agg, _ := db.Query("if/d1/5", t0.Add(-time.Hour), t0.Add(time.Hour), true)
	// Intervali: xx:45 (2 min), :00 (15), :15 (15), :30 (8, u toku).
	if len(agg) != 4 {
		t.Fatalf("agg: %d %+v", len(agg), agg)
	}
	if a := agg[1]; a.V[2] != 16000 || a.V[4] != 15 || a.V[7] != 30 {
		t.Fatalf("agg[1] max/zbir: %+v", a.V)
	}
	ragg, _ := db.Query("rtt/d1", t0.Add(-time.Hour), t0.Add(time.Hour), true)
	// :00–:14: merenja i=2..16, bez odgovora i=10 → gubitak 1/15.
	if a := ragg[1]; math.Abs(a.V[2]-100.0/15) > 0.01 || a.V[0] != 2 {
		t.Fatalf("rtt agg: %+v", a.V)
	}
	// Brisanje uređaja.
	db.DeletePrefix("if/d1/")
	if pts, _ := db.Query("if/d1/5", t0, t0.Add(time.Hour), false); len(pts) != 0 {
		t.Fatalf("posle brisanja: %d", len(pts))
	}
	// Brisanje starih dana.
	db.trim(t0.AddDate(0, 0, 3))
	if pts, _ := db.Query("rtt/d1", t0, t0.Add(time.Hour), false); len(pts) != 0 {
		t.Fatalf("stari dani: %d", len(pts))
	}
}

// fakeSNMP je lažni uređaj sa jednim portom.
type fakeSNMP struct {
	uptime   int64 // stotinke
	inOct    uint64
	outOct   uint64
	inErr    uint64
	failWith error
}

func (f *fakeSNMP) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	var out []gosnmp.SnmpPDU
	str := func(o, s string) gosnmp.SnmpPDU {
		return gosnmp.SnmpPDU{Name: o, Type: gosnmp.OctetString, Value: []byte(s)}
	}
	for _, o := range oids {
		switch o {
		case oidSysDescr:
			out = append(out, str(o, "Cisco IOS Software [Dublin], Catalyst L3 Switch Software (CAT9K_LITE_IOSXE), Version 17.18.2, RELEASE SOFTWARE (fc1)"))
		case oidSysObjectID:
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.9.1.2694"})
		case oidSysUpTime:
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.TimeTicks, Value: uint32(f.uptime)})
		case oidSysName:
			out = append(out, str(o, "HQ_DC_S-I"))
		case oidEntModel + ".1000":
			out = append(out, str(o, "C9200L-48P-4X"))
		case oidEntSerial + ".1000":
			out = append(out, str(o, "JAE24340LB9"))
		case oidIfHCIn + ".9":
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.Counter64, Value: f.inOct})
		case oidIfHCOut + ".9":
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.Counter64, Value: f.outOct})
		}
	}
	return out, nil
}

func (f *fakeSNMP) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	pdu := func(idx int, t gosnmp.Asn1BER, v any) []gosnmp.SnmpPDU {
		return []gosnmp.SnmpPDU{{Name: oid + "." + strconv.Itoa(idx), Type: t, Value: v}}
	}
	switch oid {
	case oidEntClass:
		return append(pdu(1, gosnmp.Integer, 11), pdu(1000, gosnmp.Integer, 3)...), nil
	case oidIfType:
		return append(pdu(9, gosnmp.Integer, 6), pdu(70, gosnmp.Integer, 53)...), nil // port + VLAN
	case oidIfName:
		return pdu(9, gosnmp.OctetString, []byte("Te1/1/1")), nil
	case oidIfAlias:
		return pdu(9, gosnmp.OctetString, []byte("Uplink")), nil
	case oidIfAdmin, oidIfOper:
		return pdu(9, gosnmp.Integer, 1), nil
	case oidIfHighSpeed:
		return pdu(9, gosnmp.Gauge32, uint(10000)), nil
	case oidIfHCIn:
		return pdu(9, gosnmp.Counter64, f.inOct), nil
	case oidIfHCOut:
		return pdu(9, gosnmp.Counter64, f.outOct), nil
	case oidIfInErrors:
		return pdu(9, gosnmp.Counter32, uint(f.inErr)), nil
	}
	return nil, nil
}

func (f *fakeSNMP) Close() {}

func TestSNMPPollRatesAndReboot(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{Name: "HQ_DC_S-I", IP: "10.0.35.15", Active: true, SNMP: true, Notify: true}}, nil)
	dev := &fakeSNMP{uptime: 100 * 3600 * 100, inOct: 1_000_000, outOct: 2_000_000}
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) { return dev, nil }
	now := time.Now().Add(-10 * time.Minute)
	m.now = func() time.Time { return now }
	cfg := SNMPConfig{Version: "v3", User: "netmon"}

	m.pollSNMP(cfg, m.devices[0])
	v, _ := m.device("HQ_DC_S-I")
	if v.SNMPInfo == nil || !v.SNMPInfo.OK || v.SNMPInfo.Version != "17.18.2" || v.SNMPInfo.Model != "C9200L-48P-4X" ||
		v.SNMPInfo.Serial != "JAE24340LB9" || v.SNMPInfo.Uptime != 360000 || len(v.Ports) != 1 {
		t.Fatalf("prvo čitanje: %+v %+v", v.SNMPInfo, v.Ports)
	}
	if v.Ports[0].InBps != nil {
		t.Fatal("brzina tek posle drugog čitanja")
	}
	// Minut kasnije: 7,5 MB ulaz = 1 Mb/s, 2 greške.
	now = now.Add(time.Minute)
	dev.uptime += 6000
	dev.inOct += 7_500_000
	dev.outOct += 15_000_000
	dev.inErr += 2
	m.pollSNMP(cfg, m.devices[0])
	v, _ = m.device("HQ_DC_S-I")
	p := v.Ports[0]
	if p.InBps == nil || *p.InBps != 1_000_000 || *p.OutBps != 2_000_000 || p.InErr != 2 || p.Name != "Te1/1/1" || p.Alias != "Uplink" {
		t.Fatalf("drugo čitanje: %+v", p)
	}
	if pts, _ := m.hist.Query("if/HQ_DC_S-I/9", now.Add(-time.Hour), now, false); len(pts) != 1 || pts[0].V[0] != 1_000_000 {
		t.Fatalf("istorija: %+v", pts)
	}
	// Restart: uptime je manji, brojači od nule.
	now = now.Add(time.Minute)
	dev.uptime = 3000
	dev.inOct, dev.outOct = 100, 100
	m.pollSNMP(cfg, m.devices[0])
	if evs := m.recentEvents("HQ_DC_S-I", 5); len(evs) == 0 || evs[0].Kind != "reboot" {
		t.Fatalf("restart: %+v", evs)
	}
	// Greška SNMP-a: zapis u događajima, stanje „ne radi”.
	now = now.Add(time.Minute)
	dev.failWith = io.ErrUnexpectedEOF
	m.pollSNMP(cfg, m.devices[0])
	v, _ = m.device("HQ_DC_S-I")
	if v.SNMPInfo.OK || v.SNMPInfo.Error == "" {
		t.Fatalf("greška: %+v", v.SNMPInfo)
	}
	if evs := m.recentEvents("HQ_DC_S-I", 1); evs[0].Kind != "snmp_error" {
		t.Fatalf("događaj greške: %+v", evs)
	}
}

func TestSNMPConfigValidate(t *testing.T) {
	c := SNMPConfig{Version: "v3", User: "netmon", AuthPass: "kratka", PrivPass: "dovoljnoduga"}
	if err := c.validate(); err == nil {
		t.Fatal("kratka lozinka")
	}
	c.AuthPass = "dovoljnoduga1"
	if err := c.validate(); err != nil || c.AuthProto != "SHA" || c.PrivProto != "AES" || c.Port != 161 || c.Interval != 60 {
		t.Fatalf("podrazumevano: %v %+v", err, c)
	}
	c = SNMPConfig{Version: "v2c"}
	if err := c.validate(); err == nil {
		t.Fatal("v2c bez community-ja")
	}
}

func TestCounterDelta(t *testing.T) {
	if d, ok := counterDelta(math.MaxUint32-5, 4); !ok || d != 10 {
		t.Fatalf("prelaz preko nule: %d %v", d, ok)
	}
	if _, ok := counterDelta(100, 5); ok {
		t.Fatal("reset brojača")
	}
}

func TestPickUplink(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	ports := []portView{
		{Index: 1, Name: "Gi1/0/1", Oper: "up", Speed: 1000, InBps: f(5e6), OutBps: f(1e6)},
		{Index: 49, Name: "Te1/1/1", Oper: "up", Speed: 10000, InBps: f(1e6), OutBps: f(2e7)},
		{Index: 50, Name: "Te1/1/2", Oper: "down", Speed: 10000},
	}
	if p, auto, ok := pickUplink(ports, ""); !ok || !auto || p.Name != "Te1/1/1" {
		t.Fatalf("najbrži aktivni: %+v %v %v", p, auto, ok)
	}
	ports[0].Alias = "UPLINK ka jezgru"
	if p, _, _ := pickUplink(ports, ""); p.Name != "Gi1/0/1" {
		t.Fatalf("opis „uplink”: %+v", p)
	}
	if p, auto, ok := pickUplink(ports, "te1/1/2"); !ok || auto || p.Index != 50 {
		t.Fatalf("zadat: %+v %v %v", p, auto, ok)
	}
	if _, _, ok := pickUplink([]portView{{Name: "Gi1/0/1", Oper: "down"}}, ""); ok {
		t.Fatal("bez aktivnih portova")
	}
}

func TestLiveRate(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{Name: "HQ_DC_S-I", IP: "10.0.35.15", Active: true, SNMP: true}}, nil)
	dev := &fakeSNMP{uptime: 100, inOct: 1000, outOct: 2000}
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) { return dev, nil }
	now := time.Now()
	m.now = func() time.Time { return now }
	cfg := SNMPConfig{Version: "v3", User: "netmon"}
	r, err := m.liveRate(context.Background(), cfg, m.devices[0], 9)
	if err != nil || r.InBps != nil {
		t.Fatalf("prvo očitavanje: %+v %v", r, err)
	}
	// Ponovni zahtev odmah: isto očitavanje, bez novog upita.
	dev.inOct += 1_000_000
	if r, _ := m.liveRate(context.Background(), cfg, m.devices[0], 9); r.InBps != nil {
		t.Fatalf("keš: %+v", r)
	}
	now = now.Add(5 * time.Second)
	dev.outOct += 5_000_000
	r, err = m.liveRate(context.Background(), cfg, m.devices[0], 9)
	if err != nil || r.InBps == nil || *r.InBps != 1_600_000 || *r.OutBps != 8_000_000 {
		t.Fatalf("drugo očitavanje: %+v %v", r, err)
	}
}
