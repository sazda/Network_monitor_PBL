package console

import (
	"testing"

	"github.com/gosnmp/gosnmp"
)

// fakeWLC vraća tabele AIRESPACE-WIRELESS-MIB.
type fakeWLC struct{}

func (fakeWLC) Get([]string) ([]gosnmp.SnmpPDU, error) { return nil, nil }
func (fakeWLC) Close()                                 {}
func (fakeWLC) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	ap1 := ".0.58.153.1.2.3" // 00:3a:99:01:02:03
	switch oid {
	case oidBsnAPName:
		return []gosnmp.SnmpPDU{
			{Name: oid + ap1, Type: gosnmp.OctetString, Value: []byte("AP-HQ-PRIZEMLJE")},
			{Name: oid + ".0.58.153.1.2.4", Type: gosnmp.OctetString, Value: []byte("AP-HQ-SPRAT1")},
		}, nil
	case oidBsnMobileStAPMac:
		return []gosnmp.SnmpPDU{
			{Name: oid + ".56.202.132.181.133.170", Type: gosnmp.OctetString, Value: []byte{0x00, 0x3a, 0x99, 1, 2, 3}}, // 38:ca:84:b5:85:aa
			{Name: oid + ".1.2.3.4.5.6", Type: gosnmp.OctetString, Value: []byte{0x00, 0x3a, 0x99, 1, 2, 4}},            // telefon bez agenta
		}, nil
	case oidBsnMobileStSSID:
		return []gosnmp.SnmpPDU{{Name: oid + ".56.202.132.181.133.170", Type: gosnmp.OctetString, Value: []byte("FIRMA")}}, nil
	}
	return nil, nil
}

func TestReadWLCAndAttach(t *testing.T) {
	clients, aps, err := readWLC(fakeWLC{})
	if err != nil || aps != 2 || len(clients) != 2 {
		t.Fatalf("WLC: %v %d %+v", err, aps, clients)
	}
	if c := clients["38:ca:84:b5:85:aa"]; c.AP != "AP-HQ-PRIZEMLJE" || c.SSID != "FIRMA" {
		t.Fatalf("klijent: %+v", c)
	}
	hosts := map[string]attachedDev{"38:ca:84:b5:85:aa": {Name: "inf-peras", AgentID: "a1"}}
	ports := []portView{
		{Index: 12, Name: "Gi1/0/12", Oper: "up", Neighbors: []neighbor{{Name: "AP-HQ-PRIZEMLJE.firma.local", Platform: "cisco AIR-AP2802I-E-K9"}}},
		{Index: 13, Name: "Gi1/0/13", Oper: "up", Neighbors: []neighbor{{Name: "AP-HQ-SPRAT1", Platform: "cisco AIR-AP2802I-E-K9"}}},
		{Index: 49, Name: "Te1/1/1", Oper: "up", Neighbors: []neighbor{{Name: "HQ_DC_S-I", Platform: "cisco C9300"}}},
	}
	attachWiFi(ports, clients, hosts)
	if len(ports[0].WiFi) != 1 || ports[0].WiFi[0].Name != "inf-peras" || ports[0].WiFi[0].SSID != "FIRMA" {
		t.Fatalf("port AP-a: %+v", ports[0].WiFi)
	}
	if len(ports[1].WiFi) != 0 || len(ports[2].WiFi) != 0 {
		t.Fatalf("samo računari sa agentom, samo na portu AP-a: %+v %+v", ports[1].WiFi, ports[2].WiFi)
	}
	rows := hostRows(deviceView{Ports: ports})
	if len(rows) != 1 || rows[0].Name != "inf-peras" || rows[0].AP != "AP-HQ-PRIZEMLJE.firma.local" || !rows[0].Shared {
		t.Fatalf("spisak računara: %+v", rows)
	}
	// Računar koji je na portu već naveden (AP pušta saobraćaj lokalno) se ne ponavlja.
	ports[0].WiFi = nil
	ports[0].Behind = []attachedDev{{MAC: "38:ca:84:b5:85:aa", Name: "inf-peras"}}
	attachWiFi(ports, clients, hosts)
	if len(ports[0].WiFi) != 0 {
		t.Fatal("dupli prikaz")
	}
	v := newWiFiView(&wifiTable{clients: clients, aps: aps}, hosts)
	if v.Clients != 2 || v.APs != 2 || len(v.Agents) != 1 || v.Agents[0].AP != "AP-HQ-PRIZEMLJE" {
		t.Fatalf("detalji WLC-a: %+v", v)
	}
}

func TestReadWLCNotAireOS(t *testing.T) {
	if _, _, err := readWLC(&fakeSNMP{}); err == nil {
		t.Fatal("switch nije WLC")
	}
}
