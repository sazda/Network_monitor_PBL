package console

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// FileConfig je sadržaj konfiguracionog fajla konzole (console.json).
//
// Fajl ne mora da postoji: pri prvom pokretanju konzola sama generiše token
// za agente i privremeni (self-signed) sertifikat, a administratorska lozinka
// se postavlja kroz web interfejs otvoren na samom serveru.
type FileConfig struct {
	Listen            string        `json:"listen"`
	DataDir           string        `json:"data_dir,omitempty"`
	TLSPFX            string        `json:"tls_pfx,omitempty"`          // PFX iz AD CS-a (sertifikat + ključ)
	TLSPFXPassword    string        `json:"tls_pfx_password,omitempty"` //
	TLSCert           string        `json:"tls_cert,omitempty"`         // ili PEM sertifikat + ključ
	TLSKey            string        `json:"tls_key,omitempty"`
	InsecureHTTP      bool          `json:"insecure_http,omitempty"`
	AgentToken        string        `json:"agent_token"`
	AdminUser         string        `json:"admin_user"`
	AdminPassword     string        `json:"admin_password,omitempty"`      // čist tekst (nije preporučeno)
	AdminPasswordHash string        `json:"admin_password_hash,omitempty"` // bcrypt
	Users             []User        `json:"users,omitempty"`               // dodatni korisnici
	Locations         []Location    `json:"locations"`                     // lokacije (opsezi IP adresa)
	Notify            *NotifyConfig `json:"notify,omitempty"`              // obaveštenja kada računar nije na vezi
	SNMP              *SNMPConfig   `json:"snmp,omitempty"`                // čitanje mrežne opreme (SNMP)
	SyslogAddr        string        `json:"syslog_addr,omitempty"`         // prijem syslog-a switch-eva; "" = ":5514", "-" = isključeno

	path string
	mu   sync.Mutex
}

// ProgramDataDir je podrazumevani direktorijum za konfiguraciju.
func ProgramDataDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "NetworkMonitor")
		}
		return `C:\ProgramData\NetworkMonitor`
	}
	return "/etc/network-monitor"
}

// DefaultConfigPath je podrazumevana putanja do console.json.
func DefaultConfigPath() string { return filepath.Join(ProgramDataDir(), "console.json") }

// LoadFileConfig čita konfiguraciju; ako fajl ne postoji, vraća podrazumevane
// vrednosti (fajl se kreira tek pozivom Save).
func LoadFileConfig(path string) (*FileConfig, error) {
	c := &FileConfig{Listen: ":8443", AdminUser: "admin", path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Notepad i PowerShell 5.1 (Set-Content -Encoding UTF8) dodaju UTF-8 BOM.
		b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	if c.DataDir == "" {
		if runtime.GOOS == "windows" {
			c.DataDir = filepath.Join(filepath.Dir(path), "console-data")
		} else {
			c.DataDir = "/var/lib/network-monitor"
		}
	}
	// Relativne putanje se tumače u odnosu na direktorijum konfiguracije
	// (servis ne radi iz direktorijuma u kom je pokrenut instaler).
	base := filepath.Dir(path)
	for _, p := range []*string{&c.DataDir, &c.TLSPFX, &c.TLSCert, &c.TLSKey} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, nil
}

// Path vraća putanju do fajla.
func (c *FileConfig) Path() string { return c.path }

// Dir vraća direktorijum konfiguracije (tu se čuvaju i sertifikati).
func (c *FileConfig) Dir() string { return filepath.Dir(c.path) }

// EnsureDefaults dopunjava ono što nedostaje za pokretanje: token za agente i
// privremeni self-signed sertifikat (ako TLS nije podešen). Vraća true ako je
// nešto promenjeno; tada treba pozvati Save.
func (c *FileConfig) EnsureDefaults() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	if c.AgentToken == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return false, err
		}
		c.AgentToken = hex.EncodeToString(b)
		changed = true
	}
	if c.AdminUser == "" {
		c.AdminUser = "admin"
		changed = true
	}
	// nil = lokacije nikad nisu podešavane (prazna lista [] znači da ih je
	// administrator namerno obrisao).
	if c.Locations == nil {
		locs, err := ParseLocationsText(DefaultLocations)
		if err != nil {
			return false, fmt.Errorf("podrazumevane lokacije: %w", err)
		}
		c.Locations = locs
		changed = true
	}
	if !c.InsecureHTTP && c.TLSPFX == "" && (c.TLSCert == "" || c.TLSKey == "") {
		certPath, keyPath, err := GenerateSelfSigned(c.Dir())
		if err != nil {
			return false, fmt.Errorf("pravljenje privremenog sertifikata: %w", err)
		}
		c.TLSCert, c.TLSKey = certPath, keyPath
		changed = true
	}
	return changed, nil
}

// Update menja konfiguraciju i odmah je snima.
func (c *FileConfig) Update(fn func(*FileConfig)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
	return c.saveLocked()
}

// Save snima konfiguraciju (atomski, preko privremenog fajla).
func (c *FileConfig) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

func (c *FileConfig) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
