package console

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ---------- režim podešavanja (prvo pokretanje) ----------

// isLocalRequest vraća true samo za zahteve sa samog servera (loopback), koji
// nisu prosleđeni preko reverse proxy-ja.
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// serveSetup obrađuje sve administratorske zahteve dok lozinka nije
// postavljena. Lozinku može da postavi samo neko ko je prijavljen na sam
// server, kako je ne bi prvi postavio bilo ko iz mreže.
func (s *Server) serveSetup(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "Konzola još nije podešena.\n\n"+
			"Prijavite se na server na kom je instalirana konzola i otvorite\n"+
			"Start meni -> Network Monitor konzola (ili https://localhost:<port>/)\n"+
			"da biste postavili administratorsku lozinku.\n")
		return
	}
	sub, _ := fs.Sub(webFS, "web")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/setup":
		s.handleSetup(w, r)
	case r.Method == http.MethodGet && (r.URL.Path == "/setup.js" || r.URL.Path == "/style.css"):
		http.ServeFileFS(w, r, sub, r.URL.Path[1:])
	case r.Method == http.MethodGet && !isAPIPath(r.URL.Path):
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, sub, "setup.html")
	default:
		writeErr(w, http.StatusServiceUnavailable, errors.New("konzola još nije podešena"))
	}
}

func isAPIPath(p string) bool { return len(p) >= 5 && p[:5] == "/api/" }

type setupRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); len(ct) < 16 || ct[:16] != "application/json" {
		http.Error(w, "očekuje se application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req setupRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.User == "" {
		req.User = "admin"
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if !s.needsSetup() {
		writeErr(w, http.StatusConflict, errors.New("lozinka je već postavljena"))
		return
	}
	if err := s.persist(func(c *FileConfig) {
		c.AdminUser, c.AdminPasswordHash, c.AdminPassword = req.User, hash, ""
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.setCredentials(req.User, hash)
	s.startSession(w, r, req.User) // odmah prijavljen
	s.log.Printf("administratorska lozinka postavljena (korisnik %q)", req.User)
	writeJSON(w, http.StatusOK, map[string]string{"user": req.User})
}

// persist snima izmenu u konfiguracioni fajl (ako postoji).
func (s *Server) persist(fn func(*FileConfig)) error {
	if s.cfg.File == nil {
		return nil
	}
	if err := s.cfg.File.Update(fn); err != nil {
		return fmt.Errorf("snimanje konfiguracije: %w", err)
	}
	return nil
}

// ---------- podešavanja ----------

type settingsResponse struct {
	AdminUser   string    `json:"admin_user"`
	AgentToken  string    `json:"agent_token"`
	TLS         bool      `json:"tls"`
	Certificate *CertInfo `json:"certificate,omitempty"`
	ConfigPath  string    `json:"config_path,omitempty"`
	AgentURL    string    `json:"agent_url,omitempty"` // predlog adrese konzole za agente
	AgentMSI    bool      `json:"agent_msi"`           // da li je MSI agenta dostupan za instalaciju iz konzole
	ServerMSI   bool      `json:"server_agent_msi"`    // isto za agenta za servere
	Listen      string    `json:"listen,omitempty"`
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	user, _, _ := s.credentials()
	resp := settingsResponse{AdminUser: user, AgentToken: s.cfg.AgentToken, TLS: s.cfg.Certs != nil}
	if s.cfg.Certs != nil {
		resp.Certificate = s.cfg.Certs.Info()
	}
	resp.AgentURL = s.defaultAgentURL()
	_, msiErr := s.agentMSIPath()
	resp.AgentMSI = msiErr == nil
	_, srvErr := s.msiPath(serverAgentMSIName)
	resp.ServerMSI = srvErr == nil
	if s.cfg.File != nil {
		resp.ConfigPath = s.cfg.File.Path()
		resp.Listen = s.cfg.File.Listen
	}
	writeJSON(w, http.StatusOK, resp)
}

type passwordRequest struct {
	Current     string `json:"current"`
	NewPassword string `json:"new_password"`
}

// handleChangePassword menja lozinku prijavljenog korisnika.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	me := identityFrom(r)
	if _, ok := s.authenticate(me.Name, req.Current); !ok {
		s.limiter.fail(clientIP(r))
		writeErr(w, http.StatusForbidden, errors.New("trenutna lozinka nije ispravna"))
		return
	}
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if me.Primary {
		if err := s.persist(func(c *FileConfig) {
			c.AdminPasswordHash, c.AdminPassword = hash, ""
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		s.setCredentials(me.Name, hash)
	} else {
		s.passMu.Lock()
		users := append([]User(nil), s.users...)
		for i := range users {
			if users[i].Name == me.Name {
				users[i].PasswordHash = hash
			}
		}
		err := s.saveUsersLocked(users)
		s.passMu.Unlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.log.Printf("korisnik %q je promenio lozinku (sa %s)", me.Name, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type certificateRequest struct {
	PFXBase64 string `json:"pfx_base64"`
	Password  string `json:"password"`
}

func (s *Server) handleUploadCertificate(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Certs == nil {
		writeErr(w, http.StatusConflict, errors.New("konzola radi bez TLS-a (insecure_http); sertifikat se ne može promeniti ovde"))
		return
	}
	var req certificateRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("neispravan JSON: %w", err))
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.PFXBase64)
	if err != nil || len(data) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("PFX fajl nije ispravno poslat"))
		return
	}
	cert, err := ParsePFX(data, req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if time.Now().After(cert.Leaf.NotAfter) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("sertifikat je istekao %s", cert.Leaf.NotAfter.Format("2006-01-02")))
		return
	}

	dir := s.cfg.DataDir
	if s.cfg.File != nil {
		dir = s.cfg.File.Dir()
	}
	path := filepath.Join(dir, "console.pfx")
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.persist(func(c *FileConfig) {
		c.TLSPFX, c.TLSPFXPassword = path, req.Password
		c.TLSCert, c.TLSKey = "", ""
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.cfg.Certs.Set(cert)
	s.log.Printf("TLS sertifikat zamenjen: %s (važi do %s)", cert.Leaf.Subject, cert.Leaf.NotAfter.Format("2006-01-02"))
	writeJSON(w, http.StatusOK, s.cfg.Certs.Info())
}
