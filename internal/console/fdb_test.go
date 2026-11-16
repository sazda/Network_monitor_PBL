package console

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

// fakeBridge je lažni switch za MAC tabelu: walks[kontekst][kolona] = PDU-ovi.
type fakeBridge struct {
	ctx   string
	walks map[string]map[string][]gosnmp.SnmpPDU
}

func (f *fakeBridge) Get([]string) ([]gosnmp.SnmpPDU, error) { return nil, nil }
func (f *fakeBridge) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	return f.walks[f.ctx][oid], nil
}
func (f *fakeBridge) Close() {}

func macOID(col, prefix, mac string) string {
	var parts []string
	for _, h := range strings.Split(mac, ":") {
		n, _ := strconv.ParseUint(h, 16, 8)
		parts = append(parts, strconv.Itoa(int(n)))
	}
	if prefix != "" {
		return col + "." + prefix + "." + strings.Join(parts, ".")
	}
	return col + "." + strings.Join(parts, ".")
}

func intPDU(name string, v int) gosnmp.SnmpPDU {
	return gosnmp.SnmpPDU{Name: name, Type: gosnmp.Integer, Value: v}
}

func TestReadFDBQBridge(t *testing.T) {
	sw := map[string]map[string][]gosnmp.SnmpPDU{"": {
		oidBasePortIf: {intPDU(oidBasePortIf+".1", 9), intPDU(oidBasePortIf+".49", 57)},
		oidQFdbPort: {
			intPDU(macOID(oidQFdbPort, "20", "00:1a:2b:3c:4d:5e"), 1),
			intPDU(macOID(oidQFdbPort, "20", "70:b5:e8:00:00:01"), 49),
			intPDU(macOID(oidQFdbPort, "1", "aa:aa:aa:aa:aa:aa"), 1), // sopstvena (status self)
		},
		oidQFdbStatus: {
			intPDU(macOID(oidQFdbStatus, "20", "00:1a:2b:3c:4d:5e"), 3),
			intPDU(macOID(oidQFdbStatus, "20", "70:b5:e8:00:00:01"), 3),
			intPDU(macOID(oidQFdbStatus, "1", "aa:aa:aa:aa:aa:aa"), 4),
		},
	}}
	res, err := readFDB(func(ctx string) (snmpClient, error) { return &fakeBridge{ctx: ctx, walks: sw}, nil })
	if err != nil || res.method != "Q-BRIDGE-MIB" || len(res.ports[9]) != 1 || res.ports[9][0] != "00:1a:2b:3c:4d:5e" || len(res.ports[57]) != 1 {
		t.Fatalf("Q-BRIDGE: %+v %v", res, err)
	}
}

func TestReadFDBPerVLAN(t *testing.T) {
	sw := map[string]map[string][]gosnmp.SnmpPDU{
		"": {
			oidVtpVlanState: {intPDU(oidVtpVlanState+".1.1", 1), intPDU(oidVtpVlanState+".1.20", 1), intPDU(oidVtpVlanState+".1.1002", 1)},
		},
		"vlan-20": {
			oidBasePortIf: {intPDU(oidBasePortIf+".5", 13)},
			oidFdbPort:    {intPDU(macOID(oidFdbPort, "", "00:1a:2b:3c:4d:5e"), 5)},
			oidFdbStatus:  {intPDU(macOID(oidFdbStatus, "", "00:1a:2b:3c:4d:5e"), 3)},
		},
	}
	var asked []string
	res, err := readFDB(func(ctx string) (snmpClient, error) {
		asked = append(asked, ctx)
		return &fakeBridge{ctx: ctx, walks: sw}, nil
	})
	if err != nil || len(res.ports[13]) != 1 || !strings.Contains(res.method, "VLAN") {
		t.Fatalf("po VLAN-u: %+v %v", res, err)
	}
	if strings.Join(asked, ",") != ",vlan-20" {
		t.Fatalf("konteksti: %v", asked)
	}
	// Kontekst nije dozvoljen: jasna poruka.
	_, err = readFDB(func(ctx string) (snmpClient, error) {
		if ctx != "" {
			return nil, errors.New("request timeout")
		}
		return &fakeBridge{ctx: ctx, walks: sw}, nil
	})
	if !errors.Is(err, errFDBContext) {
		t.Fatalf("bez konteksta: %v", err)
	}
	// Kontekst nije dozvoljen, a switch vraća prazan odgovor umesto greške
	// (kancelarija_50 bez linije „context vlan-”): i to je upozorenje.
	sw["vlan-30"] = nil
	sw[""][oidVtpVlanState] = append(sw[""][oidVtpVlanState], intPDU(oidVtpVlanState+".1.30", 1))
	_, err = readFDB(func(ctx string) (snmpClient, error) {
		if ctx != "" {
			return &fakeBridge{ctx: "nema", walks: sw}, nil
		}
		return &fakeBridge{ctx: ctx, walks: sw}, nil
	})
	if !errors.Is(err, errFDBContext) || !strings.Contains(err.Error(), "VLAN 20, 30") {
		t.Fatalf("prazan odgovor: %v", err)
	}
	// Jedan VLAN pročitan, drugi ne: radi, uz napomenu u metodu.
	res, err = readFDB(func(ctx string) (snmpClient, error) { return &fakeBridge{ctx: ctx, walks: sw}, nil })
	if err != nil || !strings.Contains(res.method, "nije pročitano: VLAN 30") {
		t.Fatalf("delimično: %+v %v", res.method, err)
	}
}

func TestAttachPorts(t *testing.T) {
	ports := []portView{{Index: 9, Name: "Gi1/0/1", Oper: "up"}, {Index: 57, Name: "Te1/1/1", Oper: "up"}, {Index: 10, Name: "Gi1/0/2", Oper: "up"}}
	fdb := map[int][]string{9: {"00:1a:2b:3c:4d:5e", "00:11:22:33:44:55"}}
	for i := 0; i < 30; i++ {
		fdb[57] = append(fdb[57], "02:00:00:00:00:"+strconv.Itoa(10+i))
	}
	fdb[57] = append(fdb[57], "00:1a:2b:3c:4d:5f")
	hosts := map[string]attachedDev{
		"00:1a:2b:3c:4d:5e": {Name: "INF-DRAGANZ", AgentID: "a1"},
		"00:1a:2b:3c:4d:5f": {Name: "INF-MILOSM", AgentID: "a2"},
	}
	nb := map[int][]neighbor{57: {{Name: "HQ_DC_S-I", Port: "Te1/1/1", IP: "10.0.35.15"}}}
	match := func(n neighbor) string {
		if n.IP == "10.0.35.15" {
			return "dev1"
		}
		return ""
	}
	out := attachPorts(ports, fdb, nb, nil, 0, hosts, match)
	if len(out[0].Attached) != 2 || out[0].Attached[1].Name != "INF-DRAGANZ" || out[0].Attached[0].Name != "" {
		t.Fatalf("pristupni port: %+v", out[0].Attached)
	}
	// Uplink: bez računara (vide se svi iza drugog switch-a), samo sused.
	if out[1].MACCount != 31 || len(out[1].Attached) != 0 || len(out[1].Neighbors) != 1 || out[1].Neighbors[0].DeviceID != "dev1" {
		t.Fatalf("uplink: %+v", out[1])
	}
	// Telefon (CDP) + računar iza njega: nepoznata MAC adresa je telefon.
	phone := attachPorts([]portView{{Index: 9, Oper: "up"}}, map[int][]string{9: {"00:1a:2b:3c:4d:5e", "00:11:22:33:44:55"}},
		map[int][]neighbor{9: {{Name: "SEP001122334455", Platform: "Cisco IP Phone 8845"}}}, nil, 0, hosts, match)
	if len(phone[0].Attached) != 1 || phone[0].Attached[0].Name != "INF-DRAGANZ" || len(phone[0].Neighbors) != 1 {
		t.Fatalf("telefon: %+v", phone[0])
	}
	if out[2].MACCount != 0 || ports[0].Attached != nil {
		t.Fatalf("prazan port / original izmenjen: %+v %+v", out[2], ports[0])
	}
}

func TestReadNeighbors(t *testing.T) {
	str := func(n, s string) gosnmp.SnmpPDU {
		return gosnmp.SnmpPDU{Name: n, Type: gosnmp.OctetString, Value: []byte(s)}
	}
	sw := map[string]map[string][]gosnmp.SnmpPDU{"": {
		oidCdpDeviceID: {str(oidCdpDeviceID+".57.1", "HQ_DC_S-I.firma.local"), str(oidCdpDeviceID+".9.3", "SEP00AB12345678")},
		oidCdpPort:     {str(oidCdpPort+".57.1", "TenGigabitEthernet1/1/1")},
		oidCdpPlatform: {str(oidCdpPlatform+".57.1", "cisco C9200L-48P-4X"), str(oidCdpPlatform+".9.3", "Cisco IP Phone 8845")},
		oidCdpAddress:  {{Name: oidCdpAddress + ".57.1", Type: gosnmp.OctetString, Value: []byte{10, 0, 35, 15}}},
		// LLDP: Lenovo na lokalnom portu 7 (= Te1/1/3) i isti sused kao CDP.
		oidLldpRemSysName: {str(oidLldpRemSysName+".0.7.1", "NE1032-Gornji"), str(oidLldpRemSysName+".0.57.2", "HQ_DC_S-I"),
			str(oidLldpRemSysName+".0.9.4", "Mitel 6930")},
		// Mitel šalje MAC adresu kao bajtove umesto naziva porta.
		oidLldpRemPortID: {str(oidLldpRemPortID+".0.7.1", "Ethernet1/48"),
			{Name: oidLldpRemPortID + ".0.9.4", Type: gosnmp.OctetString, Value: []byte{0x08, 0x00, 0x0f, 0x5d, 0x57, 0x01}}},
		oidLldpLocPortID: {str(oidLldpLocPortID+".7", "Te1/1/3")},
	}}
	c := &fakeBridge{walks: sw}
	nb := readNeighbors(c, []portSample{{Index: 9, Name: "Gi1/0/1"}, {Index: 57, Name: "Te1/1/1"}, {Index: 59, Name: "Te1/1/3"}})
	if len(nb[57]) != 1 || nb[57][0].Name != "HQ_DC_S-I" || nb[57][0].Port != "TenGigabitEthernet1/1/1" || nb[57][0].IP != "10.0.35.15" {
		t.Fatalf("CDP switch: %+v", nb[57])
	}
	if len(nb[9]) != 2 || nb[9][0].Platform != "Cisco IP Phone 8845" {
		t.Fatalf("CDP telefon: %+v", nb[9])
	}
	for _, n := range nb[9] {
		if n.Name == "Mitel 6930" && n.Port != "" {
			t.Fatalf("nečitljiv port mora da se izostavi: %q", n.Port)
		}
	}
	if len(nb[59]) != 1 || nb[59][0].Name != "NE1032-Gornji" || nb[59][0].Port != "Ethernet1/48" {
		t.Fatalf("LLDP: %+v", nb)
	}
}

// Slučaj sa slike: Cisco switch na Lenovo NE1032 – Lenovo prosleđuje CDP, pa
// port „vidi” sve Cisco switch-eve; direktno priključen je samo NE1032 (LLDP).
func TestFilterNeighborsCDPThroughLenovo(t *testing.T) {
	cdp := func(name, port string) neighbor { return neighbor{Name: name, Port: port, Proto: "cdp"} }
	nb := map[int][]neighbor{
		57: {cdp("HQ_DC_S-I", "Te1/1/2"), cdp("HQ_DC_S-I", "Te1/1/1"), cdp("HQ_KMN_S-I", "Te1/1/1"), cdp("HQ_FIN_S-II", "Te1/1/2"),
			cdp("HQ_UZ_S-I", "Te1/1/3"), {Name: "NE1032-Donji", Port: "Ethernet1/24", Proto: "lldp"}},
		58: {cdp("HQ_DC_S-I", "Te1/1/2"), cdp("HQ_LAB_S-I", "Te1/1/1")}, // bez LLDP-a
		9:  {cdp("SEP00AB12345678", "Port 1")},                          // telefon
		10: {cdp("HQ_FIN_S-II", "Gi1/0/48")},                            // sam sebe (npr. petlja)
	}
	out := filterNeighbors(nb, "HQ_FIN_S-II.firma.local")
	if len(out[57]) != 1 || out[57][0].Name != "NE1032-Donji" {
		t.Fatalf("Lenovo: %+v", out[57])
	}
	if len(out[58]) != 2 || !out[58][0].Indirect {
		t.Fatalf("bez LLDP-a: %+v", out[58])
	}
	if len(out[9]) != 1 || out[9][0].Indirect {
		t.Fatalf("telefon: %+v", out[9])
	}
	if len(out[10]) != 0 {
		t.Fatalf("sam sebe: %+v", out[10])
	}
}

func TestHostRows(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	v := deviceView{Ports: []portView{
		// pristupni port: računar šalje 5 Mb/s = switch na portu prima 5 Mb/s
		{Index: 9, Name: "Gi1/0/10", MACCount: 1, InBps: f(5e6), OutBps: f(1e6), Attached: []attachedDev{{Name: "fin-marijal", AgentID: "a1"}}},
		{Index: 10, Name: "Gi1/0/11", MACCount: 1, Attached: []attachedDev{{MAC: "00:11:22:33:44:55"}}},                        // bez agenta
		{Index: 57, Name: "Te1/1/1", MACCount: 53, Trunk: true, Attached: []attachedDev{{Name: "LAB-MARIJAD", AgentID: "a2"}}}, // uplink
	}}
	rows := hostRows(v)
	if len(rows) != 1 || rows[0].Name != "fin-marijal" || *rows[0].SendBps != 5e6 || *rows[0].RecvBps != 1e6 || rows[0].Port != "Gi1/0/10" {
		t.Fatalf("redovi: %+v", rows)
	}
}

func TestPortRates(t *testing.T) {
	m, _ := testMonitor(t, []NetDevice{{Name: "HQ_FIN_S-I", IP: "10.0.35.6", Active: true, SNMP: true}}, nil)
	dev := &fakeSNMP{inOct: 1000, outOct: 1000}
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) { return dev, nil }
	now := time.Now()
	m.now = func() time.Time { return now }
	cfg := SNMPConfig{Version: "v3", User: "netmon"}
	if r, err := m.portRates(context.Background(), cfg, m.devices[0]); err != nil || len(r) != 0 {
		t.Fatalf("prvo čitanje: %v %v", r, err)
	}
	now = now.Add(10 * time.Second)
	dev.inOct += 12_500_000 // 10 Mb/s
	r, err := m.portRates(context.Background(), cfg, m.devices[0])
	if err != nil || r[9][0] != 10e6 || r[9][1] != 0 {
		t.Fatalf("drugo čitanje: %v %v", r, err)
	}
	// Odmah ponovo: isto očitavanje (bez novog upita).
	dev.inOct += 999_999_999
	if r2, _ := m.portRates(context.Background(), cfg, m.devices[0]); r2[9][0] != 10e6 {
		t.Fatalf("keš: %v", r2)
	}
}

// Mali switch ispod stola na pristupnom portu: računari se vide iako je na
// portu više od 4 adrese; pravi trunk (status sa switch-a) ih ne prikazuje.
func TestAttachPortsSharedAccessPort(t *testing.T) {
	ports := []portView{{Index: 5, Name: "Gi1/0/5", Oper: "up"}, {Index: 57, Name: "Te1/1/1", Oper: "up"}, {Index: 6, Name: "Gi1/0/6", Oper: "up"}}
	fdb := map[int][]string{
		5:  {"38:ca:84:b5:85:a2", "02:00:00:00:00:01", "02:00:00:00:00:02", "02:00:00:00:00:03", "02:00:00:00:00:04", "02:00:00:00:00:05"},
		57: {"02:00:00:00:00:10", "02:00:00:00:00:11"},
		6:  {"02:00:00:00:01:00"},
	}
	hosts := map[string]attachedDev{"38:ca:84:b5:85:a2": {Name: "INF-DRAGANZ", AgentID: "a1"}}
	trunks := map[int]bool{5: false, 57: true, 6: false}
	out := attachPorts(ports, fdb, nil, trunks, 0, hosts, func(neighbor) string { return "" })
	p := out[0]
	if p.Trunk || !p.Shared || len(p.Attached) != 1 || p.Attached[0].Name != "INF-DRAGANZ" || p.Unknown != 5 {
		t.Fatalf("mali switch: %+v", p)
	}
	if !out[1].Trunk || len(out[1].Attached) != 0 {
		t.Fatalf("trunk sa switch-a: %+v", out[1])
	}
	if out[2].Trunk || out[2].Shared || len(out[2].Attached) != 1 {
		t.Fatalf("običan port: %+v", out[2])
	}
	rows := hostRows(deviceView{Ports: out})
	if len(rows) != 1 || rows[0].Name != "INF-DRAGANZ" || !rows[0].Shared {
		t.Fatalf("tabela računara: %+v", rows)
	}
	// Bez podatka o trunk-u (npr. Lenovo): 6 adresa bez suseda = pristupni port.
	out = attachPorts(ports, fdb, nil, nil, 57, hosts, func(neighbor) string { return "" })
	if out[0].Trunk || !out[0].Shared || !out[1].Trunk {
		t.Fatalf("rezervno pravilo: %+v / %+v", out[0], out[1])
	}
}

func TestReadTrunks(t *testing.T) {
	sw := map[string]map[string][]gosnmp.SnmpPDU{"": {
		oidVlanTrunkStatus: {intPDU(oidVlanTrunkStatus+".57", 1), intPDU(oidVlanTrunkStatus+".5", 2)},
	}}
	tr := readTrunks(&fakeBridge{walks: sw})
	if !tr[57] || tr[5] {
		t.Fatalf("trunk status: %v", tr)
	}
	if readTrunks(&fakeBridge{walks: map[string]map[string][]gosnmp.SnmpPDU{}}) != nil {
		t.Fatal("bez podatka = nil (rezervno pravilo)")
	}
}

// Računar iza malog switch-a koji nije u konzoli (kancelarija_50 na Gi1/0/38):
// prikazuje se na matičnom switch-u; ako je mali switch dodat, ne prikazuje se
// dvaput; uplink i port sa mnogo adresa se ne gledaju.
func TestBehindSmallSwitch(t *testing.T) {
	ports := []portView{{Index: 38, Name: "Gi1/0/38", Oper: "up"}, {Index: 57, Name: "Te1/1/1", Oper: "up"}, {Index: 40, Name: "Gi1/0/40", Oper: "up"}}
	many := []string{}
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("02:00:00:00:02:%02x", i))
	}
	fdb := map[int][]string{
		38: {"38:ca:84:b5:85:a2", "02:00:00:00:00:01"},
		57: {"aa:aa:aa:aa:aa:01"},
		40: append(many, "aa:aa:aa:aa:aa:02"),
	}
	hosts := map[string]attachedDev{
		"38:ca:84:b5:85:a2": {Name: "INF-PERAP", AgentID: "a1"},
		"aa:aa:aa:aa:aa:01": {Name: "NA-UPLINKU", AgentID: "a2"},
		"aa:aa:aa:aa:aa:02": {Name: "IZA-VELIKOG", AgentID: "a3"},
	}
	nb := map[int][]neighbor{38: {{Name: "kancelarija_50", Port: "Gi0/8", Proto: "CDP"}}}
	trunks := map[int]bool{38: true, 57: true, 40: true}
	out := attachPorts(ports, fdb, nb, trunks, 57, hosts, func(neighbor) string { return "" })
	if p := out[0]; len(p.Behind) != 1 || p.Behind[0].Name != "INF-PERAP" || p.Via != "kancelarija_50" {
		t.Fatalf("iza malog switch-a: %+v", p)
	}
	if len(out[1].Behind) != 0 || len(out[2].Behind) != 0 {
		t.Fatalf("uplink / mnogo adresa: %+v %+v", out[1].Behind, out[2].Behind)
	}
	rows := hostRows(deviceView{Ports: out})
	if len(rows) != 1 || !rows[0].Behind || rows[0].Via != "kancelarija_50" || rows[0].Port != "Gi1/0/38" {
		t.Fatalf("tabela računara: %+v", rows)
	}
	// Mali switch je dodat u konzolu: računar je u njegovim detaljima.
	out = attachPorts(ports, fdb, nb, trunks, 57, hosts, func(neighbor) string { return "k50" })
	if len(out[0].Behind) != 0 {
		t.Fatalf("dvostruko: %+v", out[0])
	}
}
