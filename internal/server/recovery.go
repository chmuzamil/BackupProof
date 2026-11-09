package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Recovery goals: how much could be lost (RPO) and how long a restore takes
// (RTO). Both are measured from passed restore tests, never from backups that
// merely finished. The limits are optional and set per item.

var recoveryMigrations = []string{
	"ALTER TABLE sources ADD COLUMN rpo_target_hours INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE sources ADD COLUMN rto_target_minutes INTEGER NOT NULL DEFAULT 0",
}

const (
	recoveryTests       = 30 // restore tests looked at and shown
	maxRPOTargetHours   = 10 * 365 * 24
	maxRTOTargetMinutes = 365 * 24 * 60

	goalUnset   = "unset"   // no limit set
	goalUnknown = "unknown" // a limit, but nothing measured yet
	goalMet     = "met"
	goalNear    = "near" // within the last fifth of the limit
	goalMissed  = "missed"
)

// Recovery is what the restore tests of an item have shown. Measurements that
// don't exist yet are absent, never zero.
type Recovery struct {
	// VerifiedPoint is when the newest backup that passed a restore test was taken.
	VerifiedPoint *time.Time     `json:"verifiedPoint,omitempty"`
	VerifiedAgeMs *int64         `json:"verifiedAgeMs,omitempty"`
	RestoreMs     *int64         `json:"restoreMs,omitempty"` // the last passed restore test, checks included
	RPOState      string         `json:"rpoState"`
	RTOState      string         `json:"rtoState"`
	History       []RecoveryTest `json:"history,omitempty"` // newest first; only on a single item
}

type RecoveryTest struct {
	Time         string     `json:"time"`
	Passed       bool       `json:"passed"`
	RestoreMs    *int64     `json:"restoreMs,omitempty"`
	SnapshotTime *time.Time `json:"snapshotTime,omitempty"`
}

func validateTargets(src *Source) error {
	if src.RPOTargetHours < 0 || src.RPOTargetHours > maxRPOTargetHours {
		return errors.New("the data-loss limit must be between 0 (no limit) and 87600 hours")
	}
	if src.RTOTargetMinutes < 0 || src.RTOTargetMinutes > maxRTOTargetMinutes {
		return errors.New("the restore-time limit must be between 0 (no limit) and 525600 minutes")
	}
	return nil
}

// RecoveryTests returns the newest restore tests of an item, newest first.
func (s *Store) RecoveryTests(sourceID int64, limit int) ([]ProofRow, error) {
	rows, err := s.db.Query("SELECT "+proofCols+" FROM proofs WHERE source_id=? AND kind='drill' ORDER BY id DESC LIMIT ?", sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProofRow
	for rows.Next() {
		p, err := scanProof(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// snapshotTime reads when the tested backup was taken from the signed statement.
func snapshotTime(p *ProofRow) time.Time {
	var pred struct {
		SnapshotTime time.Time `json:"snapshotTime"`
	}
	_ = json.Unmarshal(p.Predicate, &pred)
	return pred.SnapshotTime
}

func targetState(measured, limit time.Duration) string {
	switch {
	case limit <= 0:
		return goalUnset
	case measured > limit:
		return goalMissed
	case measured >= limit/5*4:
		return goalNear
	}
	return goalMet
}

// noMeasurement is the state of a goal with nothing to compare it to.
func noMeasurement(limit int) string {
	if limit > 0 {
		return goalUnknown
	}
	return goalUnset
}

// recoveryFrom works out the measurements from restore tests, newest first.
func recoveryFrom(src Source, tests []ProofRow, now time.Time) Recovery {
	r := Recovery{RPOState: noMeasurement(src.RPOTargetHours), RTOState: noMeasurement(src.RTOTargetMinutes)}
	for i := range tests {
		p := &tests[i]
		if !p.Passed {
			continue
		}
		if r.RestoreMs == nil {
			r.RestoreMs = p.RTOMs
		}
		// The newest by when it was taken: a manual test of an old backup
		// doesn't make the recovery point older.
		if t := snapshotTime(p); !t.IsZero() && (r.VerifiedPoint == nil || t.After(*r.VerifiedPoint)) {
			r.VerifiedPoint = &t
		}
	}
	if r.VerifiedPoint != nil {
		age := max(now.Sub(*r.VerifiedPoint), 0) // clocks can disagree
		ms := age.Milliseconds()
		r.VerifiedAgeMs = &ms
		r.RPOState = targetState(age, time.Duration(src.RPOTargetHours)*time.Hour)
	}
	if r.RestoreMs != nil {
		r.RTOState = targetState(time.Duration(*r.RestoreMs)*time.Millisecond, time.Duration(src.RTOTargetMinutes)*time.Minute)
	}
	return r
}

func recoveryHistory(tests []ProofRow) []RecoveryTest {
	out := make([]RecoveryTest, 0, len(tests))
	for i := range tests {
		p := &tests[i]
		t := RecoveryTest{Time: p.Created, Passed: p.Passed, RestoreMs: p.RTOMs}
		if st := snapshotTime(p); !st.IsZero() {
			t.SnapshotTime = &st
		}
		out = append(out, t)
	}
	return out
}

func (s *Server) sourceRecovery(src Source, now time.Time) Recovery {
	tests, err := s.store.RecoveryTests(src.ID, recoveryTests)
	if err != nil {
		s.log.Printf("recovery %s: %v", src.Name, err)
	}
	return recoveryFrom(src, s.withLastPass(src.ID, tests), now)
}

// withLastPass adds the newest passing test when the recent ones all failed,
// so a long run of failures doesn't make the last good backup disappear.
func (s *Server) withLastPass(sourceID int64, tests []ProofRow) []ProofRow {
	for i := range tests {
		if tests[i].Passed {
			return tests
		}
	}
	if p := s.store.LastProof(sourceID, "drill", true); p != nil {
		return append(tests[:len(tests):len(tests)], *p)
	}
	return tests
}

// watchRecovery alerts when an item is past a limit its owner set.
func (s *Server) watchRecovery(src Source, now time.Time) {
	id := src.ID
	r := s.sourceRecovery(src, now)
	if r.RPOState == goalMissed {
		s.alert("rpo-missed", &id, nil, fmt.Sprintf("%s: the newest backup that passed a restore test is %s old, over your limit of %s. Newer backups only count once a restore test passes.",
			src.Name, humanDur(time.Duration(*r.VerifiedAgeMs)*time.Millisecond), plural(src.RPOTargetHours, "hour")))
	} else if created, err := time.Parse(time.RFC3339Nano, src.Created); err == nil && r.VerifiedPoint == nil && src.RPOTargetHours > 0 && now.Sub(created) > time.Duration(src.RPOTargetHours)*time.Hour {
		s.alert("rpo-missed", &id, nil, fmt.Sprintf("%s: no backup has passed a restore test yet, and this item is older than your limit of %s.", src.Name, plural(src.RPOTargetHours, "hour")))
	} else {
		s.resolve("rpo-missed", &id, nil)
	}
	if r.RTOState == goalMissed {
		s.alert("rto-missed", &id, nil, fmt.Sprintf("%s: the last passed restore test took %s, over your limit of %s.",
			src.Name, humanDur(time.Duration(*r.RestoreMs)*time.Millisecond), plural(src.RTOTargetMinutes, "minute")))
	} else {
		s.resolve("rto-missed", &id, nil)
	}
}

// handleRecovery returns an item's measurements and restore-test history.
func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	src, err := s.store.Source(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	tests, err := s.store.RecoveryTests(id, recoveryTests)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	rec := recoveryFrom(*src, s.withLastPass(id, tests), time.Now())
	rec.History = recoveryHistory(tests)
	writeJSON(w, 200, rec)
}
