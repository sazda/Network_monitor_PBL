package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// req šalje zahtev sa kolačićem sesije (bez Basic prijave).
func reqCookie(h http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.RemoteAddr = "10.0.0.5:1"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("nema kolačića sesije (status %d, %s)", w.Code, w.Body.String())
	return nil
}

func TestLoginLogout(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	// Postavljanje lozinke odmah prijavljuje korisnika.
	w := do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	if c := sessionFrom(t, w); c.HttpOnly != true || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("kolačić nije zaštićen: %+v", c)
	}

	// Neprijavljen: stranica za prijavu za pretraživač, 401 bez Basic izazova za API.
	w = reqCookie(h, "GET", "/", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "login.js") {
		t.Fatalf("stranica za prijavu: %d", w.Code)
	}
	w = reqCookie(h, "GET", "/api/agents", nil, nil)
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("API bez prijave: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}

	// Pogrešna pa ispravna prijava.
	if w := reqCookie(h, "POST", "/api/login", loginRequest{User: "admin", Password: "pogresna"}, nil); w.Code != 401 {
		t.Fatalf("pogrešna lozinka: %d", w.Code)
	}
	w = reqCookie(h, "POST", "/api/login", loginRequest{User: "admin", Password: "admin-lozinka"}, nil)
	cookie := sessionFrom(t, w)
	if w := reqCookie(h, "GET", "/api/settings", nil, cookie); w.Code != 200 {
		t.Fatalf("pristup sa sesijom: %d", w.Code)
	}
	if w := reqCookie(h, "GET", "/", nil, cookie); !strings.Contains(w.Body.String(), "app.js") {
		t.Fatal("prijavljen korisnik ne dobija konzolu")
	}

	// Odjava poništava sesiju.
	if w := reqCookie(h, "POST", "/api/logout", nil, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("odjava: %d", w.Code)
	}
	if w := reqCookie(h, "GET", "/api/settings", nil, cookie); w.Code != 401 {
		t.Fatalf("sesija radi posle odjave: %d", w.Code)
	}

	// Basic prijava i dalje radi za skripte.
	if w := do(h, "GET", "/api/agents", "10.0.0.5:1", nil, "admin", "admin-lozinka"); w.Code != 200 {
		t.Fatalf("Basic prijava: %d", w.Code)
	}
}

func TestSessionInvalidation(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})
	do(h, "POST", "/api/users", "10.0.0.5:1", userRequest{Name: "op", Password: "op-lozinka", Role: RoleOperator}, "admin", "admin-lozinka")

	login := func() *http.Cookie {
		return sessionFrom(t, reqCookie(h, "POST", "/api/login", loginRequest{User: "op", Password: "op-lozinka"}, nil))
	}

	// Promena uloge važi odmah, bez nove prijave.
	c := login()
	if w := reqCookie(h, "GET", "/api/settings", nil, c); w.Code != http.StatusForbidden {
		t.Fatalf("operater u podešavanjima: %d", w.Code)
	}
	do(h, "POST", "/api/users/op", "10.0.0.5:1", userRequest{Role: RoleAdmin}, "admin", "admin-lozinka")
	if w := reqCookie(h, "GET", "/api/settings", nil, c); w.Code != 200 {
		t.Fatalf("unapređeni korisnik: %d", w.Code)
	}

	// Administrator resetuje lozinku: postojeća sesija prestaje da važi.
	do(h, "POST", "/api/users/op", "10.0.0.5:1", userRequest{Password: "nova-lozinka"}, "admin", "admin-lozinka")
	if w := reqCookie(h, "GET", "/api/agents", nil, c); w.Code != 401 {
		t.Fatalf("sesija posle reseta lozinke: %d", w.Code)
	}

	// Brisanje korisnika poništava njegovu sesiju.
	c = sessionFrom(t, reqCookie(h, "POST", "/api/login", loginRequest{User: "op", Password: "nova-lozinka"}, nil))
	r := httptest.NewRequest("DELETE", "/api/users/op", nil)
	r.Header.Set("X-Requested-With", "x")
	r.SetBasicAuth("admin", "admin-lozinka")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if w := reqCookie(h, "GET", "/api/agents", nil, c); w.Code != 401 {
		t.Fatalf("sesija obrisanog korisnika: %d", w.Code)
	}
}

// Pretraživač koji je ranije (stara verzija konzole) zapamtio Basic prijavu
// šalje je uz svaki zahtev; posle odjave to ne sme ponovo da ga prijavi.
func TestLogoutWithCachedBasicAuth(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	do(h, "POST", "/api/setup", "127.0.0.1:1", setupRequest{User: "admin", Password: "admin-lozinka"})

	browser := func(method, path string, cookie *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "10.0.0.5:1"
		r.SetBasicAuth("admin", "admin-lozinka") // zapamćeno u pretraživaču
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Posle odjave (nema kolačića sesije) pretraživač dobija stranicu za prijavu…
	if w := browser("GET", "/", nil, nil); !strings.Contains(w.Body.String(), "login.js") {
		t.Fatalf("zapamćena Basic prijava je ponovo prijavila pretraživač (status %d)", w.Code)
	}
	// …i API pozivi iz stranice ne prolaze.
	if w := browser("GET", "/api/agents", nil, map[string]string{"X-Requested-With": "fetch"}); w.Code != 401 {
		t.Fatalf("API iz pretraživača sa Basic prijavom: %d", w.Code)
	}
	// Skripte (bez Sec-Fetch-* zaglavlja) i dalje mogu da koriste Basic.
	if w := do(h, "GET", "/api/agents", "10.0.0.5:1", nil, "admin", "admin-lozinka"); w.Code != 200 {
		t.Fatalf("Basic za skripte: %d", w.Code)
	}
}
