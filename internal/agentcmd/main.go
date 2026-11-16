// Package agentcmd je zajednički glavni program agenta za računare
// (cmd/agent) i agenta za servere (cmd/serveragent).
package agentcmd

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kardianos/service"

	"github.com/sazda/network-monitor/internal/agent"
	"github.com/sazda/network-monitor/internal/logfile"
)

type program struct {
	// load učitava konfiguraciju (flagovi, fajl sa tokenom, registry). Poziva
	// se ponovo ako konfiguracija nije potpuna, da servis ne bi morao da se
	// restartuje posle ispravke podešavanja.
	load   func() agent.Config
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		for {
			cfg := p.load()
			a, err := agent.New(cfg)
			if err == nil {
				a.Run(ctx)
				return
			}
			log.Printf("agent nije podešen: %v; nova provera za 60s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
	return nil
}

// Options razlikuju agenta za računare i agenta za servere (isti kod, poseban
// program, servis i podešavanja u registry-ju).
type Options struct {
	ServiceName string
	DisplayName string
	Description string
	RegistryKey string // HKLM\... sa adresom konzole i tokenom (upisuje MSI)
	StateSubdir string // poddirektorijum u ProgramData\NetworkMonitor ("" = sam direktorijum)
	Role        string
}

// Main je glavni program agenta.
func Main(o Options) {
	consoleURL := flag.String("console", os.Getenv("NM_CONSOLE_URL"), "URL konzole, npr. https://monitor.example.com")
	tokenFile := flag.String("token-file", os.Getenv("NM_AGENT_TOKEN_FILE"), "fajl sa tokenom (alternativa za NM_AGENT_TOKEN)")
	stateDir := flag.String("state-dir", os.Getenv("NM_STATE_DIR"), "direktorijum za ID agenta")
	caFile := flag.String("ca", os.Getenv("NM_CA_FILE"), "CA sertifikat konzole (nije potreban ako je sertifikat izdao interni CA kome Windows veruje)")
	insecure := flag.Bool("insecure", false, "ne proveravaj TLS sertifikat konzole (SAMO za testiranje)")
	version := flag.Bool("version", false, "prikaži verziju")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Upotreba: %s [opcije] [run|install|uninstall|start|stop|restart]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *version {
		fmt.Println(agent.Version)
		return
	}
	action := flag.Arg(0)
	if action == "" {
		action = "run"
	}

	// Kao servis (npr. instaliran MSI paketom) agent podrazumevano čuva ID i
	// log u sistemskom direktorijumu.
	if !service.Interactive() && *stateDir == "" {
		*stateDir = defaultStateDir(o.StateSubdir)
	}

	// readToken: NM_AGENT_TOKEN, pa -token-file.
	readToken := func() (string, error) {
		token := os.Getenv("NM_AGENT_TOKEN")
		if *tokenFile != "" {
			b, err := os.ReadFile(*tokenFile)
			if err != nil {
				return "", fmt.Errorf("čitanje tokena: %w", err)
			}
			token = string(bytes.TrimSpace(b))
		}
		return token, nil
	}
	token, err := readToken()
	if err != nil {
		log.Fatal(err)
	}

	// Za servis: argumenti koji se upisuju u definiciju servisa. Token se NE
	// upisuje u komandnu liniju servisa, već se čuva u zaštićenom fajlu.
	absState := *stateDir
	if absState == "" {
		absState = defaultStateDir(o.StateSubdir)
	}
	svcArgs := []string{"-console", *consoleURL, "-state-dir", absState, "-token-file", filepath.Join(absState, "token")}
	if *caFile != "" {
		ca, _ := filepath.Abs(*caFile)
		svcArgs = append(svcArgs, "-ca", ca)
	}
	if *insecure {
		svcArgs = append(svcArgs, "-insecure")
	}
	svcArgs = append(svcArgs, "run")

	svcConfig := &service.Config{
		Name:        o.ServiceName,
		DisplayName: o.DisplayName,
		Description: o.Description,
		Arguments:   svcArgs,
	}
	// Redosled: flagovi / promenljive okruženja / fajl sa tokenom, pa
	// podešavanja koja je MSI upisao u registry (samo Windows).
	loadCfg := func() agent.Config {
		cfg := agent.Config{ConsoleURL: *consoleURL, StateDir: *stateDir, CAFile: *caFile, Insecure: *insecure, Role: o.Role}
		cfg.Token, _ = readToken()
		if cfg.ConsoleURL == "" || cfg.Token == "" {
			regURL, regToken := registryConfig(o.RegistryKey)
			if cfg.ConsoleURL == "" {
				cfg.ConsoleURL = regURL
			}
			if cfg.Token == "" {
				cfg.Token = regToken
			}
		}
		return cfg
	}
	prg := &program{load: loadCfg}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	switch action {
	case "run":
		if !service.Interactive() {
			// Kao servis: log ide u fajl pored ID-a agenta.
			if f, err := logfile.Setup(*stateDir, "agent.log"); err == nil {
				defer f.Close()
			}
		} else if c := loadCfg(); c.ConsoleURL == "" || c.Token == "" {
			log.Fatal("zadajte -console i token (NM_AGENT_TOKEN ili -token-file)")
		}
		if err := s.Run(); err != nil {
			log.Fatal(err)
		}
	case "install":
		if *consoleURL == "" || token == "" {
			log.Fatal("za instalaciju zadajte -console i token (NM_AGENT_TOKEN ili -token-file)")
		}
		if err := os.MkdirAll(absState, 0o700); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(absState, "token"), []byte(token+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
		if err := s.Install(); err != nil {
			log.Fatalf("instalacija servisa: %v", err)
		}
		if err := s.Start(); err != nil {
			log.Fatalf("pokretanje servisa: %v", err)
		}
		fmt.Println("Servis je instaliran i pokrenut.")
	case "uninstall", "start", "stop", "restart":
		if err := service.Control(s, action); err != nil {
			log.Fatalf("%s: %v", action, err)
		}
		fmt.Println("OK")
	default:
		flag.Usage()
		os.Exit(2)
	}
}
