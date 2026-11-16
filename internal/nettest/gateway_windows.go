//go:build windows

package nettest

import (
	"encoding/binary"
	"net"
	"unsafe"
)

var procGetBestRoute = iphlpapi.NewProc("GetBestRoute")

func defaultGateway() (string, error) {
	// Pitamo sistem koja ruta bi se koristila za neku adresu van LAN-a; adresa
	// se ne kontaktira. MIB_IPFORWARDROW ima 14 DWORD polja, NextHop je 4.
	var row [14]uint32
	dest := binary.LittleEndian.Uint32(net.IPv4(1, 1, 1, 1).To4())
	r, _, _ := procGetBestRoute.Call(uintptr(dest), 0, uintptr(unsafe.Pointer(&row[0])))
	if r != 0 {
		return "", ErrNoGateway
	}
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, row[3])
	ip := net.IP(b)
	if ip.IsUnspecified() {
		return "", ErrNoGateway
	}
	return ip.String(), nil
}
