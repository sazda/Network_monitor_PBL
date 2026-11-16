// Komanda console pokreće centralnu konzolu. Može da radi u prvom planu ili
// kao Windows servis. Obično se instalira MSI paketom, koji sam registruje servis.
//
//	console [-config C:\ProgramData\NetworkMonitor\console.json] run
//	console install | uninstall | start | stop | restart
//	console hash-password           (pravi bcrypt hash lozinke za config)
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/sazda/network-monitor/internal/console"
	"github.com/sazda/network-monitor/internal/logfile"
)

type program struct {
	cfg *console.FileConfig
	hs  *http.Server
	srv *console.Server
}

func (p *program) Start(s service.Service) error {
	c := p.cfg
	var certs *console.CertStore
	var tlsCfg *tls.Config
	if !c.InsecureHTTP {
		cert, err := console.LoadCertificate(c)
		if err != nil {
			return err
		}
		if cert.Leaf != nil && time.Until(cert.Leaf.NotAfter) < 30*24*time.Hour {
			log.Printf("UPOZORENJE: TLS sertifikat ističe %s", cert.Leaf.NotAfter.Format("2006-01-02"))
		}
		certs = console.NewCertStore(cert)
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.GetCertificate}
	}
	srv, err := console.New(console.Config{
		AgentToken: c.AgentToken,
		AdminUser:  c.AdminUser,
		AdminPass:  c.AdminPassword,
		AdminHash:  c.AdminPasswordHash,
		Users:      c.Users,
		Locations:  c.Locations,
		Notify:     c.Notify,
		SNMP:       c.SNMP,
		SyslogAddr: c.SyslogAddr,
		DataDir:    c.DataDir,
		File:       c,
		Certs:      certs,
	})
	if err != nil {
		return fmt.Errorf("konfiguracija: %w", err)
	}
	p.srv = srv
	p.hs = console.NewHTTPServer(c.Listen, srv.Handler(), tlsCfg)
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	log.Printf("konzola sluša na %s (TLS: %v), konfiguracija %s, podaci u %s", c.Listen, tlsCfg != nil, c.Path(), c.DataDir)
	if c.AdminPasswordHash == "" && c.AdminPassword == "" {
		log.Printf("administratorska lozinka nije postavljena: otvorite konzolu na samom serveru (https://localhost%s/)", portSuffix(c.Listen))
	}
	go func() {
		var err error
		if tlsCfg != nil {
			err = p.hs.ServeTLS(ln, "", "")
		} else {
			err = p.hs.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.hs == nil {
		return nil
	}
	// Pingovi u toku se zaustavljaju i njihovi rezultati čuvaju pre gašenja.
	if p.srv != nil {
		p.srv.Close(5 * time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.hs.Shutdown(ctx)
}

func portSuffix(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil && port != "443" {
		return ":" + port
	}
	return ""
}

// loadConfig učitava konfiguraciju, dopunjava podrazumevane vrednosti
// (token, privremeni sertifikat) i snima ih.
func loadConfig(path, listen string) (*console.FileConfig, error) {
	c, err := console.LoadFileConfig(path)
	if err != nil {
		return nil, err
	}
	changed, err := c.EnsureDefaults()
	if err != nil {
		return nil, err
	}
	if listen != "" && listen != c.Listen {
		c.Listen = listen
		changed = true
	}
	if changed {
		if err := c.Save(); err != nil {
			return nil, fmt.Errorf("snimanje %s: %w", path, err)
		}
	}
	return c, nil
}

func main() {
	cfgPath := flag.String("config", os.Getenv("NM_CONFIG"), "putanja do konfiguracionog fajla (JSON)")
	listen := flag.String("listen", "", "adresa i port, npr. :8443 (upisuje se u konfiguraciju)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"Upotreba: %s [-config fajl] [-listen :port] [run|install|uninstall|start|stop|restart|hash-password]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	action := flag.Arg(0)
	if action == "" {
		action = "run"
	}

	if action == "hash-password" {
		hashPassword()
		return
	}

	path := *cfgPath
	if path == "" {
		path = console.DefaultConfigPath()
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	args := []string{"-config", path, "run"}
	svcConfig := &service.Config{
		Name:        "network-monitor-console",
		DisplayName: "Network Monitor Console",
		Description: "Centralna konzola za merenje latencije i brzine mreže.",
		Arguments:   args,
	}
	prg := &program{}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	switch action {
	case "run":
		cfg, err := loadConfig(path, *listen)
		if !service.Interactive() {
			// Servis nema konzolu: log ide u fajl (pored podataka, a ako
			// konfiguracija ne može da se učita, pored konfiguracije).
			dir := filepath.Dir(path)
			if cfg != nil {
				dir = cfg.DataDir
			}
			if f, ferr := logfile.Setup(dir, "console.log"); ferr == nil {
				defer f.Close()
			}
		}
		if err != nil {
			log.Fatal(err)
		}
		prg.cfg = cfg
		if err := s.Run(); err != nil {
			log.Fatal(err)
		}
	case "install":
		// Proveravamo konfiguraciju pre instalacije, da servis ne bi pao odmah
		// po pokretanju.
		cfg, err := loadConfig(path, *listen)
		if err != nil {
			log.Fatal(err)
		}
		if !cfg.InsecureHTTP {
			if _, err := console.LoadCertificate(cfg); err != nil {
				log.Fatal(err)
			}
		}
		if err := s.Install(); err != nil {
			log.Fatalf("instalacija servisa: %v", err)
		}
		if err := s.Start(); err != nil {
			log.Fatalf("pokretanje servisa: %v", err)
		}
		fmt.Printf("Servis je instaliran i pokrenut (konfiguracija: %s).\n", path)
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

func hashPassword() {
	fmt.Fprint(os.Stderr, "Nova administratorska lozinka: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		log.Fatal(err)
	}
	h, err := console.HashPassword(strings.TrimRight(line, "\r\n"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(h)
}
