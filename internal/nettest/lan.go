package nettest

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LAN test: jedan agent pokreće privremeni TCP server (LANServer), drugi se na
// njega povezuje (LANWorker). Tako se meri stvarna brzina lokalne mreže između
// dva računara, a ne brzina veze prema konzoli na internetu.
//
// Protokol: klijent pošalje liniju "NMLAN1 <token> <down|up>\n".
//   down: server šalje podatke dok klijent ne zatvori konekciju
//   up:   klijent šalje podatke, server ih odbacuje

const lanMagic = "NMLAN1"

// LANServer je privremeni server za LAN test.
type LANServer struct {
	ln    net.Listener
	token string
	conns sync.WaitGroup
	sem   chan struct{}
	done  chan struct{}
	once  sync.Once
}

// StartLANServer otvara listener na zadatom portu (0 = slučajan) i gasi ga
// posle ttl-a.
func StartLANServer(port int, token string, ttl time.Duration) (*LANServer, error) {
	if len(token) < 16 {
		return nil, errors.New("token je prekratak")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	s := &LANServer{ln: ln, token: token, sem: make(chan struct{}, 32), done: make(chan struct{})}
	go s.serve()
	go func() {
		select {
		case <-time.After(ttl):
		case <-s.done:
		}
		s.Close()
	}()
	return s, nil
}

// Port vraća port na kom server sluša.
func (s *LANServer) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close gasi server.
func (s *LANServer) Close() {
	s.once.Do(func() {
		close(s.done)
		s.ln.Close()
	})
}

// Done se zatvara kada se server ugasi.
func (s *LANServer) Done() <-chan struct{} { return s.done }

func (s *LANServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		select {
		case s.sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-s.sem }()
			s.handle(c)
		}()
	}
}

func (s *LANServer) handle(c net.Conn) {
	defer c.Close()
	// Zaštita: konekcija mora da se autentifikuje u roku od 5s i sme da traje
	// najviše onoliko koliko i najduži test.
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(io.LimitReader(c, 256))
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != lanMagic || subtle.ConstantTimeCompare([]byte(f[1]), []byte(s.token)) != 1 {
		return
	}
	c.SetDeadline(time.Now().Add(2 * time.Minute))
	go func() {
		<-s.done
		c.Close()
	}()
	switch f[2] {
	case "down":
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		io.Copy(c, &PayloadReader{Ctx: ctx})
	case "up":
		io.Copy(io.Discard, c)
	}
}

// LANWorker vraća Worker koji meri propusnost prema LAN serveru drugog agenta.
// mode je "down" (preuzimanje sa drugog agenta) ili "up" (slanje).
func LANWorker(addr, token, mode string) Worker {
	return func(ctx context.Context, counter *atomic.Int64) error {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		go func() {
			<-ctx.Done()
			c.Close()
		}()
		if _, err := fmt.Fprintf(c, "%s %s %s\n", lanMagic, token, mode); err != nil {
			return err
		}
		if mode == "down" {
			_, err = io.Copy(CountingWriter{counter}, c)
		} else {
			_, err = io.Copy(c, &PayloadReader{Ctx: ctx, Counter: counter})
		}
		return err
	}
}
