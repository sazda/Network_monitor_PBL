package console

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// SNMPConfig su podešavanja za čitanje mrežne opreme preko SNMP-a (samo
// čitanje; konzola nikada ne šalje SET). Preporučuje se SNMPv3 sa
// autentifikacijom i šifrovanjem (authPriv).
type SNMPConfig struct {
	Version   string `json:"version"`              // "v3" (preporučeno) ili "v2c"
	Community string `json:"community,omitempty"`  // samo v2c
	User      string `json:"user,omitempty"`       // v3
	AuthProto string `json:"auth_proto,omitempty"` // SHA, SHA256, SHA384, SHA512, MD5
	AuthPass  string `json:"auth_pass,omitempty"`
	PrivProto string `json:"priv_proto,omitempty"` // AES, AES192C, AES256C, AES192, AES256, DES
	PrivPass  string `json:"priv_pass,omitempty"`
	Port      int    `json:"port,omitempty"`         // podrazumevano 161
	Interval  int    `json:"interval_sec,omitempty"` // podrazumevano 60 s
	Timeout   int    `json:"timeout_sec,omitempty"`  // podrazumevano 3 s
	// Profiles: dodatni imenovani nalozi (npr. „operater CE ruteri”) za
	// uređaje sa drugim SNMP podacima; uređaj bira nalog poljem SNMPProfile.
	// Port, interval i vreme čekanja su zajednički.
	Profiles []SNMPProfile `json:"profiles,omitempty"`

	// contextName: SNMP kontekst jedne sesije (npr. „vlan-20” za MAC tabelu
	// VLAN-a 20 na Cisco-u); ne čuva se u konfiguraciji.
	contextName string
}

// SNMPProfile je imenovani SNMP nalog (samo podaci za prijavu).
type SNMPProfile struct {
	Name      string `json:"name"`
	Version   string `json:"version"` // "v3" ili "v2c"
	Community string `json:"community,omitempty"`
	User      string `json:"user,omitempty"`
	AuthProto string `json:"auth_proto,omitempty"`
	AuthPass  string `json:"auth_pass,omitempty"`
	PrivProto string `json:"priv_proto,omitempty"`
	PrivPass  string `json:"priv_pass,omitempty"`
}

// forDevice vraća podešavanja za uređaj sa zadatim nalogom ("" =
// podrazumevani). Nepoznat nalog (obrisan) znači podrazumevani.
func (c SNMPConfig) forDevice(profile string) SNMPConfig {
	out := c
	out.Profiles = nil
	if profile == "" {
		return out
	}
	for _, p := range c.Profiles {
		if p.Name == profile {
			out.Version, out.Community, out.User = p.Version, p.Community, p.User
			out.AuthProto, out.AuthPass, out.PrivProto, out.PrivPass = p.AuthProto, p.AuthPass, p.PrivProto, p.PrivPass
			return out
		}
	}
	return out
}

// hasProfile kaže da li postoji imenovani nalog.
func (c *SNMPConfig) hasProfile(name string) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Profiles {
		if p.Name == name {
			return true
		}
	}
	return false
}

var authProtos = map[string]gosnmp.SnmpV3AuthProtocol{
	"MD5": gosnmp.MD5, "SHA": gosnmp.SHA, "SHA224": gosnmp.SHA224, "SHA256": gosnmp.SHA256,
	"SHA384": gosnmp.SHA384, "SHA512": gosnmp.SHA512,
}

var privProtos = map[string]gosnmp.SnmpV3PrivProtocol{
	"DES": gosnmp.DES, "AES": gosnmp.AES, "AES192": gosnmp.AES192, "AES256": gosnmp.AES256,
	"AES192C": gosnmp.AES192C, "AES256C": gosnmp.AES256C,
}

func (c *SNMPConfig) configured() bool {
	if c == nil {
		return false
	}
	if c.Version == "v2c" {
		return c.Community != ""
	}
	return c.User != ""
}

func (c *SNMPConfig) interval() time.Duration {
	if c == nil || c.Interval <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.Interval) * time.Second
}

// validate proverava podešavanja i popunjava podrazumevane vrednosti.
func (c *SNMPConfig) validate() error {
	if c.Port == 0 {
		c.Port = 161
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("neispravan SNMP port")
	}
	if c.Interval == 0 {
		c.Interval = 60
	}
	if c.Interval < 30 || c.Interval > 3600 {
		return errors.New("interval čitanja mora biti između 30 i 3600 sekundi")
	}
	if c.Timeout == 0 {
		c.Timeout = 3
	}
	if c.Timeout < 1 || c.Timeout > 30 {
		return errors.New("vreme čekanja mora biti između 1 i 30 sekundi")
	}
	if err := validateCreds(&c.Version, &c.Community, &c.User, &c.AuthProto, &c.AuthPass, &c.PrivProto, &c.PrivPass); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" || len(p.Name) > 40 {
			return errors.New("naziv dodatnog SNMP naloga: 1–40 znakova")
		}
		if seen[strings.ToLower(p.Name)] {
			return fmt.Errorf("SNMP nalog %q je naveden dvaput", p.Name)
		}
		seen[strings.ToLower(p.Name)] = true
		if err := validateCreds(&p.Version, &p.Community, &p.User, &p.AuthProto, &p.AuthPass, &p.PrivProto, &p.PrivPass); err != nil {
			return fmt.Errorf("SNMP nalog %q: %w", p.Name, err)
		}
	}
	return nil
}

// validateCreds proverava podatke za prijavu (glavni ili dodatni nalog).
func validateCreds(version, community, user, authProto, authPass, privProto, privPass *string) error {
	switch *version {
	case "v2c":
		*user, *authPass, *privPass, *authProto, *privProto = "", "", "", "", ""
		if *community == "" {
			return errors.New("unesite community")
		}
	case "v3":
		*community = ""
		if *user == "" {
			return errors.New("unesite SNMPv3 korisnika")
		}
		if *authProto == "" {
			*authProto = "SHA"
		}
		if _, ok := authProtos[*authProto]; !ok {
			return errors.New("nepoznat protokol autentifikacije")
		}
		if len(*authPass) < 8 {
			return errors.New("lozinka za autentifikaciju mora imati bar 8 karaktera")
		}
		if *privProto == "" {
			*privProto = "AES"
		}
		if _, ok := privProtos[*privProto]; !ok {
			return errors.New("nepoznat protokol šifrovanja")
		}
		if len(*privPass) < 8 {
			return errors.New("lozinka za šifrovanje mora imati bar 8 karaktera")
		}
	default:
		return errors.New("verzija mora biti v3 ili v2c")
	}
	return nil
}

// snmpClient je deo gosnmp API-ja koji koristimo (u testovima se zamenjuje).
type snmpClient interface {
	Get(oids []string) ([]gosnmp.SnmpPDU, error)
	BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error)
	Close()
}

type goSNMPClient struct{ g *gosnmp.GoSNMP }

func (c goSNMPClient) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	p, err := c.g.Get(oids)
	if err != nil {
		return nil, err
	}
	return p.Variables, nil
}

func (c goSNMPClient) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	return c.g.BulkWalkAll(oid)
}

func (c goSNMPClient) Close() {
	if c.g.Conn != nil {
		c.g.Conn.Close()
	}
}

// dialSNMP otvara SNMP sesiju ka uređaju.
func dialSNMP(ctx context.Context, cfg SNMPConfig, ip string) (snmpClient, error) {
	g := &gosnmp.GoSNMP{
		Target:         ip,
		Port:           uint16(cfg.Port),
		Context:        ctx,
		Timeout:        time.Duration(cfg.Timeout) * time.Second,
		Retries:        1,
		MaxRepetitions: 25,
		MaxOids:        gosnmp.MaxOids,
	}
	if g.Port == 0 {
		g.Port = 161
	}
	if g.Timeout == 0 {
		g.Timeout = 3 * time.Second
	}
	if cfg.Version == "v2c" {
		g.Version, g.Community = gosnmp.Version2c, cfg.Community
		if v, ok := strings.CutPrefix(cfg.contextName, "vlan-"); ok {
			g.Community += "@" + v // Cisco: community@VLAN
		}
	} else {
		g.ContextName = cfg.contextName
		g.Version = gosnmp.Version3
		g.SecurityModel = gosnmp.UserSecurityModel
		g.MsgFlags = gosnmp.AuthPriv
		g.SecurityParameters = &gosnmp.UsmSecurityParameters{
			UserName:                 cfg.User,
			AuthenticationProtocol:   authProtos[cfg.AuthProto],
			AuthenticationPassphrase: cfg.AuthPass,
			PrivacyProtocol:          privProtos[cfg.PrivProto],
			PrivacyPassphrase:        cfg.PrivPass,
		}
	}
	if err := g.Connect(); err != nil {
		return nil, err
	}
	return goSNMPClient{g}, nil
}

// OID-ovi (SNMPv2-MIB, IF-MIB, ENTITY-MIB).
const (
	oidSysDescr    = ".1.3.6.1.2.1.1.1.0"
	oidSysObjectID = ".1.3.6.1.2.1.1.2.0"
	oidSysUpTime   = ".1.3.6.1.2.1.1.3.0"
	oidSysName     = ".1.3.6.1.2.1.1.5.0"

	oidIfDescr      = ".1.3.6.1.2.1.2.2.1.2"
	oidIfType       = ".1.3.6.1.2.1.2.2.1.3"
	oidIfAdmin      = ".1.3.6.1.2.1.2.2.1.7"
	oidIfOper       = ".1.3.6.1.2.1.2.2.1.8"
	oidIfLastChange = ".1.3.6.1.2.1.2.2.1.9"
	oidIfInDiscards = ".1.3.6.1.2.1.2.2.1.13"
	oidIfInErrors   = ".1.3.6.1.2.1.2.2.1.14"
	oidIfOutDiscard = ".1.3.6.1.2.1.2.2.1.19"
	oidIfOutErrors  = ".1.3.6.1.2.1.2.2.1.20"
	oidIfName       = ".1.3.6.1.2.1.31.1.1.1.1"
	oidIfHCIn       = ".1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOut      = ".1.3.6.1.2.1.31.1.1.1.10"
	oidIfHighSpeed  = ".1.3.6.1.2.1.31.1.1.1.15"
	oidIfAlias      = ".1.3.6.1.2.1.31.1.1.1.18"

	oidEntClass  = ".1.3.6.1.2.1.47.1.1.1.1.5"
	oidEntSwRev  = ".1.3.6.1.2.1.47.1.1.1.1.10"
	oidEntSerial = ".1.3.6.1.2.1.47.1.1.1.1.11"
	oidEntModel  = ".1.3.6.1.2.1.47.1.1.1.1.13"
)

// Vrste interfejsa koje se prikazuju i za koje se čuva saobraćaj: fizički
// portovi i port-channel (agregacija). VLAN interfejsi, loopback i slično se
// preskaču.
var portTypes = map[int]bool{6: true, 62: true, 69: true, 117: true, 161: true}

// snmpSystem su osnovni podaci o uređaju.
type snmpSystem struct {
	SysName  string `json:"sys_name"`
	Descr    string `json:"descr"`
	ObjectID string `json:"object_id"`
	Model    string `json:"model,omitempty"`
	Serial   string `json:"serial,omitempty"`
	Version  string `json:"version,omitempty"`
	Uptime   int64  `json:"uptime_sec"` // sekundi od pokretanja
}

// portSample je stanje porta u jednom čitanju (brojači su sirovi).
type portSample struct {
	Index      int
	Name       string
	Descr      string
	Alias      string
	Type       int
	Admin      int // 1 = up, 2 = down (administrativno ugašen), 3 = testing
	Oper       int // 1 = up, 2 = down, ...
	SpeedMbps  int64
	LastChange int64 // ifLastChange (stotinke sekunde od pokretanja)
	InOctets   *big.Int
	OutOctets  *big.Int
	InErr      uint64
	OutErr     uint64
	InDisc     uint64
	OutDisc    uint64
}

func pduString(v gosnmp.SnmpPDU) string {
	switch x := v.Value.(type) {
	case []byte:
		return strings.TrimSpace(strings.ToValidUTF8(string(x), ""))
	case string:
		return strings.TrimSpace(x)
	}
	return ""
}

func pduInt(v gosnmp.SnmpPDU) int64 {
	if v.Value == nil || v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance {
		return 0
	}
	return gosnmp.ToBigInt(v.Value).Int64()
}

// lastIndex vraća poslednji broj iz OID-a (ifIndex, entPhysicalIndex).
func lastIndex(oid string) int {
	i := strings.LastIndexByte(oid, '.')
	n, _ := strconv.Atoi(oid[i+1:])
	return n
}

var versionRe = regexp.MustCompile(`(?i)\bversion[:\s]+([0-9][0-9A-Za-z.()_-]*)`)

// readSystem čita sistemske podatke; model, serijski broj i verziju samo kada
// je full (ređe, jer se ne menjaju).
func readSystem(c snmpClient, full bool) (snmpSystem, error) {
	var s snmpSystem
	vars, err := c.Get([]string{oidSysDescr, oidSysObjectID, oidSysUpTime, oidSysName})
	if err != nil {
		return s, err
	}
	for _, v := range vars {
		switch v.Name {
		case oidSysDescr:
			s.Descr = pduString(v)
		case oidSysObjectID:
			if o, ok := v.Value.(string); ok {
				s.ObjectID = o
			}
		case oidSysUpTime:
			s.Uptime = pduInt(v) / 100
		case oidSysName:
			s.SysName = pduString(v)
		}
	}
	if s.Descr == "" && s.SysName == "" {
		return s, errors.New("uređaj odgovara, ali ne daje podatke: na njemu verovatno nedostaje grupa ili view " +
			"za ovog SNMP korisnika (npr. snmp-server group NETMON … read NETMON-RO i snmp-server view NETMON-RO iso included)")
	}
	if m := versionRe.FindStringSubmatch(s.Descr); m != nil {
		s.Version = strings.TrimRight(m[1], ".,")
	}
	if !full {
		return s, nil
	}
	// Šasija (entPhysicalClass = 3): model i serijski broj. Kod stack-a ima
	// više šasija; uzima se prva.
	classes, err := c.BulkWalkAll(oidEntClass)
	if err != nil || len(classes) == 0 {
		return s, nil // ENTITY-MIB nije obavezan
	}
	chassis := 0
	for _, v := range classes {
		if pduInt(v) == 3 {
			chassis = lastIndex(v.Name)
			break
		}
	}
	if chassis == 0 {
		return s, nil
	}
	idx := "." + strconv.Itoa(chassis)
	ent, err := c.Get([]string{oidEntModel + idx, oidEntSerial + idx, oidEntSwRev + idx})
	if err != nil {
		return s, nil
	}
	for _, v := range ent {
		switch {
		case strings.HasPrefix(v.Name, oidEntModel+"."):
			s.Model = pduString(v)
		case strings.HasPrefix(v.Name, oidEntSerial+"."):
			s.Serial = pduString(v)
		case strings.HasPrefix(v.Name, oidEntSwRev+"."):
			if sw := pduString(v); sw != "" && s.Version == "" {
				s.Version = sw
			}
		}
	}
	return s, nil
}

// readPorts čita tabelu interfejsa (samo portove, vidi portTypes).
func readPorts(c snmpClient) ([]portSample, error) {
	types, err := c.BulkWalkAll(oidIfType)
	if err != nil {
		return nil, err
	}
	ports := map[int]*portSample{}
	var order []int
	for _, v := range types {
		t := int(pduInt(v))
		if !portTypes[t] {
			continue
		}
		i := lastIndex(v.Name)
		ports[i] = &portSample{Index: i, Type: t}
		order = append(order, i)
	}
	if len(ports) == 0 {
		return nil, nil
	}
	u64 := func(v gosnmp.SnmpPDU) uint64 {
		if v.Value == nil {
			return 0
		}
		return gosnmp.ToBigInt(v.Value).Uint64()
	}
	toBig := func(v gosnmp.SnmpPDU) *big.Int {
		if v.Value == nil || v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance {
			return nil
		}
		return gosnmp.ToBigInt(v.Value)
	}
	cols := []struct {
		oid string
		set func(p *portSample, v gosnmp.SnmpPDU)
	}{
		{oidIfDescr, func(p *portSample, v gosnmp.SnmpPDU) { p.Descr = pduString(v) }},
		{oidIfName, func(p *portSample, v gosnmp.SnmpPDU) { p.Name = pduString(v) }},
		{oidIfAlias, func(p *portSample, v gosnmp.SnmpPDU) { p.Alias = pduString(v) }},
		{oidIfAdmin, func(p *portSample, v gosnmp.SnmpPDU) { p.Admin = int(pduInt(v)) }},
		{oidIfOper, func(p *portSample, v gosnmp.SnmpPDU) { p.Oper = int(pduInt(v)) }},
		{oidIfHighSpeed, func(p *portSample, v gosnmp.SnmpPDU) { p.SpeedMbps = pduInt(v) }},
		{oidIfLastChange, func(p *portSample, v gosnmp.SnmpPDU) { p.LastChange = pduInt(v) }},
		{oidIfHCIn, func(p *portSample, v gosnmp.SnmpPDU) { p.InOctets = toBig(v) }},
		{oidIfHCOut, func(p *portSample, v gosnmp.SnmpPDU) { p.OutOctets = toBig(v) }},
		{oidIfInErrors, func(p *portSample, v gosnmp.SnmpPDU) { p.InErr = u64(v) }},
		{oidIfOutErrors, func(p *portSample, v gosnmp.SnmpPDU) { p.OutErr = u64(v) }},
		{oidIfInDiscards, func(p *portSample, v gosnmp.SnmpPDU) { p.InDisc = u64(v) }},
		{oidIfOutDiscard, func(p *portSample, v gosnmp.SnmpPDU) { p.OutDisc = u64(v) }},
	}
	for _, col := range cols {
		vars, err := c.BulkWalkAll(col.oid)
		if err != nil {
			return nil, fmt.Errorf("čitanje portova: %w", err)
		}
		for _, v := range vars {
			if p := ports[lastIndex(v.Name)]; p != nil {
				col.set(p, v)
			}
		}
	}
	out := make([]portSample, 0, len(order))
	for _, i := range order {
		p := ports[i]
		if p.Name == "" {
			p.Name = p.Descr
		}
		out = append(out, *p)
	}
	return out, nil
}

// counterDelta vraća razliku dva čitanja 32-bitnog brojača (uz prelazak
// preko nule); ok=false ako je brojač resetovan.
func counterDelta(prev, cur uint64) (uint64, bool) {
	if cur >= prev {
		return cur - prev, true
	}
	if prev <= math.MaxUint32 && prev > math.MaxUint32/2 {
		return cur + (math.MaxUint32 + 1 - prev), true
	}
	return 0, false
}

// octetRate vraća bita u sekundi između dva čitanja 64-bitnog brojača.
func octetRate(prev, cur *big.Int, dt float64) (float64, bool) {
	if prev == nil || cur == nil || dt <= 0 || cur.Cmp(prev) < 0 {
		return 0, false
	}
	d := new(big.Int).Sub(cur, prev)
	f, _ := new(big.Float).SetInt(d).Float64()
	return f * 8 / dt, true
}

// snmpErrorText daje razumljiv opis greške.
func snmpErrorText(err error) string {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "timeout"):
		return "uređaj ne odgovara na SNMP (proverite da li je SNMP uključen, da li ACL dozvoljava adresu konzole i da li su podaci za prijavu tačni)"
	case strings.Contains(low, "connection refused"):
		return "uređaj odbija SNMP (na njemu SNMP nije uključen ili radi na drugom portu)"
	case strings.Contains(low, "unknown user") || strings.Contains(low, "unknown security name"):
		return "uređaj ne poznaje SNMPv3 korisnika"
	case strings.Contains(low, "wrong digest") || strings.Contains(low, "authentication"):
		return "pogrešna lozinka ili protokol autentifikacije"
	case strings.Contains(low, "decrypt"):
		return "pogrešna lozinka ili protokol šifrovanja"
	case strings.Contains(low, "not authentic"):
		return "uređaj odgovara, ali ne prihvata SNMP podatke za prijavu (nalog, lozinke ili community se razlikuju od onih u konzoli; za uređaj sa drugim podacima izaberite dodatni SNMP nalog u Izmeni uređaja)"
	}
	return msg
}
