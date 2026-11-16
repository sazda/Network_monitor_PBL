package console

import (
	"fmt"
	"testing"
)

func TestStoreQuery(t *testing.T) {
	st, err := OpenStore(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	// 25 merenja: a1 = INF-MILOSM, a2 = HRD-SMILJAM, svako treće je LAN test a1 ↔ a2.
	for i := 0; i < 25; i++ {
		r := Result{ID: fmt.Sprint(i), AgentID: "a1", Hostname: "INF-MILOSM", Kind: "latency"}
		if i%2 == 1 {
			r.AgentID, r.Hostname = "a2", "HRD-SMILJAM"
		}
		if i%3 == 0 {
			r.Kind, r.PeerID = "lan", "a3" // a3 nema sačuvan naziv
		}
		st.Add(r)
	}
	names := func(id string) string { return map[string]string{"a3": "FIN-SANDRAA"}[id] }

	page, total := st.Query(ResultQuery{Offset: 20, Limit: 10})
	if total != 25 || len(page) != 5 || page[0].ID != "4" {
		t.Fatalf("strana 3: total=%d len=%d prvi=%v", total, len(page), page)
	}
	page, total = st.Query(ResultQuery{Search: "milos", Limit: 10})
	if total != 13 || len(page) != 10 || page[0].ID != "24" {
		t.Fatalf("pretraga milos: total=%d len=%d", total, len(page))
	}
	// Naziv drugog računara u LAN testu dolazi iz HostName (stariji rezultati).
	_, total = st.Query(ResultQuery{Search: "sandra", Limit: 10, HostName: names})
	if total != 9 {
		t.Fatalf("pretraga po drugom računaru: total=%d, očekivano 9", total)
	}
	_, total = st.Query(ResultQuery{Search: "smiljam sandra", Limit: 10, HostName: names})
	if total != 4 { // i neparno i deljivo sa 3: 3, 9, 15, 21
		t.Fatalf("pretraga sa dve reči: total=%d, očekivano 4", total)
	}
	_, total = st.Query(ResultQuery{AgentID: "a3", Limit: 10})
	if total != 9 {
		t.Fatalf("filter po agentu: total=%d", total)
	}
}
