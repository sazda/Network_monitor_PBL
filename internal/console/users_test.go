package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUserRoles(t *testing.T) {
	s, fc := newTestServer(t)
	h := s.Handler()
	const ip = "10.0.0.5:1"
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	adm := []string{"admin", "admin-lozinka"}

	// Administrator dodaje operatera.
	if w := do(h, "POST", "/api/users", ip, userRequest{Name: "pera", Password: "pera-lozinka", Role: RoleOperator}, adm...); w.Code != http.StatusCreated {
		t.Fatalf("dodavanje: %d %s", w.Code, w.Body.String())
	}
	// Duplikat (i sa drugačijim velikim slovima) i ime glavnog administratora.
	for _, name := range []string{"Pera", "admin"} {
		if w := do(h, "POST", "/api/users", ip, userRequest{Name: name, Password: "xxxxxxxx"}, adm...); w.Code != http.StatusConflict {
			t.Fatalf("duplikat %q: %d", name, w.Code)
		}
	}
	op := []string{"pera", "pera-lozinka"}

	// Operater: pregled i merenja da, podešavanja i korisnici ne.
	w := do(h, "GET", "/api/me", ip, nil, op...)
	var me Identity
	json.Unmarshal(w.Body.Bytes(), &me)
	if me.Name != "pera" || me.Role != RoleOperator || me.Primary {
		t.Fatalf("me: %s", w.Body.String())
	}
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", "/api/agents", nil, 200},
		{"GET", "/api/results", nil, 200},
		{"GET", "/", nil, 200},
		{"POST", "/api/agents/nepostojeci/latency", map[string]any{}, http.StatusConflict}, // dozvoljeno, agent nije na vezi
		{"GET", "/api/settings", nil, http.StatusForbidden},
		{"POST", "/api/settings/certificate", map[string]any{}, http.StatusForbidden},
		{"GET", "/api/users", nil, http.StatusForbidden},
		{"POST", "/api/users", userRequest{Name: "x", Password: "xxxxxxxx"}, http.StatusForbidden},
	} {
		if w := do(h, c.method, c.path, ip, c.body, op...); w.Code != c.want {
			t.Errorf("operater %s %s: %d, očekivano %d", c.method, c.path, w.Code, c.want)
		}
	}
	if w := doDelete(h, "/api/agents/x", op...); w.Code != http.StatusForbidden {
		t.Errorf("operater briše agenta: %d", w.Code)
	}

	// Rezultat beleži ko je pokrenuo merenje.
	w = do(h, "GET", "/api/results", ip, nil, op...)
	var res []Result
	json.Unmarshal(w.Body.Bytes(), &res)
	if len(res) == 0 || res[0].User != "pera" {
		t.Fatalf("korisnik nije zabeležen u rezultatu: %s", w.Body.String())
	}

	// Operater menja svoju lozinku.
	if w := do(h, "POST", "/api/settings/password", ip, passwordRequest{Current: "pera-lozinka", NewPassword: "nova-lozinka"}, op...); w.Code != 200 {
		t.Fatalf("promena sopstvene lozinke: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/agents", ip, nil, op...); w.Code != 401 {
		t.Fatal("stara lozinka operatera i dalje radi")
	}
	op = []string{"pera", "nova-lozinka"}
	if w := do(h, "GET", "/api/agents", ip, nil, op...); w.Code != 200 {
		t.Fatal("nova lozinka operatera ne radi")
	}
	// Glavni administrator i dalje radi sa svojom lozinkom.
	if w := do(h, "GET", "/api/settings", ip, nil, adm...); w.Code != 200 {
		t.Fatal("administrator ne može da pristupi podešavanjima")
	}

	// Administrator unapređuje operatera i resetuje mu lozinku.
	if w := do(h, "POST", "/api/users/pera", ip, userRequest{Role: RoleAdmin, Password: "reset-lozinka"}, adm...); w.Code != 200 {
		t.Fatalf("izmena korisnika: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/settings", ip, nil, "pera", "reset-lozinka"); w.Code != 200 {
		t.Fatalf("unapređeni korisnik nema administratorski pristup: %d", w.Code)
	}
	// Ne može sam sebe da obriše niti da sebi oduzme prava; ne može da dira glavnog administratora.
	if w := doDelete(h, "/api/users/pera", "pera", "reset-lozinka"); w.Code != http.StatusConflict {
		t.Errorf("brisanje sebe: %d", w.Code)
	}
	if w := do(h, "POST", "/api/users/pera", ip, userRequest{Role: RoleOperator}, "pera", "reset-lozinka"); w.Code != http.StatusConflict {
		t.Errorf("oduzimanje sopstvenih prava: %d", w.Code)
	}
	if w := doDelete(h, "/api/users/admin", "pera", "reset-lozinka"); w.Code != http.StatusForbidden {
		t.Errorf("brisanje glavnog administratora: %d", w.Code)
	}

	// Korisnici su snimljeni u konfiguraciju i preživljavaju restart.
	fc2, _ := LoadFileConfig(fc.Path())
	if len(fc2.Users) != 1 || fc2.Users[0].Name != "pera" || fc2.Users[0].Role != RoleAdmin {
		t.Fatalf("korisnici nisu snimljeni: %+v", fc2.Users)
	}
	if _, err := validateUsers(fc2.AdminUser, fc2.Users); err != nil {
		t.Fatal(err)
	}

	// Brisanje.
	if w := doDelete(h, "/api/users/pera", adm...); w.Code != http.StatusNoContent {
		t.Fatalf("brisanje: %d", w.Code)
	}
	if w := do(h, "GET", "/api/agents", ip, nil, "pera", "reset-lozinka"); w.Code != 401 {
		t.Fatal("obrisani korisnik i dalje može da se prijavi")
	}
}

func doDelete(h http.Handler, path string, auth ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("DELETE", path, nil)
	req.RemoteAddr = "10.0.0.5:1"
	req.Header.Set("X-Requested-With", "test")
	req.SetBasicAuth(auth[0], auth[1])
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
