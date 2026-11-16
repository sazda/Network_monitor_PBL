package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"time"
)

// Provera domenskog naloga pre prijave na switch-eve (Održavanje): pogrešna
// lozinka tako troši jedan neuspeli pokušaj u AD-u, a ne po jedan za svaki
// switch (zaključavanje naloga). Konzola na Windows-u (član domena) koristi
// LogonUser (mrežna prijava); lozinka se ne čuva. Uspešna provera se pamti
// nekoliko minuta samo kao HMAC naloga i lozinke sa slučajnim ključem koji
// postoji samo u memoriji procesa.

const adOKFor = 5 * time.Minute

// errADUnsupported: provera nije moguća na ovom sistemu (konzola nije na
// Windows-u); radnja se tada izvršava bez nje.
var errADUnsupported = errors.New("provera domenskog naloga nije dostupna na ovom sistemu")

// adError je odbijena provera (poruka za korisnika); Unavailable znači da
// domen nije dostupan (pokušati kasnije), ne da je lozinka pogrešna.
type adError struct {
	Msg         string
	Unavailable bool
}

func (e *adError) Error() string { return e.Msg }

// splitAccount: „FIRMA\korisnik” → (korisnik, FIRMA); „korisnik@firma.local” →
// (korisnik@firma.local, ""). Nalog bez domena se ne prihvata (LogonUser bi
// tada proveravao i lokalne naloge servera).
func splitAccount(acc string) (user, domain string, err error) {
	acc = strings.TrimSpace(acc)
	if d, u, ok := strings.Cut(acc, `\`); ok {
		if d == "" || u == "" || strings.ContainsAny(u, `\@`) {
			return "", "", &adError{Msg: `neispravan nalog: upišite ga kao DOMEN\nalog ili nalog@domen`}
		}
		return u, d, nil
	}
	if u, d, ok := strings.Cut(acc, "@"); ok && u != "" && strings.Contains(d, ".") {
		return acc, "", nil
	}
	return "", "", &adError{Msg: `upišite nalog sa domenom: FIRMA\nalog ili nalog@firma.local`}
}

type adVerifier struct {
	mu   sync.Mutex
	key  []byte
	ok   map[string]time.Time
	now  func() time.Time
	auth func(user, domain, pass string) error // domainLogon; menja se u testovima
}

func newADVerifier() *adVerifier {
	k := make([]byte, 32)
	rand.Read(k)
	return &adVerifier{key: k, ok: map[string]time.Time{}, now: time.Now, auth: domainLogon}
}

func (v *adVerifier) mac(acc, pass string) string {
	h := hmac.New(sha256.New, v.key)
	h.Write([]byte(strings.ToLower(strings.TrimSpace(acc)) + "\x00" + pass))
	return string(h.Sum(nil))
}

// check proverava nalog i lozinku u domenu. Vraća errADUnsupported kada
// provera nije moguća (radnja se nastavlja), *adError kada je odbijena.
func (v *adVerifier) check(acc, pass string) error {
	user, domain, err := splitAccount(acc)
	if err != nil {
		return err
	}
	k := v.mac(acc, pass)
	now := v.now()
	v.mu.Lock()
	for kk, t := range v.ok {
		if now.After(t) {
			delete(v.ok, kk)
		}
	}
	_, cached := v.ok[k]
	v.mu.Unlock()
	if cached {
		return nil
	}
	if err := v.auth(user, domain, pass); err != nil {
		return err
	}
	v.mu.Lock()
	v.ok[k] = now.Add(adOKFor)
	v.mu.Unlock()
	return nil
}
