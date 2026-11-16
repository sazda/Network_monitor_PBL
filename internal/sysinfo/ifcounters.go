package sysinfo

import (
	"net"
	"strings"
)

// IfCounters vraća ukupno primljene i poslate bajtove po mrežnoj kartici
// (ključ je MAC adresa, mala slova) – za saobraćaj računara.
func IfCounters() (map[string][2]uint64, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := map[string][2]uint64{}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) != 6 {
			continue
		}
		in, outB, ok := ifOctets(ifc)
		if ok {
			out[strings.ToLower(ifc.HardwareAddr.String())] = [2]uint64{in, outB}
		}
	}
	return out, nil
}
