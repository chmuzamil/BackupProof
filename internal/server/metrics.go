package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/engine"
)

// GET /metrics: the Prometheus text format, for any token or account that can
// read the dashboard. Something not measured yet has no sample, rather than a
// zero that would read as "very old" or "failed".

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

type metric struct {
	name, kind, help string
	samples          []string
}

// add records a sample; labels come in name, value pairs.
func (m *metric) add(value float64, labels ...string) {
	var b strings.Builder
	b.WriteString(m.name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i] + `="` + labelEscaper.Replace(labels[i+1]) + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	m.samples = append(m.samples, b.String())
}

func (m *metric) write(b *strings.Builder) {
	if len(m.samples) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)
	for _, s := range m.samples {
		b.WriteString(s + "\n")
	}
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// proofUnix is when a proof was recorded, if there is one.
func proofUnix(p *ProofRow) (float64, bool) {
	if p == nil {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, p.Created)
	if err != nil {
		return 0, false
	}
	return unixSeconds(t), true
}

// RestoreTestCounts is the number of restore tests per item and result.
func (s *Store) RestoreTestCounts() (map[string][2]int64, error) {
	rows, err := s.db.Query(`SELECT s.name, p.passed, COUNT(*) FROM proofs p JOIN sources s ON s.id=p.source_id WHERE p.kind='drill' GROUP BY s.name, p.passed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int64{} // name → [failed, passed]
	for rows.Next() {
		var name string
		var passed bool
		var n int64
		if err := rows.Scan(&name, &passed, &n); err != nil {
			return nil, err
		}
		c := out[name]
		if passed {
			c[1] = n
		} else {
			c[0] = n
		}
		out[name] = c
	}
	return out, rows.Err()
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	sources, err := s.store.Sources()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	agents, err := s.store.Agents()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	repos, err := s.store.Repositories()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	stats, err := s.store.RepoStats(1)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	counts, err := s.store.RestoreTestCounts()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	alerts, err := s.store.Alerts(true, 100000)
	if err != nil {
		writeErr(w, 500, err)
		return
	}

	const prefix = "backupproof_"
	gauge := func(name, help string) *metric { return &metric{name: prefix + name, kind: "gauge", help: help} }
	var (
		build      = gauge("build_info", "The running version.")
		enabled    = gauge("source_enabled", "1 if the item runs on schedule.")
		status     = gauge("source_status", "1 for the item's current status.")
		backup     = gauge("last_backup_timestamp_seconds", "When the last successful backup finished.")
		test       = gauge("last_restore_test_timestamp_seconds", "When the last restore test finished.")
		testOK     = gauge("last_restore_test_success", "1 if the last restore test passed.")
		point      = gauge("verified_recovery_point_timestamp_seconds", "When the newest backup that passed a restore test was taken.")
		restore    = gauge("last_restore_duration_seconds", "How long the last passed restore test took, checks included.")
		copyTest   = gauge("last_copy_restore_test_timestamp_seconds", "When the second copy was last restore-tested.")
		copyTestOK = gauge("last_copy_restore_test_success", "1 if the last restore test of the second copy passed.")
		rpo        = gauge("rpo_target_seconds", "The item's data-loss limit, if set.")
		rto        = gauge("rto_target_seconds", "The item's restore-time limit, if set.")
		tests      = &metric{name: prefix + "restore_tests_total", kind: "counter", help: "Restore tests recorded, by result."}
		agentUp    = gauge("agent_up", "1 if the server has been in touch recently.")
		agentSeen  = gauge("agent_last_seen_timestamp_seconds", "When the server was last in touch.")
		repoBytes  = gauge("repository_stored_bytes", "Stored size at the last storage check.")
		openAlerts = gauge("open_alerts", "Open alerts, by kind.")
	)

	now := time.Now()
	agentNames, repoNames := s.nameMaps()
	build.add(1, "version", engine.Version)
	for _, src := range sources {
		st := s.sourceStatus(src, agentNames, repoNames)
		name := []string{"source", src.Name}
		enabled.add(boolValue(src.Enabled), name...)
		for _, v := range []string{"proven", "at-risk", "failing", "unproven"} {
			status.add(boolValue(st.Status == v), "source", src.Name, "status", v)
		}
		if t, ok := proofUnix(s.store.LastProof(src.ID, "backup", true)); ok {
			backup.add(t, name...)
		}
		if t, ok := proofUnix(st.LastDrill); ok {
			test.add(t, name...)
			testOK.add(boolValue(st.LastDrill.Passed), name...)
		}
		if t, ok := proofUnix(st.LastCopyTest); ok {
			copyTest.add(t, name...)
			copyTestOK.add(boolValue(st.LastCopyTest.Passed), name...)
		}
		if st.Recovery.VerifiedPoint != nil {
			point.add(unixSeconds(*st.Recovery.VerifiedPoint), name...)
		}
		if st.Recovery.RestoreMs != nil {
			restore.add(float64(*st.Recovery.RestoreMs)/1000, name...)
		}
		if src.RPOTargetHours > 0 {
			rpo.add(float64(src.RPOTargetHours)*3600, name...)
		}
		if src.RTOTargetMinutes > 0 {
			rto.add(float64(src.RTOTargetMinutes)*60, name...)
		}
		if c, ok := counts[src.Name]; ok {
			tests.add(float64(c[1]), "source", src.Name, "result", "passed")
			tests.add(float64(c[0]), "source", src.Name, "result", "failed")
		}
	}
	for _, a := range agents {
		if a.Revoked || a.Removed {
			continue
		}
		// Server names aren't unique, and a repeated series fails the whole scrape.
		labels := []string{"agent", a.Name, "id", strconv.FormatInt(a.ID, 10)}
		agentUp.add(boolValue(a.LastSeen != nil && now.Sub(*a.LastSeen) <= agentOfflineAfter), labels...)
		if a.LastSeen != nil {
			agentSeen.add(unixSeconds(*a.LastSeen), labels...)
		}
	}
	for _, r := range repos {
		if st := stats[r.ID]; len(st) > 0 {
			repoBytes.add(float64(st[0].Bytes), "repository", r.Name)
		}
	}
	byKind := map[string]int{}
	for _, a := range alerts {
		byKind[a.Kind]++
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		openAlerts.add(float64(byKind[k]), "kind", k)
	}

	var b strings.Builder
	for _, m := range []*metric{build, enabled, status, backup, test, testOK, copyTest, copyTestOK, point, restore, rpo, rto, tests, agentUp, agentSeen, repoBytes, openAlerts} {
		m.write(&b)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
