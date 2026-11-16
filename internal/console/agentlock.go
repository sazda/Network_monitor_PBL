package console

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ErrBusy: na računaru je već u toku merenje.
var ErrBusy = errors.New("merenje je već u toku")

// agentLocks sprečava da se isti računar istovremeno meri dva puta (npr. dva
// korisnika pokrenu test brzine na istom računaru): testovi bi delili istu
// vezu i oba rezultata bi bila pogrešna.
type agentLocks struct {
	mu sync.Mutex
	m  map[string]agentLock
}

type agentLock struct{ user, kind string }

var kindLabels = map[string]string{"latency": "merenje latencije", "speedtest": "test brzine", "lan": "LAN test", "ping": "ping"}

// acquire zaključava sve navedene agente ili nijednog. Vraća funkciju za
// otključavanje.
func (l *agentLocks) acquire(ids []string, user, kind string, hostName func(string) string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]agentLock{}
	}
	for _, id := range ids {
		if cur, ok := l.m[id]; ok {
			who := cur.user
			if who == user {
				who = "vi"
			}
			return nil, fmt.Errorf("%w: na računaru %s je u toku %s (pokrenuo: %s); sačekajte da se završi",
				ErrBusy, hostName(id), kindLabels[cur.kind], who)
		}
	}
	for _, id := range ids {
		l.m[id] = agentLock{user: user, kind: kind}
	}
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, id := range ids {
			delete(l.m, id)
		}
	}, nil
}

// lockAgents zaključava agente za merenje; ako je neki zauzet, odgovara sa
// 409 i vraća nil (takav pokušaj se ne upisuje u istoriju rezultata).
func (s *Server) lockAgents(w http.ResponseWriter, r *http.Request, kind string, ids ...string) func() {
	release, err := s.locks.acquire(ids, identityFrom(r).Name, kind, func(id string) string {
		id = strings.TrimPrefix(id, pingLockKey)
		if d, ok := s.net.find(strings.TrimPrefix(id, netdevPrefix)); ok && strings.HasPrefix(id, netdevPrefix) {
			return d.Name
		}
		if a, ok := s.hub.Agent(id); ok && a.Hostname != "" {
			return a.Hostname
		}
		return id
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return nil
	}
	return release
}
