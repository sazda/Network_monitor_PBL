//go:build windows

package console

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

//go:embed deploy_agent.ps1
var deployScript []byte

// powershellRunner pokreće deploy_agent.ps1 u Windows PowerShell-u. Podaci
// (i lozinka) idu na standardni ulaz, ne u komandnu liniju.
func powershellRunner(dir string) deployRunner {
	script := filepath.Join(dir, "deploy-agent.ps1")
	var once sync.Once
	var writeErr error
	return func(ctx context.Context, t deployTarget) deployOutcome {
		// Skripta se upisuje jednom (više instalacija radi istovremeno), u
		// UTF-8 sa BOM-om da je Windows PowerShell 5.1 ispravno pročita.
		once.Do(func() {
			writeErr = os.WriteFile(script, append([]byte("\xef\xbb\xbf"), deployScript...), 0o600)
		})
		if writeErr != nil {
			return deployOutcome{Message: "upis skripte: " + writeErr.Error()}
		}
		in, _ := json.Marshal(t)
		ps := filepath.Join(os.Getenv("SystemRoot"), `System32\WindowsPowerShell\v1.0\powershell.exe`)
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Stdin = bytes.NewReader(in)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var out deployOutcome
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(lines[len(lines)-1])), &out); jerr != nil {
			msg := strings.TrimSpace(stderr.String())
			if ctx.Err() != nil {
				msg = "isteklo vreme (5 min)"
			} else if msg == "" && err != nil {
				msg = err.Error()
			}
			return deployOutcome{Message: msg}
		}
		return out
	}
}

func defaultDeployRunner(dir string) deployRunner { return powershellRunner(dir) }
