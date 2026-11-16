package console

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Result je jedan sačuvan rezultat merenja.
type Result struct {
	ID       string          `json:"id"`
	Time     time.Time       `json:"time"`
	Started  time.Time       `json:"started,omitzero"` // početak merenja (za ping)
	AgentID  string          `json:"agent_id"`
	Hostname string          `json:"hostname"`
	Kind     string          `json:"kind"` // latency | speedtest | lan
	PeerID   string          `json:"peer_id,omitempty"`
	PeerHost string          `json:"peer_hostname,omitempty"` // naziv računara PeerID
	User     string          `json:"user,omitempty"`          // ko je pokrenuo merenje
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// Store čuva rezultate u memoriji (poslednjih maxKeep) i dopisuje ih u
// JSON-lines fajl, tako da se istorija može lako obraditi i spoljnim alatima.
type Store struct {
	dir     string
	maxKeep int

	mu      sync.Mutex
	results []Result
	agentMu sync.Mutex
}

// OpenStore otvara (ili kreira) direktorijum sa podacima.
func OpenStore(dir string, maxKeep int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, maxKeep: maxKeep}
	f, err := os.Open(filepath.Join(dir, "results.jsonl"))
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var r Result
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				s.results = append(s.results, r)
				if len(s.results) > 2*maxKeep {
					s.results = append([]Result(nil), s.results[len(s.results)-maxKeep:]...)
				}
			}
		}
		if len(s.results) > maxKeep {
			s.results = s.results[len(s.results)-maxKeep:]
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// Add dodaje rezultat.
func (s *Store) Add(r Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	if len(s.results) > s.maxKeep {
		s.results = s.results[len(s.results)-s.maxKeep:]
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "results.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, err = f.Write(append(b, '\n'))
	return err
}

// Get vraća rezultat po ID-ju (među rezultatima u memoriji).
func (s *Store) Get(id string) (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.results) - 1; i >= 0; i-- {
		if s.results[i].ID == id {
			return s.results[i], true
		}
	}
	return Result{}, false
}

// List vraća rezultate (najnoviji prvi), opciono filtrirane po agentu.
func (s *Store) List(agentID string, limit int) []Result {
	out, _ := s.Query(ResultQuery{AgentID: agentID, Limit: limit})
	return out
}

// ResultQuery opisuje pretragu istorije merenja.
type ResultQuery struct {
	AgentID string // samo merenja ovog agenta (i LAN testovi u kojima učestvuje)
	Search  string // reči koje sve moraju da se nađu u nazivu računara (ili ID-u agenta)
	Offset  int
	Limit   int
	// HostName daje trenutni naziv računara za ID agenta (za starije
	// rezultate u kojima naziv drugog računara nije sačuvan); može biti nil.
	HostName func(id string) string
}

// Query vraća jednu stranu rezultata (najnoviji prvi) i ukupan broj pogodaka.
func (s *Store) Query(q ResultQuery) ([]Result, int) {
	words := strings.Fields(strings.ToLower(q.Search))
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Result{}
	total := 0
	for i := len(s.results) - 1; i >= 0; i-- {
		r := s.results[i]
		if q.AgentID != "" && r.AgentID != q.AgentID && r.PeerID != q.AgentID {
			continue
		}
		if len(words) > 0 && !resultMatches(r, words, q.HostName) {
			continue
		}
		if total >= q.Offset && len(out) < q.Limit {
			out = append(out, r)
		}
		total++
	}
	return out, total
}

func resultMatches(r Result, words []string, hostName func(string) string) bool {
	hay := []string{r.Hostname, r.AgentID, r.PeerID, r.PeerHost}
	if hostName != nil {
		hay = append(hay, hostName(r.AgentID))
		if r.PeerID != "" {
			hay = append(hay, hostName(r.PeerID))
		}
	}
	text := strings.ToLower(strings.Join(hay, " "))
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// LoadAgents učitava poslednje poznato stanje agenata.
func (s *Store) LoadAgents() map[string]*AgentInfo {
	m := map[string]*AgentInfo{}
	b, err := os.ReadFile(filepath.Join(s.dir, "agents.json"))
	if err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// SaveAgents snima stanje agenata (atomski, preko privremenog fajla).
func (s *Store) SaveAgents(m map[string]*AgentInfo) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(s.dir, "agents.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(s.dir, "agents.json"))
	}
}
