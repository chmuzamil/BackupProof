package proof

import (
	"encoding/json"
	"errors"
	"time"
)

const (
	StatementType   = "https://in-toto.io/Statement/v1"
	PredicateBackup = "https://backupproof.dev/attestation/backup/v1"
	PredicateDrill  = "https://backupproof.dev/attestation/restore-drill/v1"
)

type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type Statement struct {
	Type          string          `json:"_type"`
	Subject       []Subject       `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     json.RawMessage `json:"predicate"`
}

// SnapshotSubject names a snapshot by its repository-scoped ID and commits to
// its unkeyed BLAKE3 Merkle content root.
func SnapshotSubject(repoID, snapshotID, root string) Subject {
	return Subject{
		Name:   "backupproof:" + repoID + "/snapshot/" + snapshotID,
		Digest: map[string]string{"blake3": root},
	}
}

// BackupPredicate states what was captured, from where, and when.
type BackupPredicate struct {
	RepoID     string         `json:"repoId"`
	SnapshotID string         `json:"snapshotId"`
	Source     string         `json:"source"`
	Kind       string         `json:"kind"`
	Host       string         `json:"host"`
	Time       time.Time      `json:"time"`
	Entries    int            `json:"entries"`
	Bytes      int64          `json:"bytes"`
	DurationMs int64          `json:"durationMs"`
	Storage    StorageInfo    `json:"storage"`
	SourceMeta map[string]any `json:"sourceMeta,omitempty"`
	Engine     string         `json:"engine"`
}

type StorageInfo struct {
	Location       string `json:"location"`
	ObjectLockMode string `json:"objectLockMode,omitempty"`
	ObjectLockDays int    `json:"objectLockDays,omitempty"`
}

type Check struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	Detail     string `json:"detail,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// DrillPredicate states that a snapshot was restored from storage into an
// isolated sandbox and what was proven about the result.
type DrillPredicate struct {
	RepoID        string    `json:"repoId"`
	SnapshotID    string    `json:"snapshotId"`
	Source        string    `json:"source"`
	Kind          string    `json:"kind"`
	SnapshotTime  time.Time `json:"snapshotTime"`
	ExpectedRoot  string    `json:"expectedRoot"`
	RestoredRoot  string    `json:"restoredRoot"`
	Verifier      string    `json:"verifier"`
	Sandbox       string    `json:"sandbox"`
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt"`
	RTOMs         int64     `json:"rtoMs"`
	RestoredBytes int64     `json:"restoredBytes"`
	Checks        []Check   `json:"checks"`
	SampleSeed    string    `json:"sampleSeed,omitempty"`
	Passed        bool      `json:"passed"`
	Engine        string    `json:"engine"`
}

func NewStatement(subject Subject, predicateType string, predicate any) ([]byte, error) {
	p, err := json.Marshal(predicate)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Statement{Type: StatementType, Subject: []Subject{subject}, PredicateType: predicateType, Predicate: p})
}

func ParseStatement(e *Envelope) (*Statement, error) {
	if e.PayloadType != PayloadType {
		return nil, errors.New("unexpected payload type " + e.PayloadType)
	}
	raw, err := e.DecodePayload()
	if err != nil {
		return nil, err
	}
	var st Statement
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.Type != StatementType || len(st.Subject) == 0 {
		return nil, errors.New("not an in-toto v1 statement")
	}
	return &st, nil
}
