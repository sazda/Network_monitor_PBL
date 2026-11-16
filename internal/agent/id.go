package agent

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// loadOrCreateID čita ID agenta iz dir/agent-id, a ako ne postoji generiše
// novi. Stabilan ID omogućava konzoli da prepozna isti računar i posle
// promene imena ili IP adrese.
func loadOrCreateID(dir string) (string, error) {
	if dir == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			d = "."
		}
		dir = filepath.Join(d, "network-monitor")
	}
	path := filepath.Join(dir, "agent-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}
