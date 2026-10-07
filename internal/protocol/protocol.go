// Package protocol defines the agent <-> server wire messages. Agents only
// ever make outbound HTTPS requests; the server never connects to them.
package protocol

import (
	"github.com/chmuzamil/backupproof/internal/backend"
	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/retention"
	"github.com/chmuzamil/backupproof/internal/source"
)

type EnrollRequest struct {
	Token     string `json:"token"`
	Name      string `json:"name"`
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Version   string `json:"version"`
	PublicKey string `json:"publicKey"` // bpkey1:... Ed25519 attestation key
	Docker    bool   `json:"docker"`
}

type EnrollResponse struct {
	AgentID    int64  `json:"agentId"`
	AgentToken string `json:"agentToken"`
	ServerKey  string `json:"serverKey"` // pins the ledger checkpoint key
}

type PollRequest struct {
	Version string `json:"version"`
	Docker  bool   `json:"docker"`
}

// Lease is a job handed to an agent, including the secrets it needs.
type Lease struct {
	JobID      int64               `json:"jobId"`
	Kind       string              `json:"kind"` // backup | drill | check
	Source     source.Spec         `json:"source"`
	Repository backend.Config      `json:"repository"`
	RepoID     int64               `json:"repositoryId"`
	Password   string              `json:"password"`
	Creds      backend.Credentials `json:"credentials"`
	Retention  retention.Policy    `json:"retention"`
	// Verified lists snapshot IDs with passing drills, protected by retention.
	Verified []string `json:"verified,omitempty"`
	TSAs     []string `json:"tsas,omitempty"`
	// SampleSeed is the current ledger head: an unpredictable, pre-committed
	// seed for read-data sampling so the sample cannot be cherry-picked.
	SampleSeed string `json:"sampleSeed,omitempty"`
	// For drills: the snapshot to restore-test and its content root, taken from
	// the backup attestation the server verified against the source's own
	// agent key. Agents never pick "latest" themselves, so a snapshot planted
	// in a shared repository by another agent is never tested or trusted.
	SnapshotID   string `json:"snapshotId,omitempty"`
	ExpectedRoot string `json:"expectedRoot,omitempty"`
	LeaseSecs    int    `json:"leaseSecs"`
	// Speed limits for this server's storage traffic, bytes per second (0 = none).
	UploadBps   int64 `json:"uploadBps,omitempty"`
	DownloadBps int64 `json:"downloadBps,omitempty"`
	// Restore is set for "restore" and "restore-db" jobs.
	Restore *Restore `json:"restore,omitempty"`
}

// Restore says what to restore and where. The backup is SnapshotID, which
// the server checked against this item's signed backup proof.
type Restore struct {
	SnapshotID string   `json:"snapshotId"`
	Paths      []string `json:"paths,omitempty"`  // empty = everything
	Folder     string   `json:"folder,omitempty"` // empty = where it was backed up from
	// Databases: a new database name (or SQLite file path), or replace the original.
	DBTarget  string `json:"dbTarget,omitempty"`
	DBReplace bool   `json:"dbReplace,omitempty"`
}

type LogRequest struct {
	Lines string `json:"lines"`
}

// AttestRequest submits a signed attestation for ledgering. The server
// verifies the signature against the agent's enrolled key before accepting.
type AttestRequest struct {
	Kind       string           `json:"kind"`
	SnapshotID string           `json:"snapshotId"`
	Source     string           `json:"source"`
	Passed     bool             `json:"passed"`
	Envelope   *proof.Envelope  `json:"envelope"`
	Timestamp  *proof.Timestamp `json:"timestamp,omitempty"`
}

type FinishRequest struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
}
