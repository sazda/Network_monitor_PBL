//go:build linux

package nettest

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"strings"
)

func defaultGateway() (string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer f.Close()
	return parseProcNetRoute(bufio.NewScanner(f))
}

// parseProcNetRoute traži rutu 0.0.0.0/0 sa najmanjom metrikom.
func parseProcNetRoute(sc *bufio.Scanner) (string, error) {
	best, bestMetric := "", -1
	sc.Scan() // zaglavlje
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		gw, err := hex.DecodeString(f[2])
		if err != nil || len(gw) != 4 {
			continue
		}
		// Vrednosti su u redosledu bajtova hosta (little-endian).
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, binary.LittleEndian.Uint32(gw))
		if ip.IsUnspecified() {
			continue
		}
		metric := 0
		for _, c := range f[6] {
			metric = metric*10 + int(c-'0')
		}
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = ip.String(), metric
		}
	}
	if best == "" {
		return "", ErrNoGateway
	}
	return best, nil
}
