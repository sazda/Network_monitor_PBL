package console

import "testing"

func TestPickPeerAddr(t *testing.T) {
	cases := []struct {
		client, server []string
		seen           string
		want           string
	}{
		{[]string{"192.168.1.10/24"}, []string{"10.0.0.5/8", "192.168.1.20/24"}, "", "192.168.1.20"},
		{[]string{"192.168.1.10/24", "fd00::10/64"}, []string{"fd00::20/64", "192.168.1.20/24"}, "", "192.168.1.20"},
		{[]string{"fd00::10/64"}, []string{"fd00::20/64"}, "", "fd00::20"},
		{[]string{"192.168.1.10/24"}, []string{"192.168.2.20/24"}, "", "192.168.2.20"}, // drugi VLAN
		{[]string{"10.1.1.10/24"}, []string{"203.0.113.5/24", "10.2.2.20/24"}, "", "10.2.2.20"},
		{[]string{"10.1.1.10/24"}, []string{"203.0.113.5/24"}, "", ""},
		{[]string{"192.168.1.10/24"}, []string{"192.168.1.10/24"}, "", ""}, // isti računar
		// različiti VLAN-ovi, server ima i virtuelni adapter: bira se adresa
		// sa koje se server povezao na konzolu
		{[]string{"10.1.1.10/24"}, []string{"172.20.0.1/20", "10.2.2.20/24"}, "10.2.2.20", "10.2.2.20"},
	}
	for _, c := range cases {
		if got := pickPeerAddr(c.client, c.server, c.seen); got != c.want {
			t.Errorf("pickPeerAddr(%v, %v) = %q, želeli %q", c.client, c.server, got, c.want)
		}
	}
}
