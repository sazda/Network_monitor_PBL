//go:build !windows && !linux

package sysinfo

import (
	"errors"

	"github.com/sazda/network-monitor/internal/protocol"
)

var errUnsupported = errors.New("stanje servera nije podržano na ovom sistemu")

func cpuTimes() (uint64, uint64, error) { return 0, 0, errUnsupported }
func memory() (uint64, uint64, error)   { return 0, 0, errUnsupported }
func uptime() (uint64, error)           { return 0, errUnsupported }
func disks() []protocol.Disk            { return nil }

func processes() (map[procKey]procSample, error) { return nil, errUnsupported }
