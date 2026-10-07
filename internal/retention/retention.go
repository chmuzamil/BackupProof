// Package retention implements grandfather-father-son snapshot retention
// with restic-compatible semantics plus a proof-aware rule: the newest
// snapshot with a passing restore drill is never forgotten.
package retention

import (
	"fmt"
	"sort"
	"time"
)

type Policy struct {
	KeepLast    int `json:"keepLast,omitempty"`
	KeepHourly  int `json:"keepHourly,omitempty"`
	KeepDaily   int `json:"keepDaily,omitempty"`
	KeepWeekly  int `json:"keepWeekly,omitempty"`
	KeepMonthly int `json:"keepMonthly,omitempty"`
	KeepYearly  int `json:"keepYearly,omitempty"`
	// KeepWithin keeps everything newer than this duration (relative to the newest snapshot).
	KeepWithin Duration `json:"keepWithin,omitempty"`
	// KeepLastVerified keeps the N newest snapshots that have a passing proof (default 1).
	KeepLastVerified int `json:"keepLastVerified,omitempty"`
}

func (p Policy) Empty() bool {
	return p.KeepLast == 0 && p.KeepHourly == 0 && p.KeepDaily == 0 && p.KeepWeekly == 0 &&
		p.KeepMonthly == 0 && p.KeepYearly == 0 && p.KeepWithin == 0
}

type Item struct {
	ID       string
	Time     time.Time
	Verified bool
}

type Decision struct {
	Item
	Keep    bool
	Reasons []string
}

// Apply decides which items to keep. Items may be in any order; the result is
// newest first. A snapshot is kept if any rule keeps it. An empty policy keeps
// everything (forgetting must be opted into).
func Apply(items []Item, p Policy, loc *time.Location) []Decision {
	if loc == nil {
		loc = time.Local
	}
	sorted := append([]Item(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time.After(sorted[j].Time) })
	out := make([]Decision, len(sorted))
	for i, it := range sorted {
		out[i] = Decision{Item: it}
	}
	if p.Empty() {
		for i := range out {
			out[i].Keep = true
			out[i].Reasons = []string{"no retention policy"}
		}
		return out
	}

	keep := func(i int, reason string) {
		out[i].Keep = true
		out[i].Reasons = append(out[i].Reasons, reason)
	}
	for i := 0; i < len(out) && i < p.KeepLast; i++ {
		keep(i, "last")
	}
	if p.KeepWithin > 0 && len(out) > 0 {
		cutoff := out[0].Time.Add(-time.Duration(p.KeepWithin))
		for i := range out {
			if !out[i].Time.Before(cutoff) {
				keep(i, "within "+time.Duration(p.KeepWithin).String())
			}
		}
	}

	buckets := []struct {
		name string
		n    int
		key  func(time.Time) string
	}{
		{"hourly", p.KeepHourly, func(t time.Time) string { return t.Format("2006-01-02T15") }},
		{"daily", p.KeepDaily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{"weekly", p.KeepWeekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) }},
		{"monthly", p.KeepMonthly, func(t time.Time) string { return t.Format("2006-01") }},
		{"yearly", p.KeepYearly, func(t time.Time) string { return t.Format("2006") }},
	}
	for _, b := range buckets {
		if b.n <= 0 {
			continue
		}
		seen := map[string]bool{}
		lastIdx := -1
		for i := range out {
			k := b.key(out[i].Time.In(loc))
			if seen[k] {
				continue
			}
			if len(seen) >= b.n {
				break
			}
			seen[k] = true
			keep(i, b.name+" "+k)
			lastIdx = i
		}
		// restic: if the buckets could not all be filled, keep the oldest too.
		if len(seen) < b.n && lastIdx >= 0 && len(out) > 0 && lastIdx != len(out)-1 {
			keep(len(out)-1, "oldest ("+b.name+")")
		}
	}

	lv := p.KeepLastVerified
	if lv == 0 {
		lv = 1
	}
	for i := range out {
		if lv == 0 {
			break
		}
		if out[i].Verified {
			keep(i, "last verified")
			lv--
		}
	}
	return out
}

// Duration marshals as a Go duration string ("720h") or accepts "30d".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := ParseDuration(s)
	*d = Duration(v)
	return err
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func ParseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	var n int
	var unit string
	if _, err := fmt.Sscanf(s, "%d%s", &n, &unit); err == nil {
		switch unit {
		case "d":
			return time.Duration(n) * 24 * time.Hour, nil
		case "w":
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		case "y":
			return time.Duration(n) * 365 * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}
