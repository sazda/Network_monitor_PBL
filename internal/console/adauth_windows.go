//go:build windows

package console

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procLogonUserW = windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW")

const (
	logon32LogonNetwork    = 3
	logon32ProviderDefault = 0
)

// domainLogon proverava nalog i lozinku kod kontrolera domena (mrežna
// prijava, kao pri pristupu deljenom folderu); token se odmah zatvara.
func domainLogon(user, domain, pass string) error {
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return &adError{Msg: "neispravan nalog"}
	}
	var d *uint16
	if domain != "" {
		if d, err = windows.UTF16PtrFromString(domain); err != nil {
			return &adError{Msg: "neispravan domen"}
		}
	}
	p, err := windows.UTF16PtrFromString(pass)
	if err != nil {
		return &adError{Msg: "neispravna lozinka"}
	}
	var tok windows.Handle
	r, _, e := procLogonUserW.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(p)),
		logon32LogonNetwork, logon32ProviderDefault, uintptr(unsafe.Pointer(&tok)))
	if r != 0 {
		windows.CloseHandle(tok)
		return nil
	}
	errno, _ := e.(syscall.Errno)
	switch errno {
	// Lozinka je ispravna, ali prijava na ovaj server nije dozvoljena
	// (pravo prijave preko mreže, vreme prijave, dozvoljeni računari): za
	// switch-eve to nije bitno.
	case windows.ERROR_LOGON_TYPE_NOT_GRANTED, windows.ERROR_ACCOUNT_RESTRICTION, windows.ERROR_INVALID_LOGON_HOURS:
		return nil
	case windows.ERROR_LOGON_FAILURE:
		return &adError{Msg: "domen je odbio prijavu: pogrešan nalog ili lozinka (switch-evi nisu kontaktirani)"}
	case windows.ERROR_ACCOUNT_LOCKED_OUT:
		return &adError{Msg: "domenski nalog je zaključan"}
	case windows.ERROR_PASSWORD_EXPIRED, windows.ERROR_PASSWORD_MUST_CHANGE:
		return &adError{Msg: "lozinka domenskog naloga je istekla – promenite je pa pokušajte ponovo"}
	case windows.ERROR_ACCOUNT_DISABLED:
		return &adError{Msg: "domenski nalog je onemogućen"}
	case windows.ERROR_ACCOUNT_EXPIRED:
		return &adError{Msg: "domenski nalog je istekao"}
	case windows.ERROR_NO_LOGON_SERVERS:
		return &adError{Msg: "kontroler domena nije dostupan sa servera konzole – pokušajte kasnije", Unavailable: true}
	}
	return &adError{Msg: fmt.Sprintf("provera naloga u domenu nije uspela: %v", e), Unavailable: true}
}
