package console

import (
	"context"
	"errors"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Režim uživo: dok su detalji uređaja otvoreni, pretraživač na svakih 5 s
// traži trenutni saobraćaj uplink porta. Konzola tada čita samo dva brojača
// tog porta; ako je poslednje čitanje novije od liveMinGap (npr. isti uređaj
// gleda više korisnika), vraća se ono, pa switch ne dobija više upita.
const liveMinGap = 4 * time.Second

type portLiveSample struct {
	at      time.Time
	idx     int
	in, out *big.Int
	inBps   *float64
	outBps  *float64
}

type liveReadings struct {
	mu sync.Mutex
	m  map[string]portLiveSample
}

// liveResponse je jedno očitavanje uživo.
type liveResponse struct {
	T      int64    `json:"t"`
	Port   int      `json:"port"`
	InBps  *float64 `json:"in_bps,omitempty"` // nil kod prvog očitavanja
	OutBps *float64 `json:"out_bps,omitempty"`
}

func (l *liveReadings) get(id string) (portLiveSample, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.m[id]
	return s, ok
}

// liveRate čita brojače porta idx i vraća brzinu od prethodnog očitavanja.
func (m *netMonitor) liveRate(ctx context.Context, cfg SNMPConfig, d NetDevice, idx int) (liveResponse, error) {
	now := m.now()
	if s, ok := m.liveData.get(d.ID); ok && s.idx == idx && now.Sub(s.at) < liveMinGap {
		return liveResponse{T: s.at.Unix(), Port: idx, InBps: s.inBps, OutBps: s.outBps}, nil
	}
	m.mu.Lock()
	dial := m.dialSNMP
	m.mu.Unlock()
	c, err := dial(ctx, cfg, d.IP)
	if err != nil {
		return liveResponse{}, err
	}
	defer c.Close()
	suffix := "." + strconv.Itoa(idx)
	vars, err := c.Get([]string{oidIfHCIn + suffix, oidIfHCOut + suffix})
	if err != nil {
		return liveResponse{}, err
	}
	var in, out *big.Int
	for _, v := range vars {
		if v.Value == nil || v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance {
			continue
		}
		switch v.Name {
		case oidIfHCIn + suffix:
			in = gosnmp.ToBigInt(v.Value)
		case oidIfHCOut + suffix:
			out = gosnmp.ToBigInt(v.Value)
		}
	}
	if in == nil || out == nil {
		return liveResponse{}, errors.New("uređaj nije vratio brojače saobraćaja za taj port")
	}
	at := m.now()
	cur := portLiveSample{at: at, idx: idx, in: in, out: out}
	m.liveData.mu.Lock()
	if m.liveData.m == nil {
		m.liveData.m = map[string]portLiveSample{}
	}
	if prev, ok := m.liveData.m[d.ID]; ok && prev.idx == idx {
		dt := at.Sub(prev.at).Seconds()
		r1, ok1 := octetRate(prev.in, in, dt)
		r2, ok2 := octetRate(prev.out, out, dt)
		if ok1 && ok2 {
			cur.inBps, cur.outBps = ptr(math.Round(r1)), ptr(math.Round(r2))
		}
	}
	m.liveData.m[d.ID] = cur
	m.liveData.mu.Unlock()
	return liveResponse{T: at.Unix(), Port: idx, InBps: cur.inBps, OutBps: cur.outBps}, nil
}

// handleNetdevLive: trenutni saobraćaj porta (?port=<ifIndex>) za režim uživo.
func (s *Server) handleNetdevLive(w http.ResponseWriter, r *http.Request) {
	d, ok := s.net.find(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	idx, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || idx <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("neispravan port"))
		return
	}
	cfg := s.net.snmpConfig()
	if !cfg.configured() || !d.SNMP || !d.Active {
		writeErr(w, http.StatusConflict, errors.New("SNMP čitanje nije uključeno za ovaj uređaj"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := s.net.liveRate(ctx, cfg.forDevice(d.SNMPProfile), d, idx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, errors.New(snmpErrorText(err)))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleNetdevUplink bira uplink port uređaja ({"port": "Te1/1/1"}; prazno =
// automatski).
func (s *Server) handleNetdevUplink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port string `json:"port"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	id := r.PathValue("id")
	list := s.net.list()
	i := slices.IndexFunc(list, func(x NetDevice) bool { return x.ID == id })
	if i < 0 {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	list[i].Uplink = clean(req.Port, 64)
	if err := s.net.setDevices(list); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("mrežna oprema: uplink za %s = %q – %s", list[i].Name, list[i].Uplink, identityFrom(r).Name)
	v, _ := s.net.device(id)
	writeJSON(w, http.StatusOK, v)
}
