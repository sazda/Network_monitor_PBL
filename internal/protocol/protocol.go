// Package protocol definiše poruke koje konzola i agent razmenjuju preko
// WebSocket kontrolnog kanala. Sve poruke su JSON objekti sa poljem "type".
package protocol

import (
	"encoding/json"
	"time"
)

// Verzija protokola; agent je šalje u hello poruci.
const Version = 1

// Tipovi poruka.
const (
	TypeHello   = "hello"   // agent -> konzola, prva poruka posle povezivanja
	TypeCommand = "command" // konzola -> agent, zahtev za merenje
	TypeResult  = "result"  // agent -> konzola, odgovor na command
	// TypeProgress: agent -> konzola, međurezultat merenja koje je u toku
	// (prikaz uživo). Starije konzole ovu poruku ignorišu.
	TypeProgress = "progress"
	// TypeCancel: konzola -> agent, prekid komande sa datim ID-jem (npr.
	// korisnik zaustavi dug ping). Agent vraća dotadašnji rezultat.
	TypeCancel = "cancel"
	// TypeMetrics: agent za servere -> konzola, stanje servera (uptime, CPU,
	// RAM, diskovi) na 30 sekundi.
	TypeMetrics = "metrics"
	// TypeLive: agent za servere -> konzola, samo CPU i RAM na 5 sekundi
	// (prikaz u tabeli; istorija i obaveštenja koriste TypeMetrics).
	// Izdanje agenta za servere 2 i novije.
	TypeLive = "live"
	// TypeNetStats: agent za računare -> konzola, saobraćaj mrežnih kartica
	// na 10 sekundi (izdanje agenta 2 i novije).
	TypeNetStats = "netstats"
)

// RoleServer: agent za servere (poseban program i servis); ne izvršava
// merenja, samo šalje stanje servera.
const RoleServer = "server"

// Izdanja agenata. Broj se povećava (ručno, uz izmenu agenta) samo kada
// nadogradnja agenta donosi nešto novo: novu mogućnost ili ispravku u samom
// agentu. Konzola traži nadogradnju agenta čije je izdanje manje od ovog;
// izmene samo u konzoli ne menjaju izdanje. Agenti pre uvođenja izdanja
// javljaju 0.
const (
	// AgentRelease: agent za računare.
	//   1 – uvedeno izdanje agenta.
	//   2 – saobraćaj mrežnih kartica (brzina računara u detaljima switch-a).
	AgentRelease = 2
	// ServerAgentRelease: agent za servere.
	//   1 – procesi sa najvećim zauzećem procesora.
	//   2 – CPU i RAM na 5 s (TypeLive).
	ServerAgentRelease = 2
)

// WantRelease vraća izdanje koje konzola očekuje za agenta date uloge.
func WantRelease(role string) int {
	if role == RoleServer {
		return ServerAgentRelease
	}
	return AgentRelease
}

// Komande koje agent podržava. Agent izvršava ISKLJUČIVO ove komande —
// ne postoji mogućnost izvršavanja proizvoljnih komandi na računaru korisnika.
const (
	CmdLatency   = "latency"    // latencija (ICMP ili TCP) prema zadatom cilju
	CmdSpeedTest = "speedtest"  // propusnost agent <-> konzola (HTTP)
	CmdLANServer = "lan_server" // agent otvara privremeni TCP server za LAN test
	CmdLANClient = "lan_client" // agent meri propusnost prema drugom agentu
)

// Envelope je zajednički omotač svih poruka.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`      // ID zahteva (command/result)
	Command string          `json:"command,omitempty"` // samo za command
	Params  json.RawMessage `json:"params,omitempty"`  // samo za command
	Hello   *Hello          `json:"hello,omitempty"`   // samo za hello
	OK      bool            `json:"ok,omitempty"`      // samo za result
	Error   string          `json:"error,omitempty"`   // samo za result
	Data    json.RawMessage `json:"data,omitempty"`    // samo za result

	Progress *Progress `json:"progress,omitempty"` // samo za progress
	Metrics  *Metrics  `json:"metrics,omitempty"`  // samo za metrics
	Live     *Live     `json:"live,omitempty"`     // samo za live
	NetStats []IfRate  `json:"netstats,omitempty"` // samo za netstats
}

// IfRate je saobraćaj jedne mrežne kartice računara (bitova u sekundi,
// prosek od prethodnog slanja).
type IfRate struct {
	MAC  string  `json:"mac"`
	Recv float64 `json:"recv_bps"` // računar prima
	Send float64 `json:"send_bps"` // računar šalje
}

// Live je brzo stanje servera: CPU (prosek od prethodnog slanja) i memorija.
type Live struct {
	CPUPct   float64 `json:"cpu_pct"`
	MemTotal uint64  `json:"mem_total"`
	MemUsed  uint64  `json:"mem_used"`
}

// Metrics je stanje servera (agent za servere).
type Metrics struct {
	UptimeSec uint64  `json:"uptime_sec"`
	CPUPct    float64 `json:"cpu_pct"`
	MemTotal  uint64  `json:"mem_total"` // bajtova
	MemUsed   uint64  `json:"mem_used"`
	Disks     []Disk  `json:"disks,omitempty"`
	// Procs: procesi koji najviše opterećuju procesor (do 5; novije verzije
	// agenta za servere).
	Procs []Proc `json:"procs,omitempty"`
}

// Proc je jedan proces: zauzeće procesora (u % celog servera, svih jezgara)
// i memorije (radni skup, bajtova). Komandna linija se ne šalje.
type Proc struct {
	Name   string  `json:"name"`
	PID    uint32  `json:"pid"`
	CPUPct float64 `json:"cpu_pct"`
	Mem    uint64  `json:"mem"`
}

// Disk je jedan lokalni disk (Windows: slovo diska, npr. „C:”).
type Disk struct {
	Name  string `json:"name"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// Progress je međurezultat merenja brzine (šalje se oko dva puta u sekundi).
type Progress struct {
	Phase string  `json:"phase"` // latency | download | upload | probe
	Mbps  float64 `json:"mbps"`  // trenutna brzina (poslednjih ~0,5 s)

	// Samo za phase "probe" (merenje latencije, jedan paket):
	Seq   int     `json:"seq,omitempty"`    // redni broj (od 1)
	RTTMS float64 `json:"rtt_ms,omitempty"` // vreme odgovora
	Lost  bool    `json:"lost,omitempty"`   // nije stigao odgovor
}

// Interface opisuje mrežni interfejs agenta.
type Interface struct {
	Name  string   `json:"name"`
	MAC   string   `json:"mac,omitempty"`
	Addrs []string `json:"addrs"` // CIDR notacija, npr. 192.168.1.10/24
}

// Hello šalje agent odmah po povezivanju.
type Hello struct {
	AgentID    string      `json:"agent_id"`
	Hostname   string      `json:"hostname"`
	OS         string      `json:"os"`
	Arch       string      `json:"arch"`
	Version    string      `json:"version"`
	Protocol   int         `json:"protocol"`
	Interfaces []Interface `json:"interfaces"`
	Gateway    string      `json:"gateway,omitempty"`
	// Features su mogućnosti agenta koje starije verzije nemaju.
	Features []string `json:"features,omitempty"`
	// Role: "" = agent za računare, RoleServer = agent za servere.
	Role string `json:"role,omitempty"`
	// Release: izdanje agenta (AgentRelease / ServerAgentRelease).
	Release int `json:"release,omitempty"`
}

// FeatureLive: agent šalje međurezultate merenja (LAN test i latencija uživo).
const FeatureLive = "live"

// FeaturePing: agent podržava ping u trajanju (DurationSec) i prekid komande.
const FeaturePing = "ping"

// LatencyParams su parametri komande latency.
type LatencyParams struct {
	// Target: prazno = konzola, "gateway" = podrazumevani gateway agenta,
	// inače host (za ICMP) ili host:port (za TCP).
	Target     string `json:"target"`
	Method     string `json:"method"`      // icmp | tcp; prazno = tcp za konzolu i host:port, inače icmp
	Count      int    `json:"count"`       // broj merenja
	IntervalMS int    `json:"interval_ms"` // pauza između merenja
	TimeoutMS  int    `json:"timeout_ms"`  // timeout jednog merenja
	// DurationSec > 0: ping u trajanju (broj merenja = trajanje / interval,
	// interval 200 ms–10 s). Stariji agenti ga ignorišu i koriste Count.
	DurationSec int `json:"duration_sec,omitempty"`
}

// LatencyResult je rezultat komande latency. Vremena su u milisekundama.
type LatencyResult struct {
	Target   string    `json:"target"`
	Method   string    `json:"method"`
	Sent     int       `json:"sent"`
	Received int       `json:"received"`
	LossPct  float64   `json:"loss_pct"`
	MinMS    float64   `json:"min_ms"`
	AvgMS    float64   `json:"avg_ms"`
	MaxMS    float64   `json:"max_ms"`
	JitterMS float64   `json:"jitter_ms"` // srednja apsolutna razlika uzastopnih merenja (RFC 3550 stil)
	Samples  []float64 `json:"samples_ms"`
	Lost     []int     `json:"lost,omitempty"`    // redni brojevi (od 1) izgubljenih paketa; stariji agenti ga ne šalju
	Stopped  bool      `json:"stopped,omitempty"` // merenje je zaustavljeno pre kraja
	// TimesMS: vreme svakog poslatog paketa (ms od početka), redom po rednom
	// broju; popunjava ga konzola iz međurezultata (za izveštaj).
	TimesMS []int64 `json:"times_ms,omitempty"`
}

// SpeedTestParams su parametri komande speedtest.
type SpeedTestParams struct {
	Direction string `json:"direction"` // download | upload | both
	Seconds   int    `json:"seconds"`   // trajanje po smeru
	Streams   int    `json:"streams"`   // broj paralelnih konekcija
}

// ThroughputResult opisuje jedno merenje propusnosti u jednom smeru.
type ThroughputResult struct {
	Direction string  `json:"direction"`
	Bytes     int64   `json:"bytes"`
	Seconds   float64 `json:"seconds"`
	Mbps      float64 `json:"mbps"`
	Streams   int     `json:"streams"`
}

// SpeedTestResult je rezultat komande speedtest ili lan_client.
type SpeedTestResult struct {
	Peer     string             `json:"peer"`
	Download *ThroughputResult  `json:"download,omitempty"`
	Upload   *ThroughputResult  `json:"upload,omitempty"`
	Latency  *LatencyResult     `json:"latency,omitempty"`
	Extra    map[string]float64 `json:"extra,omitempty"`
}

// LANServerParams: agent otvara privremeni TCP server koji prihvata samo
// konekcije koje znaju jednokratni token.
type LANServerParams struct {
	Port       int    `json:"port"`        // 0 = slučajan port
	Token      string `json:"token"`       // jednokratni token koji generiše konzola
	TTLSeconds int    `json:"ttl_seconds"` // posle ovoga server se gasi
}

// LANServerResult vraća adrese na kojima agent sluša.
type LANServerResult struct {
	Port  int      `json:"port"`
	Addrs []string `json:"addrs"`
}

// LANClientParams: agent se povezuje na LAN server drugog agenta.
type LANClientParams struct {
	Addr      string `json:"addr"` // host:port
	Token     string `json:"token"`
	Direction string `json:"direction"`
	Seconds   int    `json:"seconds"`
	Streams   int    `json:"streams"`
	// LatencyCount je broj merenja latencije pre testa brzine (0 = 5).
	// Stariji agenti ga ignorišu i uvek mere 5 puta.
	LatencyCount int `json:"latency_count,omitempty"`
}

// Uobičajene vrednosti i granice — agent ih nameće i kada konzola pošalje
// nešto drugo, kako kompromitovana konzola ne bi mogla da zaguši mrežu.
const (
	DefaultLatencyCount = 10
	MaxLatencyCount     = 100
	DefaultSeconds      = 5
	MaxSeconds          = 30
	DefaultStreams      = 4
	MaxStreams          = 16
	DefaultLANTTL       = 60
	MaxLANTTL           = 300
	MaxPingSeconds      = 24 * 3600 // najduži ping (i neprekidni ping se tada sam zaustavlja)
	MaxPingProbes       = 172800    // najviše paketa u jednom pingu (24 h na 0,5 s)

	PingPeriod = 25 * time.Second
	PongWait   = 60 * time.Second
)

// Clamp vraća v ograničeno na [1, max], uz def ako je v <= 0.
func Clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// NormalizeDirection vraća "download", "upload" ili "both".
func NormalizeDirection(d string) string {
	switch d {
	case "download", "upload":
		return d
	default:
		return "both"
	}
}
