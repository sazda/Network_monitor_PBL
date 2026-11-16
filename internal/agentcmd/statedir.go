package agentcmd

import (
	"os"
	"path/filepath"
	"runtime"
)

// defaultStateDir vraća sistemski direktorijum za podatke agenta kada radi
// kao servis (servis ne radi pod korisničkim nalogom pa UserConfigDir nije
// pouzdan).
func defaultStateDir(sub string) string {
	return filepath.Join(baseStateDir(), sub)
}

func baseStateDir() string {
	switch runtime.GOOS {
	case "windows":
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "NetworkMonitor")
		}
		return `C:\ProgramData\NetworkMonitor`
	case "darwin":
		return "/Library/Application Support/NetworkMonitor"
	default:
		return "/var/lib/network-monitor-agent"
	}
}
