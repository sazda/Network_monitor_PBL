//go:build !windows && !linux

package nettest

import (
	"net"
	"os/exec"
	"strings"
)

func defaultGateway() (string, error) {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return "", ErrNoGateway
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && k == "gateway" {
			if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
				return ip.String(), nil
			}
		}
	}
	return "", ErrNoGateway
}
