package console

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewHTTPServer pravi HTTP(S) server konzole. HTTP/2 je namerno isključen:
// WebSocket veza agenata radi samo preko HTTP/1.1, a agenti do ispravke u
// verziji 1.1 su posle testa brzine nudili „h2” i tada se nisu mogli
// ponovo povezati. Bez HTTP/2 na konzoli i takvi agenti rade.
func NewHTTPServer(addr string, h http.Handler, tlsCfg *tls.Config) *http.Server {
	if tlsCfg != nil {
		tlsCfg = tlsCfg.Clone()
		tlsCfg.NextProtos = []string{"http/1.1"}
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         tlsCfg,
		// Prazna (ne nil) mapa isključuje automatsko uključivanje HTTP/2.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		// Bez WriteTimeout-a: merenja i WebSocket veze traju dugo.
	}
}
