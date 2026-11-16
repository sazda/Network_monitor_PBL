//go:build linux

package nettest

import (
	"bufio"
	"strings"
	"testing"
)

func TestParseProcNetRoute(t *testing.T) {
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	600	00000000	0	0	0
eth1	00000000	FE0010AC	0003	0	0	100	00000000	0	0	0
eth0	0001A8C0	00000000	0001	0	0	600	00FFFFFF	0	0	0
`
	got, err := parseProcNetRoute(bufio.NewScanner(strings.NewReader(table)))
	if err != nil || got != "172.16.0.254" {
		t.Fatalf("dobijeno %q, %v", got, err)
	}
}
