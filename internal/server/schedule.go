package server

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseSchedule accepts 5-field cron expressions and descriptors such as
// "@daily", "@hourly" or "@every 6h".
// never is the schedule of "manual" sources (e.g. one-off imports).
type never struct{}

func (never) Next(t time.Time) time.Time { return t.AddDate(100, 0, 0) }

func ParseSchedule(expr string) (cron.Schedule, error) {
	if expr == "manual" {
		return never{}, nil
	}
	if expr == "" {
		return nil, fmt.Errorf("schedule is required")
	}
	return cronParser.Parse(expr)
}

// jitter spreads sources sharing a schedule over up to 10 minutes,
// deterministically per source, to avoid thundering herds on storage.
func jitter(name string) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(name))
	return time.Duration(h.Sum32()%600) * time.Second
}

func nextRun(expr, name string, after time.Time) time.Time {
	s, err := ParseSchedule(expr)
	if err != nil {
		return after.Add(24 * time.Hour)
	}
	return s.Next(after).Add(jitter(name))
}

// schedulerLoop enqueues due backups and drills, expires dead leases and
// runs the watchdog.
func (s *Server) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		s.tick(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) tick(now time.Time) {
	expired, err := s.store.ExpireLeases()
	if err != nil {
		s.log.Printf("scheduler: expiring leases: %v", err)
	}
	for _, j := range expired {
		sid := j.SourceID
		s.alert("job-failed", &sid, nil, fmt.Sprintf("A %s stopped because the server running it stopped responding.", plainKind(j.Kind)))
	}
	sources, err := s.store.Sources()
	if err != nil {
		s.log.Printf("scheduler: %v", err)
		return
	}
	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		nb, nd := src.NextBackup, src.NextDrill
		if nb == nil || nd == nil {
			b, d := nextRun(src.BackupCron, src.Name, now), nextRun(src.DrillCron, src.Name+"/drill", now)
			if err := s.store.SetNextRuns(src.ID, b, d); err != nil {
				s.log.Printf("%s: could not save next run times: %v", src.Name, err)
			}
			continue
		}
		changed := false
		b, d := *nb, *nd
		if !now.Before(b) {
			if _, err := s.store.EnqueueJob("backup", src.ID, src.AgentID, "schedule"); err == nil {
				s.log.Printf("scheduled backup for %s", src.Name)
			}
			b, changed = nextRun(src.BackupCron, src.Name, now), true
		}
		if !now.Before(d) {
			if _, err := s.store.EnqueueJob("drill", src.ID, src.DrillAgent(), "schedule"); err == nil {
				s.log.Printf("scheduled restore drill for %s", src.Name)
			}
			d, changed = nextRun(src.DrillCron, src.Name+"/drill", now), true
		}
		if changed {
			if err := s.store.SetNextRuns(src.ID, b, d); err != nil {
				s.log.Printf("%s: could not save next run times: %v", src.Name, err)
			}
		}
	}
	s.watchdog(now, sources)
	s.maybeSendWeeklyReport(now)
	s.heartbeat()
}

// watchdog is the dead-man's switch: it alerts on what did NOT happen —
// overdue backups, stale proofs and silent agents — which job-level failure
// alerts can never catch.
func (s *Server) watchdog(now time.Time, sources []Source) {
	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		id := src.ID
		sched, err := ParseSchedule(src.BackupCron)
		if err != nil || src.BackupCron == "manual" {
			continue
		}
		// Expected interval = distance between two consecutive runs.
		n1 := sched.Next(now)
		interval := sched.Next(n1).Sub(n1)
		grace := interval + interval/2 + 15*time.Minute
		created, _ := time.Parse(time.RFC3339Nano, src.Created)
		last := s.store.LastProof(src.ID, "backup", false)
		var lastAt time.Time
		if last != nil {
			lastAt, _ = time.Parse(time.RFC3339Nano, last.Created)
		}
		if lastAt.IsZero() && now.Sub(created) > grace || !lastAt.IsZero() && now.Sub(lastAt) > grace {
			msg := fmt.Sprintf("%s: no backup for %s (it should run every %s).", src.Name, since(lastAt, created, now), humanDur(interval))
			s.alert("backup-overdue", &id, nil, msg)
		} else {
			s.resolve("backup-overdue", &id, nil)
		}

		maxAge := time.Duration(src.ProofMaxAgeHours) * time.Hour
		lp := s.store.LastProof(src.ID, "drill", true)
		var proofAt time.Time
		if lp != nil {
			proofAt, _ = time.Parse(time.RFC3339Nano, lp.Created)
		}
		if proofAt.IsZero() && now.Sub(created) > maxAge || !proofAt.IsZero() && now.Sub(proofAt) > maxAge {
			s.alert("proof-stale", &id, nil, fmt.Sprintf("%s: not restore-tested for %s (should be at least every %s).", src.Name, since(proofAt, created, now), humanDur(maxAge)))
		} else {
			s.resolve("proof-stale", &id, nil)
		}
	}
	agents, _ := s.store.Agents()
	for _, a := range agents {
		aid := a.ID
		if a.Revoked {
			continue
		}
		if a.LastSeen != nil && now.Sub(*a.LastSeen) > 10*time.Minute {
			s.alert("agent-offline", nil, &aid, fmt.Sprintf("The server '%s' hasn't been in touch since %s. Is it switched on?", a.Name, a.LastSeen.Local().Format("Jan 2, 15:04")))
		} else {
			s.resolve("agent-offline", nil, &aid)
		}
	}
}

func since(last, created, now time.Time) string {
	if last.IsZero() {
		return humanDur(now.Sub(created)) + " (never done)"
	}
	return humanDur(now.Sub(last))
}

func plainKind(k string) string {
	return map[string]string{"backup": "backup", "drill": "restore test", "check": "storage check"}[k]
}
