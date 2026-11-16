package console

import (
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosnmp/gosnmp"
)

// Susedi na portovima iz CDP-a (Cisco: switch-evi, ruteri, AP-ovi, IP
// telefoni) i LLDP-a (standard, npr. Lenovo). Uređaj sam objavljuje svoje ime
// i port, pa je podatak pouzdan.
const (
	oidCdpAddress  = ".1.3.6.1.4.1.9.9.23.1.2.1.1.4"
	oidCdpDeviceID = ".1.3.6.1.4.1.9.9.23.1.2.1.1.6"
	oidCdpPort     = ".1.3.6.1.4.1.9.9.23.1.2.1.1.7"
	oidCdpPlatform = ".1.3.6.1.4.1.9.9.23.1.2.1.1.8"

	oidLldpRemPortID  = ".1.0.8802.1.1.2.1.4.1.1.7"
	oidLldpRemPortDsc = ".1.0.8802.1.1.2.1.4.1.1.8"
	oidLldpRemSysName = ".1.0.8802.1.1.2.1.4.1.1.9"
	oidLldpRemSysDesc = ".1.0.8802.1.1.2.1.4.1.1.10"
	oidLldpLocPortID  = ".1.0.8802.1.1.2.1.3.7.1.3"
	oidLldpLocPortDsc = ".1.0.8802.1.1.2.1.3.7.1.4"
)

// neighbor je sused na portu.
type neighbor struct {
	Name     string `json:"name"`               // ime uređaja (bez domena)
	Port     string `json:"port,omitempty"`     // njegov port
	Platform string `json:"platform,omitempty"` // model / opis
	IP       string `json:"ip,omitempty"`
	DeviceID string `json:"device_id,omitempty"` // ako je uređaj u konzoli (link na detalje)
	Proto    string `json:"proto"`               // cdp ili lldp
	// Indirect: više CDP suseda na portu bez LLDP suseda – CDP stiže preko
	// uređaja koji ga ne razume (prosleđuje ga dalje), pa nijedan nije
	// direktno priključen.
	Indirect bool `json:"indirect,omitempty"`
}

// shortName skida domen sa imena (HQ_DC_S-I.firma.local → HQ_DC_S-I); IP adrese
// i MAC adrese ostaju kakve jesu.
func shortName(s string) string {
	s = strings.TrimSpace(s)
	if _, err := netip.ParseAddr(s); err == nil {
		return s
	}
	if i := strings.IndexByte(s, '.'); i > 0 {
		return s[:i]
	}
	return s
}

// indexParts vraća poslednja dva broja OID-a.
func indexParts(oid string) (int, int) {
	parts := strings.Split(oid, ".")
	if len(parts) < 2 {
		return 0, 0
	}
	a, _ := strconv.Atoi(parts[len(parts)-2])
	b, _ := strconv.Atoi(parts[len(parts)-1])
	return a, b
}

type nbKey struct{ port, idx int }

// readCDP vraća susede po ifIndex-u (CDP indeks: ifIndex.redni broj).
func readCDP(c snmpClient) map[int][]neighbor {
	out := map[int][]neighbor{}
	ids, err := c.BulkWalkAll(oidCdpDeviceID)
	if err != nil || len(ids) == 0 {
		return out
	}
	nb := map[nbKey]*neighbor{}
	var order []nbKey
	for _, v := range ids {
		p, i := indexParts(v.Name)
		k := nbKey{p, i}
		nb[k] = &neighbor{Name: shortName(pduString(v)), Proto: "cdp"}
		order = append(order, k)
	}
	fill := func(oid string, set func(n *neighbor, v gosnmp.SnmpPDU)) {
		vars, err := c.BulkWalkAll(oid)
		if err != nil {
			return
		}
		for _, v := range vars {
			p, i := indexParts(v.Name)
			if n := nb[nbKey{p, i}]; n != nil {
				set(n, v)
			}
		}
	}
	fill(oidCdpPort, func(n *neighbor, v gosnmp.SnmpPDU) {
		if s := pduString(v); printable(s) {
			n.Port = s
		}
	})
	fill(oidCdpPlatform, func(n *neighbor, v gosnmp.SnmpPDU) { n.Platform = pduString(v) })
	fill(oidCdpAddress, func(n *neighbor, v gosnmp.SnmpPDU) {
		if b, ok := v.Value.([]byte); ok && len(b) == 4 {
			n.IP = netip.AddrFrom4([4]byte(b)).String()
		}
	})
	for _, k := range order {
		if n := nb[k]; n.Name != "" {
			out[k.port] = append(out[k.port], *n)
		}
	}
	return out
}

// readLLDP vraća susede po ifIndex-u. LLDP broji portove svojim brojem
// (lldpLocPortNum), pa se on povezuje sa portom po nazivu (lldpLocPortId ili
// lldpLocPortDesc = ifName/ifDescr), a inače se pretpostavlja da je jednak
// ifIndex-u.
// printable kaže da li je tekst čitljiv (bez kontrolnih znakova i neispravnog UTF-8).
func printable(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func readLLDP(c snmpClient, ports []portSample) map[int][]neighbor {
	out := map[int][]neighbor{}
	names, err := c.BulkWalkAll(oidLldpRemSysName)
	if err != nil || len(names) == 0 {
		return out
	}
	byName := map[string]int{}
	known := map[int]bool{}
	for _, p := range ports {
		known[p.Index] = true
		for _, n := range []string{p.Name, p.Descr} {
			if n != "" {
				byName[strings.ToLower(n)] = p.Index
			}
		}
	}
	locToIf := map[int]int{}
	for _, col := range []string{oidLldpLocPortID, oidLldpLocPortDsc} {
		if vars, err := c.BulkWalkAll(col); err == nil {
			for _, v := range vars {
				if idx, ok := byName[strings.ToLower(pduString(v))]; ok {
					locToIf[lastIndex(v.Name)] = idx
				}
			}
		}
	}
	// Port suseda: ID porta, a ako nije čitljiv tekst (npr. Mitel telefon
	// šalje MAC adresu kao bajtove) opis porta; inače se ne prikazuje.
	remPort := map[nbKey]string{}
	for _, col := range []string{oidLldpRemPortDsc, oidLldpRemPortID} {
		if vars, err := c.BulkWalkAll(col); err == nil {
			for _, v := range vars {
				p, i := indexParts(v.Name)
				if s := pduString(v); printable(s) {
					remPort[nbKey{p, i}] = s
				}
			}
		}
	}
	remDesc := map[nbKey]string{}
	if vars, err := c.BulkWalkAll(oidLldpRemSysDesc); err == nil {
		for _, v := range vars {
			p, i := indexParts(v.Name)
			remDesc[nbKey{p, i}] = pduString(v)
		}
	}
	for _, v := range names {
		loc, i := indexParts(v.Name)
		name := shortName(pduString(v))
		if name == "" {
			continue
		}
		ifIndex, ok := locToIf[loc]
		if !ok && known[loc] {
			ifIndex, ok = loc, true
		}
		if !ok {
			continue
		}
		desc := remDesc[nbKey{loc, i}]
		if len(desc) > 80 {
			desc = desc[:80]
		}
		out[ifIndex] = append(out[ifIndex], neighbor{Name: name, Port: remPort[nbKey{loc, i}], Platform: desc, Proto: "lldp"})
	}
	return out
}

// filterNeighbors uklanja sam uređaj (self = njegov sysName) i rešava
// slučaj kada CDP stiže preko uređaja koji ga ne razume (npr. Lenovo): takav
// uređaj CDP poruke prosleđuje dalje, pa port „vidi” sve Cisco switch-eve iza
// njega. Ako port ima više CDP suseda i LLDP suseda, ostaje samo LLDP sused
// (on je direktno priključen); bez LLDP-a se CDP susedi označavaju kao
// posredni.
func filterNeighbors(nb map[int][]neighbor, self string) map[int][]neighbor {
	self = strings.ToLower(shortName(self))
	out := make(map[int][]neighbor, len(nb))
	for idx, list := range nb {
		var cdp, lldp []neighbor
		for _, n := range list {
			if self != "" && strings.ToLower(n.Name) == self {
				continue
			}
			if n.Proto == "lldp" {
				lldp = append(lldp, n)
			} else {
				cdp = append(cdp, n)
			}
		}
		switch {
		case len(cdp) > 1 && len(lldp) > 0:
			out[idx] = lldp
		case len(cdp) > 1:
			for i := range cdp {
				cdp[i].Indirect = true
			}
			out[idx] = cdp
		case len(cdp)+len(lldp) > 0:
			out[idx] = append(cdp, lldp...)
		}
	}
	return out
}

// oidVlanTrunkStatus: CISCO-VTP-MIB vlanTrunkPortDynamicStatus (po ifIndex-u):
// 1 = port radi kao trunk, 2 = pristupni port.
const oidVlanTrunkStatus = ".1.3.6.1.4.1.9.9.46.1.6.1.1.14"

// readTrunks vraća koji portovi rade kao trunk (nil ako uređaj to ne javlja).
func readTrunks(c snmpClient) map[int]bool {
	vars, err := c.BulkWalkAll(oidVlanTrunkStatus)
	if err != nil || len(vars) == 0 {
		return nil
	}
	out := map[int]bool{}
	for _, v := range vars {
		out[lastIndex(v.Name)] = pduInt(v) == 1
	}
	return out
}

// isPhoneOrAP: sused koji nije mrežna oprema (telefon, pristupna tačka).
func (n neighbor) isPhoneOrAP() bool {
	p := strings.ToLower(n.Platform)
	return strings.Contains(p, "phone") || strings.Contains(p, "air-") || strings.Contains(p, "access point") ||
		strings.Contains(p, "c91") && strings.Contains(p, "ax")
}

// readNeighbors spaja CDP i LLDP (isti sused preko oba protokola samo jednom).
func readNeighbors(c snmpClient, ports []portSample) map[int][]neighbor {
	out := readCDP(c)
	for idx, list := range readLLDP(c, ports) {
		for _, n := range list {
			dup := false
			for _, x := range out[idx] {
				if strings.EqualFold(x.Name, n.Name) {
					dup = true
					break
				}
			}
			if !dup {
				out[idx] = append(out[idx], n)
			}
		}
	}
	return out
}
