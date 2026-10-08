package engine

import (
	"context"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/transfer"
)

// Uploader runs chunk uploads in parallel under a transfer.Controller, with
// the chunk data waiting to go out capped in bytes. Backups and copies use it;
// both write their snapshot only after Wait reports every upload succeeded,
// so a failure never leaves a snapshot pointing at a missing chunk.
type Uploader struct {
	ctx    context.Context
	cancel context.CancelFunc
	seq    bool
	ctrl   *transfer.Controller
	wg     sync.WaitGroup

	mu       sync.Mutex
	cond     *sync.Cond
	cap      int64
	inflight int64
	peak     int64
	err      error
}

// NewUploader uses the transfer settings in ctx.
func NewUploader(ctx context.Context) *Uploader {
	s := transfer.SettingsFrom(ctx)
	ctx, cancel := context.WithCancel(ctx)
	u := &Uploader{ctx: ctx, cancel: cancel, seq: s.Sequential(), cap: s.Inflight()}
	u.cond = sync.NewCond(&u.mu)
	u.ctrl = transfer.NewController(s, 0)
	context.AfterFunc(ctx, func() {
		u.mu.Lock()
		u.cond.Broadcast()
		u.mu.Unlock()
	})
	return u
}

// Go uploads size bytes with fn, waiting for room first. It returns the
// first upload error so far, after which nothing more should be queued.
// fn reports how many bytes it actually sent (0 when the chunk was stored
// already).
func (u *Uploader) Go(size int64, fn func(ctx context.Context) (int64, error)) error {
	if u.seq {
		if _, err := fn(u.ctx); err != nil {
			u.fail(err)
		}
		return u.failed()
	}
	u.mu.Lock()
	for u.inflight > 0 && u.inflight+size > u.cap && u.err == nil && u.ctx.Err() == nil {
		u.cond.Wait()
	}
	if u.err != nil || u.ctx.Err() != nil {
		u.mu.Unlock()
		return u.failed()
	}
	u.inflight += size
	u.peak = max(u.peak, u.inflight)
	u.mu.Unlock()
	if err := u.ctrl.Acquire(u.ctx); err != nil {
		u.done(size)
		return u.failed()
	}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		start := time.Now()
		n, err := fn(transfer.WithController(u.ctx, u.ctrl))
		u.ctrl.Release(n, time.Since(start), err == nil)
		if err != nil {
			u.fail(err)
		}
		u.done(size)
	}()
	return nil
}

func (u *Uploader) done(size int64) {
	u.mu.Lock()
	u.inflight -= size
	u.cond.Broadcast()
	u.mu.Unlock()
}

func (u *Uploader) fail(err error) {
	u.mu.Lock()
	if u.err == nil {
		u.err = err
		u.cancel()
	}
	u.cond.Broadcast()
	u.mu.Unlock()
}

func (u *Uploader) failed() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return u.err
	}
	return u.ctx.Err()
}

// Wait waits for every upload and returns the first error.
func (u *Uploader) Wait() error {
	u.wg.Wait()
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

// Peak is the most chunk data waiting at once, for tests.
func (u *Uploader) Peak() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.peak
}

// Close stops any uploads still running.
func (u *Uploader) Close() {
	u.cancel()
	u.wg.Wait()
}
