package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/protocol"
)

// Restore tests of second copies: a copy that has never been restored is only
// a hope. Agents run them as ordinary restore tests pointed at the second
// storage, so they need no upgrade; the server keeps them apart, and they say
// nothing about the item's main status, recovery goals or retention.

const (
	copyDrillKind = "copy-drill"
	copyTestEvery = 30 * 24 * time.Hour
)

// copyDrillParams pins what a job tests when it's queued, so a copy that lands
// while the test runs can't change what its proof has to match.
type copyDrillParams struct {
	RepoID     int64  `json:"repoId"`
	SnapshotID string `json:"snapshotId"`
	Root       string `json:"root"`
}

func copyTestKey(sourceID int64) string {
	return "copy_test_at:" + strconv.FormatInt(sourceID, 10)
}

// queueCopyDrill queues a restore test of the newest copy in the item's
// current second storage.
func (s *Server) queueCopyDrill(src *Source, trigger string, now time.Time) (int64, error) {
	if src.CopyRepoID == nil {
		return 0, errors.New("this item has no second storage")
	}
	repo, _, err := s.store.Repository(*src.CopyRepoID)
	if err != nil {
		return 0, err
	}
	notCopied := errors.New("nothing has been copied to the second storage yet")
	last := s.store.LastProof(src.ID, "copy", true)
	if last == nil || repo.RepoID == "" {
		return 0, notCopied
	}
	var pred struct {
		RepoID string `json:"repoId"`
	}
	// The newest copy may be in a storage this item no longer uses.
	if err := json.Unmarshal(last.Predicate, &pred); err != nil || pred.RepoID != repo.RepoID {
		return 0, notCopied
	}
	snapshotID, root, err := s.proofRoot(last)
	if err != nil {
		return 0, err
	}
	id, err := s.store.EnqueueJobParams(copyDrillKind, src.ID, src.DrillAgent(), trigger, copyDrillParams{*src.CopyRepoID, snapshotID, root})
	if err != nil {
		return 0, err
	}
	s.wake(src.DrillAgent())
	if err := s.store.SetSetting(copyTestKey(src.ID), now.UTC().Format(time.RFC3339Nano)); err != nil {
		s.log.Printf("restore test of the copy of %s: %v", src.Name, err)
	}
	return id, nil
}

// scheduleCopyTests restore-tests each second copy once every 30 days. That's
// less often than the main restore tests because each one downloads the whole
// backup from the second storage.
func (s *Server) scheduleCopyTests(now time.Time, sources []Source) {
	for i := range sources {
		src := &sources[i]
		if !src.Enabled || src.CopyRepoID == nil {
			continue
		}
		last, err := time.Parse(time.RFC3339Nano, s.store.Setting(copyTestKey(src.ID)))
		if err == nil && now.Sub(last) < copyTestEvery {
			continue
		}
		if _, err := s.queueCopyDrill(src, "schedule", now); err == nil {
			s.log.Printf("scheduled a restore test of the second copy of %s", src.Name)
		}
	}
}

func (s *Server) copyDrillParams(jobID int64) (copyDrillParams, error) {
	var p copyDrillParams
	raw, err := s.store.JobParams(jobID)
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(raw, &p)
}

// copyDrillLease points a restore test at the second storage.
func (s *Server) copyDrillLease(job *Job, src *Source, lease *protocol.Lease) error {
	p, err := s.copyDrillParams(job.ID)
	if err != nil {
		return err
	}
	if src.CopyRepoID == nil || *src.CopyRepoID != p.RepoID {
		return errors.New("this item's second storage changed after the test was queued")
	}
	repo, sec, err := s.store.Repository(p.RepoID)
	if err != nil {
		return err
	}
	lease.Kind, lease.SnapshotID, lease.ExpectedRoot = "drill", p.SnapshotID, p.Root
	lease.Source.Drill.AppTest = nil // this test is about the second storage, not the app
	lease.Repository, lease.RepoID, lease.Password, lease.Creds = repo.Backend, repo.ID, sec.Password, sec.Credentials
	return nil
}

// checkCopyDrill makes sure a proof describes the copy this job was given.
func (s *Server) checkCopyDrill(job *Job, st *proof.Statement, snapshotID string) (copyDrillParams, error) {
	p, err := s.copyDrillParams(job.ID)
	if err != nil {
		return p, err
	}
	var pred struct {
		RepoID       string `json:"repoId"`
		ExpectedRoot string `json:"expectedRoot"`
	}
	if err := json.Unmarshal(st.Predicate, &pred); err != nil {
		return p, err
	}
	repo, _, err := s.store.Repository(p.RepoID)
	if err != nil {
		return p, err
	}
	if snapshotID != p.SnapshotID || pred.ExpectedRoot != p.Root || pred.RepoID != repo.RepoID {
		return p, errors.New("the restore test doesn't describe the copy it was asked to test")
	}
	return p, nil
}

// copyDrillOutcome alerts on a failed test of a copy, or closes the alert.
func (s *Server) copyDrillOutcome(src *Source, passed bool, checks []proof.Check) {
	id := src.ID
	switch {
	case passed:
		s.resolve("copy-drill-failed", &id, nil)
	case storageOnlyFailure(checks):
		s.alert("copy-drill-failed", &id, nil, fmt.Sprintf("%s: the restore test of the second copy couldn't read part of it from storage (the storage service kept returning errors). Nothing was found damaged; it runs again within the hour.", src.Name))
		again := time.Now().Add(-copyTestEvery + time.Hour).UTC().Format(time.RFC3339Nano)
		_ = s.store.SetSetting(copyTestKey(src.ID), again)
	default:
		s.alert("copy-drill-failed", &id, nil, fmt.Sprintf("%s: the restore test of the second copy FAILED. That copy may not be usable, so don't rely on it. Your main backups aren't affected by this result.", src.Name))
	}
}
