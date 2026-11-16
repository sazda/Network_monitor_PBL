//go:build linux

package sysinfo

import (
	"net"
	"os"
	"strconv"
	"strings"
)

func ifOctets(ifc net.Interface) (in, out uint64, ok bool) {
	read := func(f string) (uint64, bool) {
		b, err := os.ReadFile("/sys/class/net/" + ifc.Name + "/statistics/" + f)
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return n, err == nil
	}
	in, ok1 := read("rx_bytes")
	out, ok2 := read("tx_bytes")
	return in, out, ok1 && ok2
}
