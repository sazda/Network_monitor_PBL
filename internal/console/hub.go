package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sazda/network-monitor/internal/protocol"
)

// AgentInfo je javni prikaz agenta u API-ju.
type AgentInfo struct {
	protocol.Hello
	RemoteAddr  string    `json:"remote_addr"`
	Online      bool      `json:"online"`
	ConnectedAt time.Time `json:"connected_at,omitzero"`
	LastSeen    time.Time `json:"last_seen"`
	ControlRTT  float64   `json:"control_rtt_ms"` // RTT WebSocket ping/pong kanala
	Location    string    `json:"location"`       // računa se pri prikazu (vidi locations.go)
	Watched     bool      `json:"watched"`        // obaveštenja kada nije na vezi (računa se pri prikazu)
	// Outdated: izdanje agenta je manje od onog koje konzola očekuje
	// (WantRelease); računa se pri prikazu.
	Outdated    bool `json:"outdated"`
	WantRelease int  `json:"want_release,omitempty"`
}

// markRelease popunjava Outdated i WantRelease.
func (a *AgentInfo) markRelease() {
	a.WantRelease = protocol.WantRelease(a.Role)
	a.Outdated = a.Release < a.WantRelease
}

// IsServer: agent za servere (stranica „Serveri”), ne agent za računare.
func (a AgentInfo) IsServer() bool { return a.Role == protocol.RoleServer }

type agentConn struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	pending  map[string]chan protocol.Envelope
	progress map[string]func(protocol.Progress) // međurezultati po ID-ju komande
	pendMu   sync.Mutex
	closed   chan struct{}
	// Saobraćaj mrežnih kartica (agent za računare, izdanje 2+), po MAC adresi.
	statsMu sync.Mutex
	stats   map[string]protocol.IfRate
	statsAt time.Time
}

// Hub vodi evidenciju o povezanim agentima i prosleđuje im komande.
type Hub struct {
	log *log.Logger

	mu     sync.RWMutex
	agents map[string]*AgentInfo
	conns  map[string]*agentConn
	onSave func(map[string]*AgentInfo)
	// onStatus se poziva (van zaključavanja) kada se agent poveže ili izgubi vezu.
	onStatus func(AgentInfo)
	// onMetrics prima stanje servera (agent za servere).
	onMetrics func(id string, m protocol.Metrics)
	// onLive prima CPU i RAM na 5 s (agent za servere, izdanje 2+).
	onLive func(id string, l protocol.Live)
}

// NewHub kreira prazan hub.
func NewHub(logger *log.Logger) *Hub {
	return &Hub{log: logger, agents: map[string]*AgentInfo{}, conns: map[string]*agentConn{}}
}

// Agents vraća spisak svih poznatih agenata, sortiran po imenu računara.
func (h *Hub) Agents() []AgentInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]AgentInfo, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname != out[j].Hostname {
			return out[i].Hostname < out[j].Hostname
		}
		return out[i].AgentID < out[j].AgentID
	})
	return out
}

// Agent vraća jednog agenta.
func (h *Hub) Agent(id string) (AgentInfo, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	a, ok := h.agents[id]
	if !ok {
		return AgentInfo{}, false
	}
	return *a, true
}

// Forget briše agenta iz evidencije (samo ako nije na vezi).
func (h *Hub) Forget(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.conns[id]; ok {
		return errors.New("agent je trenutno povezan")
	}
	delete(h.agents, id)
	h.saveLocked()
	return nil
}

// restore učitava poslednje poznato stanje agenata i vraća ID-jeve onih koji
// su bili na vezi kada je konzola zaustavljena.
func (h *Hub) restore(agents map[string]*AgentInfo) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var wasOnline []string
	for id, a := range agents {
		if a.Online {
			wasOnline = append(wasOnline, id)
		}
		a.Online = false
		h.agents[id] = a
	}
	return wasOnline
}

func (h *Hub) statusChanged(id string) {
	if h.onStatus == nil {
		return
	}
	if a, ok := h.Agent(id); ok {
		h.onStatus(a)
	}
}

func (h *Hub) saveLocked() {
	if h.onSave == nil {
		return
	}
	cp := make(map[string]*AgentInfo, len(h.agents))
	for k, v := range h.agents {
		c := *v
		cp[k] = &c
	}
	h.onSave(cp)
}

// Serve obrađuje jednu WebSocket konekciju agenta do njenog prekida.
func (h *Hub) Serve(conn *websocket.Conn, remote string) {
	defer conn.Close()
	conn.SetReadLimit(4 << 20)
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	var hello protocol.Envelope
	if err := conn.ReadJSON(&hello); err != nil || hello.Type != protocol.TypeHello || hello.Hello == nil || hello.Hello.AgentID == "" {
		h.log.Printf("agent %s: neispravna hello poruka (%v)", remote, err)
		return
	}
	id := hello.Hello.AgentID
	if len(id) > 64 {
		return
	}
	ac := &agentConn{conn: conn, pending: map[string]chan protocol.Envelope{},
		progress: map[string]func(protocol.Progress){}, closed: make(chan struct{})}

	h.mu.Lock()
	if old, ok := h.conns[id]; ok {
		old.conn.Close() // isti agent se ponovo povezao; stara veza je mrtva
	}
	h.conns[id] = ac
	now := time.Now()
	h.agents[id] = &AgentInfo{Hello: *hello.Hello, RemoteAddr: remote, Online: true, ConnectedAt: now, LastSeen: now}
	h.saveLocked()
	h.mu.Unlock()
	h.log.Printf("agent %s (%s) povezan sa %s", id, hello.Hello.Hostname, remote)
	h.statusChanged(id)

	defer func() {
		close(ac.closed)
		h.mu.Lock()
		current := h.conns[id] == ac
		if current {
			delete(h.conns, id)
			if a, ok := h.agents[id]; ok {
				a.Online = false
				a.LastSeen = time.Now()
			}
			h.saveLocked()
		}
		h.mu.Unlock()
		h.log.Printf("agent %s odvezan", id)
		if current {
			h.statusChanged(id)
		}
	}()

	// Ping petlja: održava vezu živom (NAT/proxy timeout) i meri RTT
	// kontrolnog kanala.
	var pingSent time.Time
	var pingMu sync.Mutex
	conn.SetPongHandler(func(string) error {
		pingMu.Lock()
		rtt := time.Since(pingSent)
		pingMu.Unlock()
		h.mu.Lock()
		if a, ok := h.agents[id]; ok {
			a.LastSeen = time.Now()
			a.ControlRTT = float64(rtt.Microseconds()) / 1000
		}
		h.mu.Unlock()
		return conn.SetReadDeadline(time.Now().Add(protocol.PongWait))
	})
	go func() {
		t := time.NewTicker(protocol.PingPeriod)
		defer t.Stop()
		send := func() {
			pingMu.Lock()
			pingSent = time.Now()
			pingMu.Unlock()
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				conn.Close()
			}
		}
		send()
		for {
			select {
			case <-ac.closed:
				return
			case <-t.C:
				send()
			}
		}
	}()
	conn.SetReadDeadline(time.Now().Add(protocol.PongWait))

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		if env.Type == protocol.TypeMetrics && env.Metrics != nil {
			h.mu.Lock()
			if a, ok := h.agents[id]; ok {
				a.LastSeen = time.Now()
			}
			h.mu.Unlock()
			if h.onMetrics != nil {
				h.onMetrics(id, *env.Metrics)
			}
			continue
		}
		if env.Type == protocol.TypeLive && env.Live != nil {
			h.mu.Lock()
			if a, ok := h.agents[id]; ok {
				a.LastSeen = time.Now()
			}
			h.mu.Unlock()
			if h.onLive != nil {
				h.onLive(id, *env.Live)
			}
			continue
		}
		if env.Type == protocol.TypeNetStats {
			m := make(map[string]protocol.IfRate, len(env.NetStats))
			for _, r := range env.NetStats {
				m[strings.ToLower(r.MAC)] = r
			}
			ac.statsMu.Lock()
			ac.stats, ac.statsAt = m, time.Now()
			ac.statsMu.Unlock()
			continue
		}
		if env.Type == protocol.TypeProgress && env.Progress != nil {
			ac.pendMu.Lock()
			fn := ac.progress[env.ID]
			ac.pendMu.Unlock()
			if fn != nil {
				fn(*env.Progress)
			}
			continue
		}
		if env.Type != protocol.TypeResult {
			continue
		}
		ac.pendMu.Lock()
		ch, ok := ac.pending[env.ID]
		delete(ac.pending, env.ID)
		ac.pendMu.Unlock()
		if ok {
			ch <- env
		}
	}
}

// netStatsMaxAge: starija merenja saobraćaja računara se ne prikazuju.
const netStatsMaxAge = 30 * time.Second

// NetRate vraća poslednji saobraćaj mrežne kartice računara (po MAC adresi);
// ok=false ako agent nije na vezi, ne šalje saobraćaj (starija verzija) ili
// je merenje staro.
func (h *Hub) NetRate(id, mac string) (protocol.IfRate, bool) {
	h.mu.RLock()
	ac, ok := h.conns[id]
	h.mu.RUnlock()
	if !ok {
		return protocol.IfRate{}, false
	}
	ac.statsMu.Lock()
	defer ac.statsMu.Unlock()
	r, ok := ac.stats[strings.ToLower(mac)]
	return r, ok && time.Since(ac.statsAt) < netStatsMaxAge
}

// Disconnect prekida vezu sa agentom (agent se zatim sam ponovo povezuje).
func (h *Hub) Disconnect(id string) bool {
	h.mu.RLock()
	ac, ok := h.conns[id]
	h.mu.RUnlock()
	if ok {
		ac.conn.Close()
	}
	return ok
}

// ErrOffline se vraća kada agent nije povezan.
var ErrOffline = errors.New("agent nije povezan")

// Call šalje komandu agentu i čeka rezultat. Rezultat se dekodira u out.
func (h *Hub) Call(ctx context.Context, agentID, command string, params, out any) error {
	return h.CallProgress(ctx, agentID, command, params, out, nil)
}

// CallProgress je Call uz prijem međurezultata (onProgress može biti nil).
func (h *Hub) CallProgress(ctx context.Context, agentID, command string, params, out any, onProgress func(protocol.Progress)) error {
	return h.callFull(ctx, agentID, command, params, out, onProgress, nil)
}

// Cancel traži od agenta da prekine komandu (agent vraća dotadašnji rezultat).
func (h *Hub) Cancel(agentID, cmdID string) error {
	h.mu.RLock()
	ac, ok := h.conns[agentID]
	h.mu.RUnlock()
	if !ok {
		return ErrOffline
	}
	ac.writeMu.Lock()
	defer ac.writeMu.Unlock()
	ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ac.conn.WriteJSON(protocol.Envelope{Type: protocol.TypeCancel, ID: cmdID})
}

// callFull je CallProgress uz onSent, koji dobija ID poslate komande (za Cancel).
func (h *Hub) callFull(ctx context.Context, agentID, command string, params, out any, onProgress func(protocol.Progress), onSent func(string)) error {
	h.mu.RLock()
	ac, ok := h.conns[agentID]
	h.mu.RUnlock()
	if !ok {
		return ErrOffline
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := newID()
	ch := make(chan protocol.Envelope, 1)
	ac.pendMu.Lock()
	ac.pending[id] = ch
	if onProgress != nil {
		ac.progress[id] = onProgress
	}
	ac.pendMu.Unlock()
	defer func() {
		ac.pendMu.Lock()
		delete(ac.pending, id)
		delete(ac.progress, id)
		ac.pendMu.Unlock()
	}()

	ac.writeMu.Lock()
	ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = ac.conn.WriteJSON(protocol.Envelope{Type: protocol.TypeCommand, ID: id, Command: command, Params: raw})
	ac.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("slanje komande: %w", err)
	}
	if onSent != nil {
		onSent(id)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("agent nije odgovorio na vreme: %w", ctx.Err())
	case <-ac.closed:
		return errors.New("veza sa agentom je prekinuta tokom merenja")
	case env := <-ch:
		if !env.OK {
			return errors.New(env.Error)
		}
		if out != nil {
			return json.Unmarshal(env.Data, out)
		}
		return nil
	}
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
