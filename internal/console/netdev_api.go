package console

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------- HTTP API za mrežnu opremu ----------

func (s *Server) handleNetdevList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.net.overview())
}

func (s *Server) handleNetdevGet(w http.ResponseWriter, r *http.Request) {
	v, ok := s.net.device(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleNetdevEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > maxEvents {
		limit = 100
	}
	writeJSON(w, http.StatusOK, s.net.recentEvents(r.URL.Query().Get("device"), limit))
}

// handleNetdevSave dodaje (bez {id}) ili menja uređaj.
func (s *Server) handleNetdevSave(w http.ResponseWriter, r *http.Request) {
	var d NetDevice
	if err := decodeBody(r, &d); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := d.normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	if d.SNMPProfile != "" && !s.net.snmpConfig().hasProfile(d.SNMPProfile) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("SNMP nalog %q ne postoji (SNMP podešavanja)", d.SNMPProfile))
		return
	}
	list := s.net.list()
	id := r.PathValue("id")
	d.ID = id
	if err := s.net.mplsConflict(d); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if slices.ContainsFunc(list, func(x NetDevice) bool { return x.IP == d.IP && x.ID != id }) {
		writeErr(w, http.StatusConflict, fmt.Errorf("uređaj sa adresom %s već postoji", d.IP))
		return
	}
	action := "dodat"
	if id == "" {
		d.ID = newID()
		list = append(list, d)
	} else {
		i := slices.IndexFunc(list, func(x NetDevice) bool { return x.ID == id })
		if i < 0 {
			writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
			return
		}
		d.ID, d.Uplink = id, list[i].Uplink // uplink se bira posebno (handleNetdevUplink)
		list[i] = d
		action = "izmenjen"
	}
	if err := s.net.setDevices(list); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("mrežna oprema: %s %s (%s) – %s", action, d.Name, d.IP, identityFrom(r).Name)
	v, _ := s.net.device(d.ID)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleNetdevDelete(w http.ResponseWriter, r *http.Request) {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	id := r.PathValue("id")
	list := s.net.list()
	i := slices.IndexFunc(list, func(x NetDevice) bool { return x.ID == id })
	if i < 0 {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	d := list[i]
	if err := s.net.setDevices(slices.Delete(list, i, i+1)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("mrežna oprema: obrisan %s (%s) – %s", d.Name, d.IP, identityFrom(r).Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleNetdevImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	in, errs := parseDeviceCSV(req.Text)
	if len(in) == 0 && len(errs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("nema nijednog uređaja za uvoz"))
		return
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	list, res := mergeDevices(s.net.list(), in, newID)
	res.Errors = errs
	if res.Added+res.Updated > 0 {
		if err := s.net.setDevices(list); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.log.Printf("mrežna oprema: uvoz – dodato %d, izmenjeno %d, grešaka %d (%s)", res.Added, res.Updated, len(errs), identityFrom(r).Name)
	writeJSON(w, http.StatusOK, res)
}

// handleNetdevHistory vraća istoriju odziva (series=rtt) ili saobraćaja porta
// (series=if:<ifIndex>). Vrednosti NaN (bez odgovora) postaju null.
func (s *Server) handleNetdevHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dev, ok := s.net.find(id)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	if s.net.hist == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("istorija nije dostupna (pogledajte log konzole)"))
		return
	}
	q := r.URL.Query()
	from, to, agg, err := historyWindow(q, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var key, label string
	switch ser := q.Get("series"); {
	case ser == "rtt" || ser == "":
		key, label = "rtt/"+id, "odziv"
	case ser == "mpls":
		key, label = "mpls/"+id, "MPLS saobracaj"
	case ser == "temp":
		key, label = "temp/"+id, "temperatura"
	case strings.HasPrefix(ser, "if:"):
		n, err := strconv.Atoi(strings.TrimPrefix(ser, "if:"))
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, errors.New("neispravan port"))
			return
		}
		key, label = fmt.Sprintf("if/%s/%d", id, n), fmt.Sprintf("port %d", n)
		if l := strings.TrimSpace(q.Get("label")); l != "" && len(l) <= 64 {
			label = l
		}
	default:
		writeErr(w, http.StatusBadRequest, errors.New("nepoznata serija"))
		return
	}
	pts, err := s.net.hist.Query(key, from, to, agg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if q.Get("format") == "xlsx" {
		historyExport(w, dev.Name, label, seriesKind(key), from, to, agg, pts)
		return
	}
	rows := make([][]any, len(pts))
	for i, p := range pts {
		row := make([]any, 0, len(p.V)+1)
		row = append(row, p.T)
		for _, v := range p.V {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				row = append(row, nil)
			} else {
				row = append(row, math.Round(v*100)/100)
			}
		}
		rows[i] = row
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from.Unix(), "to": to.Unix(), "agg": agg, "points": rows})
}

// ---------- SNMP podešavanja ----------

// snmpSettingsView je prikaz SNMP podešavanja: lozinke se ne vraćaju, već
// samo informacija da su postavljene.
type snmpSettingsView struct {
	Version   string `json:"version"`
	User      string `json:"user"`
	AuthProto string `json:"auth_proto"`
	PrivProto string `json:"priv_proto"`
	Port      int    `json:"port"`
	Interval  int    `json:"interval_sec"`
	Timeout   int    `json:"timeout_sec"`

	CommunitySet bool `json:"community_set"`
	AuthPassSet  bool `json:"auth_pass_set"`
	PrivPassSet  bool `json:"priv_pass_set"`
	// Samo pri snimanju: prazno/nil = bez promene.
	Community string  `json:"community,omitempty"`
	AuthPass  *string `json:"auth_pass,omitempty"`
	PrivPass  *string `json:"priv_pass,omitempty"`

	// Profiles: pri snimanju nil (izostavljeno) = bez promene.
	Profiles *[]snmpProfileView `json:"profiles"`
}

// snmpProfileView: dodatni nalog za prikaz (lozinke se ne vraćaju).
type snmpProfileView struct {
	Name         string  `json:"name"`
	Version      string  `json:"version"`
	User         string  `json:"user"`
	AuthProto    string  `json:"auth_proto"`
	PrivProto    string  `json:"priv_proto"`
	Devices      int     `json:"devices"` // koliko uređaja ga koristi (samo prikaz)
	CommunitySet bool    `json:"community_set"`
	AuthPassSet  bool    `json:"auth_pass_set"`
	PrivPassSet  bool    `json:"priv_pass_set"`
	Community    string  `json:"community,omitempty"`
	AuthPass     *string `json:"auth_pass,omitempty"`
	PrivPass     *string `json:"priv_pass,omitempty"`
}

func snmpResponse(c *SNMPConfig) snmpSettingsView {
	if c == nil {
		return snmpSettingsView{Version: "v3", AuthProto: "SHA", PrivProto: "AES", Port: 161, Interval: 60, Timeout: 3}
	}
	v := snmpSettingsView{
		Version: c.Version, User: c.User, AuthProto: c.AuthProto, PrivProto: c.PrivProto,
		Port: c.Port, Interval: c.Interval, Timeout: c.Timeout,
		CommunitySet: c.Community != "", AuthPassSet: c.AuthPass != "", PrivPassSet: c.PrivPass != "",
	}
	list := []snmpProfileView{}
	v.Profiles = &list
	for _, p := range c.Profiles {
		list = append(list, snmpProfileView{Name: p.Name, Version: p.Version, User: p.User, AuthProto: p.AuthProto, PrivProto: p.PrivProto,
			CommunitySet: p.Community != "", AuthPassSet: p.AuthPass != "", PrivPassSet: p.PrivPass != ""})
	}
	*v.Profiles = list
	return v
}

func derefProfiles(p *[]snmpProfileView) []snmpProfileView {
	if p == nil {
		return nil
	}
	return *p
}

func (s *Server) handleGetSNMP(w http.ResponseWriter, r *http.Request) {
	v := snmpResponse(s.net.snmpConfig())
	if v.Profiles != nil {
		for i := range *v.Profiles {
			for _, d := range s.net.list() {
				if d.SNMPProfile == (*v.Profiles)[i].Name {
					(*v.Profiles)[i].Devices++
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSetSNMP(w http.ResponseWriter, r *http.Request) {
	var v snmpSettingsView
	if err := decodeBody(r, &v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	c := SNMPConfig{}
	if old := s.net.snmpConfig(); old != nil {
		c = *old
	}
	c.Version, c.User = v.Version, strings.TrimSpace(v.User)
	c.AuthProto, c.PrivProto = v.AuthProto, v.PrivProto
	c.Port, c.Interval, c.Timeout = v.Port, v.Interval, v.Timeout
	if v.Community != "" {
		c.Community = v.Community
	}
	if v.AuthPass != nil {
		c.AuthPass = *v.AuthPass
	}
	if v.PrivPass != nil {
		c.PrivPass = *v.PrivPass
	}
	// Dodatni nalozi: lozinke koje nisu poslate ostaju kao ranije (po nazivu).
	old := map[string]SNMPProfile{}
	for _, p := range c.Profiles {
		old[p.Name] = p
	}
	if v.Profiles != nil {
		c.Profiles = nil
	}
	for _, pv := range derefProfiles(v.Profiles) {
		p := old[strings.TrimSpace(pv.Name)]
		p.Name, p.Version, p.User = strings.TrimSpace(pv.Name), pv.Version, strings.TrimSpace(pv.User)
		p.AuthProto, p.PrivProto = pv.AuthProto, pv.PrivProto
		if pv.Community != "" {
			p.Community = pv.Community
		}
		if pv.AuthPass != nil {
			p.AuthPass = *pv.AuthPass
		}
		if pv.PrivPass != nil {
			p.PrivPass = *pv.PrivPass
		}
		c.Profiles = append(c.Profiles, p)
	}
	// Nalog koji koristi neki uređaj ne može da se ukloni.
	for _, d := range s.net.list() {
		if d.SNMPProfile != "" && !c.hasProfile(d.SNMPProfile) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("SNMP nalog %q koristi uređaj %s – prvo mu izaberite drugi nalog", d.SNMPProfile, d.Name))
			return
		}
	}
	if err := c.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.persist(func(fc *FileConfig) { cp := c; fc.SNMP = &cp }); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.net.setSNMP(&c)
	s.log.Printf("SNMP podešavanja promenjena (%s, korisnik %q) – %s", c.Version, c.User, identityFrom(r).Name)
	writeJSON(w, http.StatusOK, snmpResponse(&c))
}

// handleSNMPTest odmah čita jedan uređaj i vraća ishod (za proveru podešavanja).
func (s *Server) handleSNMPTest(w http.ResponseWriter, r *http.Request) {
	d, ok := s.net.find(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	cfg := s.net.snmpConfig()
	if !cfg.configured() {
		writeErr(w, http.StatusBadRequest, errors.New("SNMP nije podešen (dugme „SNMP podešavanja”)"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	dc := cfg.forDevice(d.SNMPProfile)
	cfg = &dc
	sys, ports, err := s.net.readDevice(ctx, *cfg, d.IP, true)
	res := map[string]any{"ms": time.Since(start).Milliseconds()}
	if err != nil {
		res["ok"], res["error"] = false, snmpErrorText(err)
	} else {
		res["ok"], res["system"], res["ports"] = true, sys, len(ports)
		// Susedi (CDP/LLDP) i MAC tabela (koji uređaj je na kom portu).
		nbCount := 0
		for _, l := range filterNeighbors(s.net.readNeighbors(ctx, *cfg, d.IP, ports), sys.SysName) {
			nbCount += len(l)
		}
		res["neighbors"] = nbCount
		if d.Type == wlcType {
			// WLC: umesto MAC tabele, Wi-Fi klijenti.
			if cl, aps, err := s.net.readWLC(ctx, *cfg, d.IP); err != nil {
				res["wifi_error"] = snmpErrorText(err)
			} else {
				res["wifi_clients"], res["wifi_aps"] = len(cl), aps
			}
		} else if skipFDB(d) {
			res["fdb_skipped"] = true
		} else if fdb, err := s.net.readFDB(ctx, *cfg, d.IP); err != nil {
			res["fdb_error"] = snmpErrorText(err)
		} else {
			n := 0
			for _, macs := range fdb.ports {
				n += len(macs)
			}
			res["fdb_method"], res["fdb_macs"] = fdb.method, n
		}
		if d.SNMP && d.Active {
			go s.net.pollSNMP(*cfg, d) // osveži prikaz odmah
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// handleNetdevPing: konzola pinguje uređaj (isto kao dugme „Ping” kod računara).
func (s *Server) handleNetdevPing(w http.ResponseWriter, r *http.Request) {
	req, ok := s.pingRequest(w, r)
	if !ok {
		return
	}
	d, found := s.net.find(r.PathValue("id"))
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("uređaj nije pronađen"))
		return
	}
	s.consolePing(w, r, req, consolePingTarget{ID: netdevPrefix + d.ID, Host: d.Name, IP: d.IP, Device: true})
}

// agentMACs vraća MAC adrese mrežnih kartica svih poznatih računara (agenata)
// – za prikaz koji računar je priključen na koji port switch-a.
func (s *Server) agentMACs() map[string]attachedDev {
	out := map[string]attachedDev{}
	for _, a := range s.hub.Agents() {
		for _, ifc := range a.Interfaces {
			mac := strings.ToLower(ifc.MAC)
			if len(mac) == 17 && mac != "00:00:00:00:00:00" {
				out[mac] = attachedDev{Name: a.Hostname, AgentID: a.AgentID}
			}
		}
	}
	return out
}
