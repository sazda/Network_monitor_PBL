package console

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Wi-Fi klijenti sa Cisco WLC-a (AireOS, AIRESPACE-WIRELESS-MIB). Kada AP
// radi preko WLC-a, saobraćaj računara ide tunelom do kontrolera, pa switch
// ne zna na kom je AP-u računar. Konzola zato sa WLC-a čita koji je klijent
// na kom AP-u, a AP (po nazivu) nalazi među CDP/LLDP susedima switch-eva.
const (
	oidBsnAPName         = ".1.3.6.1.4.1.14179.2.2.1.1.3" // indeks: MAC AP-a (radio)
	oidBsnMobileStAPMac  = ".1.3.6.1.4.1.14179.2.1.4.1.4" // indeks: MAC klijenta
	oidBsnMobileStSSID   = ".1.3.6.1.4.1.14179.2.1.4.1.7"
	wlcType              = "wlc"
	wifiClientsReadEvery = fdbEvery
)

// wifiClient: na kom je AP-u (naziv) i na kojoj mreži Wi-Fi klijent.
type wifiClient struct {
	AP   string `json:"ap"`
	SSID string `json:"ssid,omitempty"`
	WLC  string `json:"wlc,omitempty"` // naziv WLC-a u konzoli
}

// wifiTable je poslednje čitanje jednog WLC-a.
type wifiTable struct {
	clients map[string]wifiClient // MAC klijenta (aa:bb:..) → AP
	aps     int
	at      time.Time
	err     string
}

// macFromIndex: poslednjih 6 brojeva OID-a su bajtovi MAC adrese.
func macFromIndex(oid, base string) (string, bool) {
	rest := strings.TrimPrefix(strings.TrimPrefix(oid, "."), strings.TrimPrefix(base, ".")+".")
	parts := strings.Split(rest, ".")
	if len(parts) < 6 {
		return "", false
	}
	parts = parts[len(parts)-6:]
	b := make([]string, 6)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return "", false
		}
		b[i] = fmt.Sprintf("%02x", n)
	}
	return strings.Join(b, ":"), true
}

func macFromBytes(v any) (string, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != 6 {
		return "", false
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5]), true
}

// readWLC čita AP-ove i klijente sa WLC-a.
func readWLC(c snmpClient) (map[string]wifiClient, int, error) {
	apPDUs, err := c.BulkWalkAll(oidBsnAPName)
	if err != nil {
		return nil, 0, err
	}
	apNames := map[string]string{}
	for _, p := range apPDUs {
		if mac, ok := macFromIndex(p.Name, oidBsnAPName); ok {
			if n := pduString(p); n != "" {
				apNames[mac] = n
			}
		}
	}
	if len(apNames) == 0 {
		return nil, 0, errors.New("WLC nije vratio spisak AP-ova (AIRESPACE-WIRELESS-MIB); da li je uređaj Cisco WLC (AireOS)?")
	}
	stPDUs, err := c.BulkWalkAll(oidBsnMobileStAPMac)
	if err != nil {
		return nil, len(apNames), err
	}
	ssid := map[string]string{}
	if ps, err := c.BulkWalkAll(oidBsnMobileStSSID); err == nil {
		for _, p := range ps {
			if mac, ok := macFromIndex(p.Name, oidBsnMobileStSSID); ok {
				ssid[mac] = pduString(p)
			}
		}
	}
	out := map[string]wifiClient{}
	for _, p := range stPDUs {
		mac, ok := macFromIndex(p.Name, oidBsnMobileStAPMac)
		ap, ok2 := macFromBytes(p.Value)
		if !ok || !ok2 {
			continue
		}
		name := apNames[ap]
		if name == "" {
			name = ap // AP koji nije u tabeli: bar njegova MAC adresa
		}
		out[mac] = wifiClient{AP: name, SSID: ssid[mac]}
	}
	return out, len(apNames), nil
}

// wifiClientsLocked spaja klijente sa svih WLC-ova u konzoli.
func (m *netMonitor) wifiClientsLocked() map[string]wifiClient {
	var out map[string]wifiClient
	for _, d := range m.devices {
		st := m.rt(d.ID)
		if d.Type != wlcType || st.snmp == nil || st.snmp.wifi == nil {
			continue
		}
		for mac, c := range st.snmp.wifi.clients {
			if out == nil {
				out = map[string]wifiClient{}
			}
			c.WLC = d.Name
			out[mac] = c
		}
	}
	return out
}

// apKey: naziv AP-a za poređenje (CDP ponekad dodaje domen).
func apKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, '.'); i > 0 {
		s = s[:i]
	}
	return s
}

// wifiDev je računar (agent) na Wi-Fi-ju preko AP-a na ovom portu.
type wifiDev struct {
	attachedDev
	AP   string `json:"ap"`
	SSID string `json:"ssid,omitempty"`
	WLC  string `json:"wlc,omitempty"`
}

// attachWiFi dodaje računare (agente) koji su preko WLC-a povezani na AP koji
// je CDP/LLDP sused na portu. Računar koji je na tom portu već naveden (AP
// koji saobraćaj pušta lokalno) se ne ponavlja.
func attachWiFi(ports []portView, clients map[string]wifiClient, hosts map[string]attachedDev) {
	if len(clients) == 0 {
		return
	}
	byAP := map[string][]string{}
	for mac, c := range clients {
		if _, ok := hosts[mac]; ok {
			byAP[apKey(c.AP)] = append(byAP[apKey(c.AP)], mac)
		}
	}
	if len(byAP) == 0 {
		return
	}
	for i := range ports {
		p := &ports[i]
		for _, n := range p.Neighbors {
			if !n.isPhoneOrAP() {
				continue
			}
			macs := slices.Clone(byAP[apKey(n.Name)])
			slices.Sort(macs)
			for _, mac := range macs {
				if slices.ContainsFunc(p.Attached, func(a attachedDev) bool { return a.MAC == mac }) ||
					slices.ContainsFunc(p.Behind, func(a attachedDev) bool { return a.MAC == mac }) {
					continue
				}
				a := hosts[mac]
				a.MAC = mac
				c := clients[mac]
				p.WiFi = append(p.WiFi, wifiDev{attachedDev: a, AP: n.Name, SSID: c.SSID, WLC: c.WLC})
			}
		}
	}
}

// wifiView: stanje čitanja WLC-a (detalji WLC-a).
type wifiView struct {
	Clients int         `json:"clients"`
	APs     int         `json:"aps"`
	At      time.Time   `json:"at,omitzero"`
	Error   string      `json:"error,omitempty"`
	Agents  []wifiAgent `json:"agents,omitempty"` // računari sa agentom na Wi-Fi-ju
}

type wifiAgent struct {
	Name    string `json:"name"`
	AgentID string `json:"agent_id"`
	AP      string `json:"ap"`
	SSID    string `json:"ssid,omitempty"`
}

func newWiFiView(t *wifiTable, hosts map[string]attachedDev) *wifiView {
	v := &wifiView{Clients: len(t.clients), APs: t.aps, At: t.at, Error: t.err}
	for mac, c := range t.clients {
		if a, ok := hosts[mac]; ok && a.Name != "" {
			v.Agents = append(v.Agents, wifiAgent{Name: a.Name, AgentID: a.AgentID, AP: c.AP, SSID: c.SSID})
		}
	}
	slices.SortFunc(v.Agents, func(a, b wifiAgent) int {
		if c := strings.Compare(strings.ToLower(a.AP), strings.ToLower(b.AP)); c != 0 {
			return c
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return v
}
