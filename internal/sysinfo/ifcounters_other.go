//go:build !windows && !linux

package sysinfo

import "net"

func ifOctets(net.Interface) (uint64, uint64, bool) { return 0, 0, false }
