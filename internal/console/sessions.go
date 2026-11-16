package console

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Web interfejs koristi prijavu preko stranice i kolačić sesije (sa
// dugmetom za odjavu). HTTP Basic prijava i dalje radi za skripte i REST API.

const (
	sessionCookie   = "nm_session"
	sessionIdle     = 4 * time.Hour  // odjava posle neaktivnosti
	sessionLifetime = 12 * time.Hour // najduže trajanje sesije
)

type session struct {
	name     string
	hash     string // hash lozinke u trenutku prijave; promena lozinke poništava sesiju
	created  time.Time
	lastSeen time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore { return &sessionStore{sessions: map[string]*session{}} }

func (st *sessionStore) create(name, hash string) string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	for t, s := range st.sessions {
		if s.expired(now) {
			delete(st.sessions, t)
		}
	}
	st.sessions[token] = &session{name: name, hash: hash, created: now, lastSeen: now}
	return token
}

func (s *session) expired(now time.Time) bool {
	return now.Sub(s.lastSeen) > sessionIdle || now.Sub(s.created) > sessionLifetime
}

func (st *sessionStore) get(token string) (session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[token]
	if !ok {
		return session{}, false
	}
	now := time.Now()
	if s.expired(now) {
		delete(st.sessions, token)
		return session{}, false
	}
	s.lastSeen = now
	return *s, true
}

func (st *sessionStore) delete(token string) {
	st.mu.Lock()
	delete(st.sessions, token)
	st.mu.Unlock()
}

// lookupUser vraća identitet i trenutni hash lozinke korisnika.
func (s *Server) lookupUser(name string) (Identity, string, bool) {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	if name == s.cfg.AdminUser {
		h := s.cfg.AdminHash
		if h == "" {
			h = "plain:" + s.cfg.AdminPass
		}
		return Identity{Name: name, Role: RoleAdmin, Primary: true}, h, true
	}
	for _, u := range s.users {
		if u.Name == name {
			return Identity{Name: u.Name, Role: u.Role}, u.PasswordHash, true
		}
	}
	return Identity{}, "", false
}

// sessionIdentity proverava kolačić sesije. Uloga se čita iz trenutne
// konfiguracije, a obrisan korisnik ili promenjena lozinka poništavaju sesiju.
func (s *Server) sessionIdentity(r *http.Request) (Identity, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return Identity{}, false
	}
	sess, ok := s.sessions.get(c.Value)
	if !ok {
		return Identity{}, false
	}
	id, hash, ok := s.lookupUser(sess.name)
	if !ok || hash != sess.hash {
		s.sessions.delete(c.Value)
		return Identity{}, false
	}
	return id, true
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, name string) {
	_, hash, _ := s.lookupUser(name)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sessions.create(name, hash),
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionLifetime / time.Second),
	})
}

// unauthorized odgovara neprijavljenom korisniku: API dobija 401, a
// pretraživač stranicu za prijavu.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && !isAPIPath(r.URL.Path) {
		sub, _ := fs.Sub(webFS, "web")
		switch r.URL.Path {
		case "/login.js", "/style.css":
			http.ServeFileFS(w, r, sub, r.URL.Path[1:])
		default:
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFileFS(w, r, sub, "login.html")
		}
		return
	}
	writeErr(w, http.StatusUnauthorized, errors.New("potrebna je prijava"))
}

type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, errors.New("previše neuspešnih pokušaja prijave, pokušajte kasnije"))
		return
	}
	if s.needsSetup() {
		writeErr(w, http.StatusServiceUnavailable, errors.New("konzola još nije podešena"))
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "očekuje se application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req loginRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id, ok := s.authenticate(strings.TrimSpace(req.User), req.Password)
	if !ok {
		s.limiter.fail(ip)
		s.log.Printf("neuspešna prijava (%q) sa %s", req.User, ip)
		writeErr(w, http.StatusUnauthorized, errors.New("pogrešno korisničko ime ili lozinka"))
		return
	}
	s.startSession(w, r, id.Name)
	s.log.Printf("prijava: %q sa %s", id.Name, ip)
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
