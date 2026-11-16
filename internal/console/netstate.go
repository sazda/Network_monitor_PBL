package console

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"time"
)

// Stanje padova se čuva na disku (netdev-state.json): posle restarta konzole
// (npr. instalacija nove verzije) zna se koji uređaji i lokacije nisu radili
// i da li je za njih poslato obaveštenje, pa se povratak normalno javlja.

type downRecord struct {
	Since   time.Time `json:"since"`
	Alerted bool      `json:"alerted,omitempty"`
}

type locRecord struct {
	Status string    `json:"status"`
	Since  time.Time `json:"since"`
}

type netPersist struct {
	Down      map[string]downRecord `json:"down,omitempty"`
	Locations map[string]locRecord  `json:"locations,omitempty"`
}

func (m *netMonitor) loadStateLocked() {
	if m.statePath == "" {
		return
	}
	b, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var p netPersist
	if err := json.Unmarshal(b, &p); err != nil {
		m.log.Printf("stanje mrežne opreme: %v", err)
		return
	}
	known := map[string]bool{}
	for _, d := range m.devices {
		known[d.ID] = true
	}
	for id, r := range p.Down {
		if known[id] {
			m.state[id] = &devRuntime{status: stDown, since: r.Since, alerted: r.Alerted, misses: downAfter, rtt: math.NaN()}
		}
	}
	for name, r := range p.Locations {
		m.locs[name] = &locRuntime{status: r.Status, since: r.Since}
	}
	m.stateSaved = b
}

// saveStateLocked upisuje stanje samo kada se promenilo.
func (m *netMonitor) saveStateLocked() {
	if m.statePath == "" {
		return
	}
	p := netPersist{Down: map[string]downRecord{}, Locations: map[string]locRecord{}}
	for id, st := range m.state {
		if st.status == stDown {
			p.Down[id] = downRecord{Since: st.since, Alerted: st.alerted}
		}
	}
	for name, l := range m.locs {
		if l.status == stLocDown || l.status == stBackup {
			p.Locations[name] = locRecord{Status: l.status, Since: l.since}
		}
	}
	b, err := json.Marshal(p)
	if err != nil || bytes.Equal(b, m.stateSaved) {
		return
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		m.log.Printf("stanje mrežne opreme: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath); err != nil {
		m.log.Printf("stanje mrežne opreme: %v", err)
		return
	}
	m.stateSaved = b
}
