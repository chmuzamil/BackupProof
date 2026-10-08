// Package transfer sets how BackupProof moves chunks to and from storage:
// how many requests run at once (adapting to what the link and the storage
// service can take), how much data may be held in memory meanwhile, and how
// failed requests are retried.
//
// It only changes speed. What is read, written and verified, and what counts
// as a passed restore test, is decided elsewhere and is the same at any
// concurrency.
package transfer

import "context"

const (
	// DefaultMaxInflight caps the chunk data held in memory per transfer.
	DefaultMaxInflight int64 = 64 << 20
	// Adaptive concurrency starts here and moves between Min and Max.
	StartConcurrency = 8
	MinConcurrency   = 4
	MaxConcurrency   = 32
	// CheckConcurrency is the most a storage health check uses, so a weekly
	// check doesn't saturate a small server.
	CheckConcurrency = 4
)

// Settings tune transfers. The zero value is the default: adaptive
// concurrency and a 64 MiB memory cap.
type Settings struct {
	// Concurrency is how many requests run at once: 0 adapts between
	// MinConcurrency and MaxConcurrency, 1 moves one chunk at a time exactly
	// as older versions did, and any other number is used as is.
	Concurrency int `json:"concurrency,omitempty"`
	// MaxInflight caps the chunk data (decrypted and decompressed) held in
	// memory at once, in bytes. 0 means DefaultMaxInflight.
	MaxInflight int64 `json:"maxInflight,omitempty"`
}

// Inflight is the memory cap in bytes.
func (s Settings) Inflight() int64 {
	if s.MaxInflight > 0 {
		return s.MaxInflight
	}
	return DefaultMaxInflight
}

// Sequential reports whether transfers should move one chunk at a time.
func (s Settings) Sequential() bool { return s.Concurrency == 1 }

type settingsKey struct{}

// WithSettings attaches transfer settings to a job's context.
func WithSettings(ctx context.Context, s Settings) context.Context {
	return context.WithValue(ctx, settingsKey{}, s)
}

// SettingsFrom returns the settings attached to ctx, or the defaults.
func SettingsFrom(ctx context.Context) Settings {
	s, _ := ctx.Value(settingsKey{}).(Settings)
	return s
}
