package console

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Tabela MAC adresa switch-a (koji uređaj je na kom portu). Čita se na tri
// načina, redom:
//  1. Q-BRIDGE-MIB (standard, svi VLAN-ovi odjednom);
//  2. BRIDGE-MIB bez konteksta (kod Cisco-a samo VLAN 1);
//  3. BRIDGE-MIB po VLAN-u (Cisco: SNMPv3 kontekst „vlan-N”, odnosno
//     community@N za v2c) – VLAN-ovi iz CISCO-VTP-MIB.
const (
	oidQFdbPort     = ".1.3.6.1.2.1.17.7.1.2.2.1.2"
	oidQFdbStatus   = ".1.3.6.1.2.1.17.7.1.2.2.1.3"
	oidFdbPort      = ".1.3.6.1.2.1.17.4.3.1.2"
	oidFdbStatus    = ".1.3.6.1.2.1.17.4.3.1.3"
	oidBasePortIf   = ".1.3.6.1.2.1.17.1.4.1.2"
	oidVtpVlanState = ".1.3.6.1.4.1.9.9.46.1.3.1.1.2"

	fdbLearned = 3 // dot1dTpFdbStatus: learned (ostalo: sopstvene, statičke...)
)

// fdbResult je MAC tabela: ifIndex → MAC adrese (aa:bb:cc:dd:ee:ff).
type fdbResult struct {
	ports  map[int][]string
	method string
}

// macFromOID vraća MAC adresu iz poslednjih 6 brojeva OID-a.
func macFromOID(oid string) (string, bool) {
	parts := strings.Split(oid, ".")
	if len(parts) < 6 {
		return "", false
	}
	b := make([]string, 6)
	for i, p := range parts[len(parts)-6:] {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return "", false
		}
		b[i] = fmt.Sprintf("%02x", n)
	}
	return strings.Join(b, ":"), true
}

// fdbKey je deo OID-a posle kolone (za spajanje porta i statusa).
func fdbKey(oid, col string) string { return strings.TrimPrefix(oid, col+".") }

// walkFDB čita kolone port/status i vraća bridge port → MAC adrese (samo
// naučene adrese).
func walkFDB(c snmpClient, portCol, statusCol string) (map[int][]string, error) {
	ports, err := c.BulkWalkAll(portCol)
	if err != nil {
		return nil, err
	}
	status := map[string]int64{}
	if st, err := c.BulkWalkAll(statusCol); err == nil {
		for _, v := range st {
			status[fdbKey(v.Name, statusCol)] = pduInt(v)
		}
	}
	out := map[int][]string{}
	for _, v := range ports {
		key := fdbKey(v.Name, portCol)
		if s, ok := status[key]; ok && s != fdbLearned {
			continue
		}
		bp := int(pduInt(v))
		mac, ok := macFromOID(v.Name)
		if bp <= 0 || !ok {
			continue
		}
		out[bp] = append(out[bp], mac)
	}
	return out, nil
}

func basePorts(c snmpClient) map[int]int {
	out := map[int]int{}
	vars, err := c.BulkWalkAll(oidBasePortIf)
	if err != nil {
		return out
	}
	for _, v := range vars {
		out[lastIndex(v.Name)] = int(pduInt(v))
	}
	return out
}

// addFDB dodaje MAC adrese (bridge port → ifIndex) u rezultat, bez duplikata.
func addFDB(dst map[int][]string, byBridge map[int][]string, base map[int]int) int {
	n := 0
	for bp, macs := range byBridge {
		ifIndex, ok := base[bp]
		if !ok {
			continue
		}
		for _, m := range macs {
			if !containsStr(dst[ifIndex], m) {
				dst[ifIndex] = append(dst[ifIndex], m)
				n++
			}
		}
	}
	return n
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var errFDBContext = errors.New("MAC tabela po VLAN-ovima nije dostupna; na switch-u grupi NETMON verovatno treba dozvoliti " +
	"VLAN kontekste: snmp-server group NETMON v3 priv context vlan- match prefix read NETMON-RO access NETMON-SNMP")

// readFDB čita tabelu MAC adresa. dial(context) otvara sesiju u zadatom
// SNMP kontekstu ("" = podrazumevani, "vlan-N" = VLAN N).
func readFDB(dial func(context string) (snmpClient, error)) (fdbResult, error) {
	res := fdbResult{ports: map[int][]string{}}
	c, err := dial("")
	if err != nil {
		return res, err
	}
	defer c.Close()
	base := basePorts(c)

	// 1. Q-BRIDGE-MIB.
	if q, err := walkFDB(c, oidQFdbPort, oidQFdbStatus); err == nil && len(q) > 0 {
		if addFDB(res.ports, q, base) > 0 {
			res.method = "Q-BRIDGE-MIB"
			return res, nil
		}
	}
	// 2. BRIDGE-MIB bez konteksta.
	if b, err := walkFDB(c, oidFdbPort, oidFdbStatus); err == nil {
		addFDB(res.ports, b, base)
	}
	// 3. Cisco: BRIDGE-MIB po VLAN-u.
	var vlans []int
	if vs, err := c.BulkWalkAll(oidVtpVlanState); err == nil {
		for _, v := range vs {
			id := lastIndex(v.Name)
			if pduInt(v) == 1 && id > 1 && (id < 1002 || id > 1005) {
				vlans = append(vlans, id)
			}
		}
	}
	sort.Ints(vlans)
	if len(vlans) == 0 {
		if len(res.ports) == 0 {
			return res, errors.New("switch nije vratio tabelu MAC adresa (ni Q-BRIDGE-MIB ni BRIDGE-MIB)")
		}
		res.method = "BRIDGE-MIB"
		return res, nil
	}
	// VLAN je „pročitan” ako je vratio bar spisak portova (BRIDGE-MIB baza);
	// kada switch ne dozvoli kontekst „vlan-N”, Cisco često vrati prazan
	// odgovor umesto greške, pa se i to računa kao nepročitan VLAN.
	okVlans := 0
	var missed []string
	var lastErr error
	for _, vlan := range vlans {
		vc, err := dial("vlan-" + strconv.Itoa(vlan))
		if err != nil {
			missed = append(missed, strconv.Itoa(vlan))
			lastErr = err
			continue
		}
		b, err := walkFDB(vc, oidFdbPort, oidFdbStatus)
		vbase := basePorts(vc)
		vc.Close()
		if err != nil {
			missed = append(missed, strconv.Itoa(vlan))
			lastErr = err
			continue
		}
		if len(b) == 0 && len(vbase) == 0 {
			missed = append(missed, strconv.Itoa(vlan))
			continue
		}
		okVlans++
		addFDB(res.ports, b, vbase)
	}
	if okVlans == 0 {
		detail := "VLAN " + strings.Join(missed, ", ") + " bez odgovora"
		if lastErr != nil {
			detail += "; " + lastErr.Error()
		}
		if len(res.ports) > 0 {
			res.method = "BRIDGE-MIB (samo VLAN 1)"
		}
		return res, fmt.Errorf("%w (%s)", errFDBContext, detail)
	}
	res.method = fmt.Sprintf("BRIDGE-MIB po VLAN-ovima (%d)", okVlans)
	if len(missed) > 0 {
		res.method += ", nije pročitano: VLAN " + strings.Join(missed, ", ")
	}
	return res, nil
}
