package retention

import (
	"fmt"
	"testing"
	"time"
)

func daily(n int) []Item {
	base := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	var items []Item
	for i := 0; i < n; i++ {
		items = append(items, Item{ID: fmt.Sprint(i), Time: base.AddDate(0, 0, -i)})
	}
	return items
}

func kept(ds []Decision) int {
	n := 0
	for _, d := range ds {
		if d.Keep {
			n++
		}
	}
	return n
}

func TestDailyWeekly(t *testing.T) {
	ds := Apply(daily(60), Policy{KeepDaily: 7, KeepWeekly: 4}, time.UTC)
	// 7 dailies (Oct 14-20); weekly adds the newest of ISO weeks 41 and 40.
	if k := kept(ds); k != 9 {
		t.Fatalf("kept %d, want 9", k)
	}
}

func TestEmptyPolicyKeepsAll(t *testing.T) {
	if k := kept(Apply(daily(5), Policy{}, time.UTC)); k != 5 {
		t.Fatalf("kept %d", k)
	}
}

func TestOldestKeptWhenBucketsUnfilled(t *testing.T) {
	ds := Apply(daily(3), Policy{KeepMonthly: 6}, time.UTC)
	// all 3 in same month: newest kept as monthly, oldest kept as "oldest"
	if !ds[0].Keep || !ds[2].Keep || ds[1].Keep {
		t.Fatalf("unexpected: %+v", ds)
	}
}

func TestLastVerifiedAlwaysKept(t *testing.T) {
	items := daily(30)
	items[20].Verified = true
	ds := Apply(items, Policy{KeepLast: 3}, time.UTC)
	for _, d := range ds {
		if d.ID == "20" && !d.Keep {
			t.Fatal("last verified snapshot was forgotten")
		}
	}
	if k := kept(ds); k != 4 {
		t.Fatalf("kept %d, want 4", k)
	}
}
