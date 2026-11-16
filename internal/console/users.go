package console

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Uloge korisnika konzole.
const (
	RoleAdmin    = "admin"    // sve: merenja, podešavanja, korisnici, sertifikat
	RoleOperator = "operator" // samo pregled i pokretanje merenja
)

// User je dodatni korisnik konzole (pored glavnog administratora iz
// admin_user / admin_password_hash).
type User struct {
	Name         string `json:"name"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
}

// Identity je prijavljeni korisnik.
type Identity struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Primary bool   `json:"primary"` // glavni administrator (ne može se obrisati)
}

type identityKey struct{}

func identityFrom(r *http.Request) Identity {
	id, _ := r.Context().Value(identityKey{}).(Identity)
	return id
}

var userNameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func validRole(r string) bool { return r == RoleAdmin || r == RoleOperator }

// validateUsers proverava korisnike iz konfiguracije.
func validateUsers(primary string, users []User) ([]User, error) {
	seen := map[string]bool{strings.ToLower(primary): true}
	out := make([]User, 0, len(users))
	for _, u := range users {
		if !userNameRe.MatchString(u.Name) {
			return nil, fmt.Errorf("neispravno korisničko ime %q", u.Name)
		}
		if seen[strings.ToLower(u.Name)] {
			return nil, fmt.Errorf("korisnik %q je naveden više puta", u.Name)
		}
		seen[strings.ToLower(u.Name)] = true
		if !validRole(u.Role) {
			return nil, fmt.Errorf("korisnik %q: nepoznata uloga %q", u.Name, u.Role)
		}
		if _, err := bcrypt.Cost([]byte(u.PasswordHash)); err != nil {
			return nil, fmt.Errorf("korisnik %q: neispravan hash lozinke", u.Name)
		}
		out = append(out, u)
	}
	return out, nil
}

// authenticate proverava ime i lozinku. Uspešne bcrypt provere se keširaju
// (ključ uključuje i hash, pa promena lozinke automatski poništava keš),
// jer pretraživač šalje Basic autentifikaciju uz svaki zahtev.
func (s *Server) authenticate(name, pass string) (Identity, bool) {
	if name == "" || pass == "" {
		return Identity{}, false
	}
	s.passMu.Lock()
	var id Identity
	var hash, plain string
	switch {
	case name == s.cfg.AdminUser:
		id = Identity{Name: s.cfg.AdminUser, Role: RoleAdmin, Primary: true}
		hash, plain = s.cfg.AdminHash, s.cfg.AdminPass
	default:
		for _, u := range s.users {
			if strings.EqualFold(u.Name, name) {
				id = Identity{Name: u.Name, Role: u.Role}
				hash = u.PasswordHash
				break
			}
		}
	}
	s.passMu.Unlock()
	if id.Name == "" {
		// Ista količina posla kao za postojećeg korisnika (otežava
		// otkrivanje koja imena postoje).
		bcrypt.CompareHashAndPassword(dummyHash, []byte(pass))
		return Identity{}, false
	}
	if hash == "" {
		return id, plain != "" && subtle.ConstantTimeCompare([]byte(pass), []byte(plain)) == 1
	}

	key := sha256.Sum256([]byte(id.Name + "\x00" + pass + "\x00" + hash))
	s.passMu.Lock()
	cached := s.passCache[key]
	s.passMu.Unlock()
	if cached {
		return id, true
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
		return Identity{}, false
	}
	s.passMu.Lock()
	if s.passCache == nil || len(s.passCache) > 256 {
		s.passCache = map[[32]byte]bool{}
	}
	s.passCache[key] = true
	s.passMu.Unlock()
	return id, true
}

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("network-monitor-dummy"), 12)

// checkPassword proverava lozinku glavnog administratora.
func (s *Server) checkPassword(p string) bool {
	user, _, _ := s.credentials()
	_, ok := s.authenticate(user, p)
	return ok
}

// ---------- API ----------

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Identity
		Version string `json:"version"`
	}{identityFrom(r), Version})
}

type userInfo struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Primary bool   `json:"primary"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	s.passMu.Lock()
	out := []userInfo{{Name: s.cfg.AdminUser, Role: RoleAdmin, Primary: true}}
	for _, u := range s.users {
		out = append(out, userInfo{Name: u.Name, Role: u.Role})
	}
	s.passMu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

type userRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// saveUsersLocked snima korisnike u konfiguraciju; poziva se pod passMu.
func (s *Server) saveUsersLocked(users []User) error {
	if err := s.persist(func(c *FileConfig) { c.Users = append([]User(nil), users...) }); err != nil {
		return err
	}
	s.users = users
	return nil
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !userNameRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, errors.New("korisničko ime sme da sadrži slova, cifre i . _ @ - (najviše 64 znaka)"))
		return
	}
	if req.Role == "" {
		req.Role = RoleOperator
	}
	if !validRole(req.Role) {
		writeErr(w, http.StatusBadRequest, errors.New("uloga mora biti admin ili operator"))
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.passMu.Lock()
	defer s.passMu.Unlock()
	if strings.EqualFold(req.Name, s.cfg.AdminUser) {
		writeErr(w, http.StatusConflict, errors.New("korisnik sa tim imenom već postoji"))
		return
	}
	for _, u := range s.users {
		if strings.EqualFold(u.Name, req.Name) {
			writeErr(w, http.StatusConflict, errors.New("korisnik sa tim imenom već postoji"))
			return
		}
	}
	users := append(append([]User(nil), s.users...), User{Name: req.Name, PasswordHash: hash, Role: req.Role})
	if err := s.saveUsersLocked(users); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("korisnik %q (%s) dodat; dodao %q", req.Name, req.Role, identityFrom(r).Name)
	writeJSON(w, http.StatusCreated, userInfo{Name: req.Name, Role: req.Role})
}

// handleUpdateUser menja ulogu i/ili postavlja novu lozinku korisniku.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Role != "" && !validRole(req.Role) {
		writeErr(w, http.StatusBadRequest, errors.New("uloga mora biti admin ili operator"))
		return
	}
	var hash string
	if req.Password != "" {
		h, err := HashPassword(req.Password)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		hash = h
	}
	name := r.PathValue("name")
	me := identityFrom(r)
	s.passMu.Lock()
	defer s.passMu.Unlock()
	if strings.EqualFold(name, s.cfg.AdminUser) {
		writeErr(w, http.StatusForbidden, errors.New("glavni administrator menja svoju lozinku u odeljku „Moja lozinka”"))
		return
	}
	users := append([]User(nil), s.users...)
	idx := -1
	for i, u := range users {
		if strings.EqualFold(u.Name, name) {
			idx = i
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, errors.New("korisnik ne postoji"))
		return
	}
	if req.Role != "" && strings.EqualFold(users[idx].Name, me.Name) && req.Role != RoleAdmin {
		writeErr(w, http.StatusConflict, errors.New("ne možete sami sebi oduzeti administratorska prava"))
		return
	}
	if req.Role != "" {
		users[idx].Role = req.Role
	}
	if hash != "" {
		users[idx].PasswordHash = hash
	}
	if err := s.saveUsersLocked(users); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("korisnik %q izmenjen (uloga %s, nova lozinka: %v); izmenio %q", users[idx].Name, users[idx].Role, hash != "", me.Name)
	writeJSON(w, http.StatusOK, userInfo{Name: users[idx].Name, Role: users[idx].Role})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	me := identityFrom(r)
	s.passMu.Lock()
	defer s.passMu.Unlock()
	if strings.EqualFold(name, s.cfg.AdminUser) {
		writeErr(w, http.StatusForbidden, errors.New("glavni administrator se ne može obrisati"))
		return
	}
	if strings.EqualFold(name, me.Name) {
		writeErr(w, http.StatusConflict, errors.New("ne možete obrisati sopstveni nalog"))
		return
	}
	users := make([]User, 0, len(s.users))
	found := false
	for _, u := range s.users {
		if strings.EqualFold(u.Name, name) {
			found = true
			continue
		}
		users = append(users, u)
	}
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("korisnik ne postoji"))
		return
	}
	if err := s.saveUsersLocked(users); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("korisnik %q obrisan; obrisao %q", name, me.Name)
	w.WriteHeader(http.StatusNoContent)
}
