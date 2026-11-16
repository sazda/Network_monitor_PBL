package console

import (
	"context"
	"errors"
	"math"
	"math/big"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Tabela „Računari na ovom switch-u”: računari sa agentom na pristupnim
// portovima i njihov trenutni saobraćaj iz ugla računara (šalje = switch na
// tom portu prima, prima = switch šalje). Dok su detalji otvoreni, pretraživač
// je traži na 10 s; konzola tada čita brojače svih portova (dva upita), a
// više korisnika deli isto očitavanje.
const hostsMinGap = 8 * time.Second

type portCounters struct {
	at      time.Time
	in, out map[int]*big.Int
	rates   map[int][2]float64 // ifIndex → [ulaz b/s, izlaz b/s]
}

type hostReadings struct {
	mu sync.Mutex
	m  map[string]portCounters
}

// hostRow je jedan računar u tabeli.
type hostRow struct {
	Name    string   `json:"name"`
	AgentID string   `json:"agent_id"`
	Port    string   `json:"port"`
	Index   int      `json:"index"`
	SendBps *float64 `json:"send_bps,omitempty"` // računar šalje (ulaz na port)
	RecvBps *float64 `json:"recv_bps,omitempty"` // računar prima (izlaz sa porta)
	Shared  bool     `json:"shared,omitempty"`   // port deli više uređaja (brzina je zbir)
	Via     string   `json:"via,omitempty"`      // računar je iza manjeg switch-a (naziv, ako je poznat)
	Behind  bool     `json:"behind,omitempty"`
	// Wi-Fi preko AP-a na ovom portu (podaci sa WLC-a); brzina je saobraćaj AP-a.
	AP   string `json:"ap,omitempty"`
	SSID string `json:"ssid,omitempty"`
	WLC  string `json:"wlc,omitempty"`
	// Saobraćaj same mrežne kartice računara (meri agent, izdanje 2+);
	// prazno ako agent ne šalje. SendBps/RecvBps su saobraćaj porta.
	MAC       string   `json:"mac,omitempty"`
	PCSendBps *float64 `json:"pc_send_bps,omitempty"`
	PCRecvBps *float64 `json:"pc_recv_bps,omitempty"`
	PCNoStats bool     `json:"pc_no_stats,omitempty"` // agent je na vezi, ali ne šalje saobraćaj (stara verzija)
}

type hostsResponse struct {
	T     int64     `json:"t"`
	Live  bool      `json:"live"` // brzine iz očitavanja na 10 s (inače iz čitanja na 60 s)
	Error string    `json:"error,omitempty"`
	Hosts []hostRow `json:"hosts"`
}

// portRates čita brojače saobraćaja svih portova i vraća brzine od
// prethodnog čitanja (prazno kod prvog).
func (m *netMonitor) portRates(ctx context.Context, cfg SNMPConfig, d NetDevice) (map[int][2]float64, error) {
	now := m.now()
	m.hostData.mu.Lock()
	if pc, ok := m.hostData.m[d.ID]; ok && now.Sub(pc.at) < hostsMinGap {
		m.hostData.mu.Unlock()
		return pc.rates, nil
	}
	m.hostData.mu.Unlock()

	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	c, err := dial(ctx, cfg, d.IP)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	walk := func(oid string) (map[int]*big.Int, error) {
		vars, err := c.BulkWalkAll(oid)
		if err != nil {
			return nil, err
		}
		out := map[int]*big.Int{}
		for _, v := range vars {
			if v.Value != nil && v.Type != gosnmp.NoSuchObject && v.Type != gosnmp.NoSuchInstance {
				out[lastIndex(v.Name)] = gosnmp.ToBigInt(v.Value)
			}
		}
		return out, nil
	}
	in, err := walk(oidIfHCIn)
	if err != nil {
		return nil, err
	}
	out, err := walk(oidIfHCOut)
	if err != nil {
		return nil, err
	}
	at := m.now()
	cur := portCounters{at: at, in: in, out: out, rates: map[int][2]float64{}}
	m.hostData.mu.Lock()
	defer m.hostData.mu.Unlock()
	if m.hostData.m == nil {
		m.hostData.m = map[string]portCounters{}
	}
	if prev, ok := m.hostData.m[d.ID]; ok {
		dt := at.Sub(prev.at).Seconds()
		for idx, v := range in {
			r1, ok1 := octetRate(prev.in[idx], v, dt)
			r2, ok2 := octetRate(prev.out[idx], out[idx], dt)
			if ok1 && ok2 {
				cur.rates[idx] = [2]float64{math.Round(r1), math.Round(r2)}
			}
		}
	}
	m.hostData.m[d.ID] = cur
	return cur.rates, nil
}

// hostRows pravi spisak računara (agenata) na pristupnim portovima uređaja,
// sa brzinama iz poslednjeg SNMP čitanja (60 s).
func hostRows(v deviceView) []hostRow {
	rows := []hostRow{}
	for _, p := range v.Ports {
		for _, a := range p.Behind {
			rows = append(rows, hostRow{Name: a.Name, AgentID: a.AgentID, Port: p.Name, Index: p.Index,
				SendBps: p.InBps, RecvBps: p.OutBps, Shared: true, Behind: true, Via: p.Via, MAC: a.MAC})
		}
		for _, a := range p.WiFi {
			rows = append(rows, hostRow{Name: a.Name, AgentID: a.AgentID, Port: p.Name, Index: p.Index,
				SendBps: p.InBps, RecvBps: p.OutBps, Shared: true, AP: a.AP, SSID: a.SSID, WLC: a.WLC, MAC: a.MAC})
		}
		if p.MACCount == 0 || p.Trunk {
			continue // prazan port ili uplink/trunk
		}
		for _, a := range p.Attached {
			if a.Name == "" {
				continue
			}
			rows = append(rows, hostRow{Name: a.Name, AgentID: a.AgentID, Port: p.Name, Index: p.Index, SendBps: p.InBps, RecvBps: p.OutBps,
				Shared: p.Shared, MAC: a.MAC})
		}
	}
	return rows
}

func (s *Server) handleNetdevHosts(w http.ResponseWriter, r *http.Request) {
	v, ok := s.net.device(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	resp := hostsResponse{T: time.Now().Unix(), Hosts: hostRows(v)}
	for i := range resp.Hosts {
		h := &resp.Hosts[i]
		if r, ok := s.hub.NetRate(h.AgentID, h.MAC); ok {
			h.PCSendBps, h.PCRecvBps = ptr(math.Round(r.Send)), ptr(math.Round(r.Recv))
		} else if a, ok := s.hub.Agent(h.AgentID); ok && a.Online && a.Release < 2 {
			h.PCNoStats = true
		}
	}
	cfg := s.net.snmpConfig()
	if cfg.configured() && v.SNMP && v.Active && len(resp.Hosts) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		rates, err := s.net.portRates(ctx, cfg.forDevice(v.SNMPProfile), v.NetDevice)
		switch {
		case err != nil:
			resp.Error = snmpErrorText(err)
		case len(rates) > 0:
			resp.Live = true
			for i := range resp.Hosts {
				if rt, ok := rates[resp.Hosts[i].Index]; ok {
					resp.Hosts[i].SendBps, resp.Hosts[i].RecvBps = ptr(rt[0]), ptr(rt[1])
				}
			}
		}
	}
	// Redosled po brzini računara (meri agent); bez nje po saobraćaju porta.
	val := func(h hostRow) float64 {
		s, r := h.SendBps, h.RecvBps
		if h.PCSendBps != nil {
			s, r = h.PCSendBps, h.PCRecvBps
		}
		var a, b float64
		if s != nil {
			a = *s
		}
		if r != nil {
			b = *r
		}
		return math.Max(a, b)
	}
	sort.SliceStable(resp.Hosts, func(i, j int) bool {
		if vi, vj := val(resp.Hosts[i]), val(resp.Hosts[j]); vi != vj {
			return vi > vj
		}
		return resp.Hosts[i].Name < resp.Hosts[j].Name
	})
	writeJSON(w, http.StatusOK, resp)
}
