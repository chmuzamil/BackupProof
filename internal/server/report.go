package server

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

// The weekly summary: one email a week with what was backed up and
// restore-tested, so people notice the absence of problems too.

type reportRow struct {
	Name, Status, Storage  string
	Backups, BackupsFailed int
	Tests, TestsFailed     int
	LastPass               *time.Time
}

type weeklyReport struct {
	From, To   time.Time
	Rows       []reportRow
	OpenAlerts []string
	Proven     int
}

func (s *Server) buildWeeklyReport(now time.Time) (*weeklyReport, error) {
	rep := &weeklyReport{From: now.Add(-7 * 24 * time.Hour), To: now}
	statuses, err := s.sourceStatuses()
	if err != nil {
		return nil, err
	}
	proofs, err := s.store.Proofs(0, 5000)
	if err != nil {
		return nil, err
	}
	for _, st := range statuses {
		row := reportRow{Name: st.Source.Name, Status: statusWords[st.Status], Storage: st.RepoName}
		if st.Status == "proven" {
			rep.Proven++
		}
		for _, p := range proofs {
			if p.SourceID == nil || *p.SourceID != st.Source.ID {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, p.Created)
			if err != nil || t.Before(rep.From) {
				continue
			}
			switch p.Kind {
			case "backup":
				row.Backups++
				if !p.Passed {
					row.BackupsFailed++
				}
			case "drill":
				row.Tests++
				if !p.Passed {
					row.TestsFailed++
				} else if row.LastPass == nil || t.After(*row.LastPass) {
					tt := t
					row.LastPass = &tt
				}
			}
		}
		rep.Rows = append(rep.Rows, row)
	}
	alerts, err := s.store.Alerts(true, 50)
	if err != nil {
		return nil, err
	}
	for _, a := range alerts {
		rep.OpenAlerts = append(rep.OpenAlerts, a.Message)
	}
	return rep, nil
}

var statusWords = map[string]string{"proven": "Restore tested", "unproven": "Not tested yet", "at-risk": "Needs attention", "failing": "Problem"}

func (r *weeklyReport) subject() string {
	switch {
	case len(r.Rows) == 0:
		return "BackupProof weekly summary: nothing is protected yet"
	case r.Proven == len(r.Rows) && len(r.OpenAlerts) == 0:
		return fmt.Sprintf("BackupProof weekly summary: all %d items are restore-tested", len(r.Rows))
	default:
		return fmt.Sprintf("BackupProof weekly summary: %d of %d items restore-tested, %d open alerts", r.Proven, len(r.Rows), len(r.OpenAlerts))
	}
}

func (r *weeklyReport) text(dashboard string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Week of %s to %s\n\n", r.From.Format("2 Jan"), r.To.Format("2 Jan 2006"))
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "%s — %s\n  %d backups (%d failed), %d restore tests (%d failed)", row.Name, row.Status, row.Backups, row.BackupsFailed, row.Tests, row.TestsFailed)
		if row.LastPass != nil {
			fmt.Fprintf(&b, ", last passed %s", row.LastPass.Local().Format("Mon 2 Jan 15:04"))
		}
		b.WriteString("\n")
	}
	if len(r.OpenAlerts) > 0 {
		b.WriteString("\nNeeds your attention:\n")
		for _, a := range r.OpenAlerts {
			b.WriteString("- " + a + "\n")
		}
	}
	if dashboard != "" {
		b.WriteString("\nOpen the dashboard: " + dashboard + "\n")
	}
	return b.String()
}

func (r *weeklyReport) html(dashboard string) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html><html><body style="margin:0;background:#f6f8fc;font-family:Segoe UI,Helvetica,Arial,sans-serif;color:#17203a">`)
	b.WriteString(`<div style="max-width:640px;margin:0 auto;padding:24px 16px">`)
	b.WriteString(`<p style="margin:0 0 4px;font-weight:700;font-size:18px">Backup<span style="color:#3d6bff">Proof</span></p>`)
	fmt.Fprintf(&b, `<h1 style="font-size:22px;margin:12px 0 4px">%s</h1><p style="color:#56607a;margin:0 0 18px">%s to %s</p>`,
		e(strings.TrimPrefix(r.subject(), "BackupProof weekly summary: ")), e(r.From.Format("2 Jan")), e(r.To.Format("2 Jan 2006")))
	b.WriteString(`<table style="width:100%;border-collapse:collapse;background:#fff;border:1px solid #d9dfec">`)
	b.WriteString(`<tr style="text-align:left;color:#56607a;font-size:13px"><th style="padding:10px">Item</th><th style="padding:10px">Status</th><th style="padding:10px">Backups</th><th style="padding:10px">Restore tests</th></tr>`)
	for _, row := range r.Rows {
		color := "#b3261e"
		if row.Status == "Restore tested" {
			color = "#157a52"
		} else if row.Status == "Not tested yet" {
			color = "#56607a"
		}
		fmt.Fprintf(&b, `<tr style="border-top:1px solid #d9dfec"><td style="padding:10px;font-weight:600">%s</td><td style="padding:10px;color:%s">%s</td><td style="padding:10px">%d%s</td><td style="padding:10px">%d%s</td></tr>`,
			e(row.Name), color, e(row.Status), row.Backups, failed(row.BackupsFailed), row.Tests, failed(row.TestsFailed))
	}
	b.WriteString(`</table>`)
	if len(r.OpenAlerts) > 0 {
		b.WriteString(`<h2 style="font-size:16px;margin:22px 0 8px">Needs your attention</h2><ul style="padding-left:18px">`)
		for _, a := range r.OpenAlerts {
			b.WriteString("<li>" + e(a) + "</li>")
		}
		b.WriteString(`</ul>`)
	}
	if dashboard != "" {
		fmt.Fprintf(&b, `<p style="margin:24px 0"><a href="%s" style="background:#3d6bff;color:#fff;padding:10px 18px;border-radius:999px;text-decoration:none;font-weight:600">Open the dashboard</a></p>`, e(dashboard))
	}
	b.WriteString(`<p style="color:#56607a;font-size:12px">You get this summary because it's turned on in BackupProof's Settings.</p></div></body></html>`)
	return b.String()
}

func failed(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(` <span style="color:#b3261e">(%d failed)</span>`, n)
}

// sendWeeklyReport emails the summary (email only: chat channels get alerts).
func (s *Server) sendWeeklyReport(now time.Time) error {
	n := s.notifySettings()
	if n.SMTPHost == "" || n.To == "" {
		return errors.New("set up email under “Alerts by email or chat” first; the weekly summary is sent by email")
	}
	rep, err := s.buildWeeklyReport(now)
	if err != nil {
		return err
	}
	dash := strings.TrimPrefix(s.dashboardLine(), "\n\nOpen the dashboard: ")
	return sendEmail(n, event{Subject: rep.subject(), Body: rep.text(dash), HTML: rep.html(dash)})
}

// maybeSendWeeklyReport runs on every scheduler tick and sends the summary
// once per week at the chosen day and hour.
func (s *Server) maybeSendWeeklyReport(now time.Time) {
	n := s.notifySettings()
	if !n.WeeklyReport {
		return
	}
	local := now.Local()
	if int(local.Weekday()) != n.ReportDay || local.Hour() != n.ReportHour {
		return
	}
	week := local.Format("2006-01-02")
	if s.store.Setting("report_sent") == week {
		return
	}
	if err := s.store.SetSetting("report_sent", week); err != nil {
		s.log.Printf("weekly summary: %v", err)
		return
	}
	if err := s.sendWeeklyReport(now); err != nil {
		s.log.Printf("weekly summary: %v", err)
	}
}

func (s *Server) handleSendReport(w http.ResponseWriter, r *http.Request) {
	if err := s.sendWeeklyReport(time.Now()); err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) sourceStatuses() ([]SourceStatus, error) {
	sources, err := s.store.Sources()
	if err != nil {
		return nil, err
	}
	agentNames, repoNames := s.nameMaps()
	out := make([]SourceStatus, 0, len(sources))
	for _, src := range sources {
		out = append(out, s.sourceStatus(src, agentNames, repoNames))
	}
	return out, nil
}
