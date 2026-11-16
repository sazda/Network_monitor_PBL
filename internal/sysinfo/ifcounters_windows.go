//go:build windows

package sysinfo

import (
	"net"

	"golang.org/x/sys/windows"
)

func ifOctets(ifc net.Interface) (in, out uint64, ok bool) {
	var row windows.MibIfRow2
	row.InterfaceIndex = uint32(ifc.Index)
	if windows.GetIfEntry2Ex(windows.MibIfEntryNormal, &row) != nil {
		return 0, 0, false
	}
	return row.InOctets, row.OutOctets, true
}
