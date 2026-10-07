package backend

import (
	"context"
	"io"
	"sync"
	"time"
)

// Throttle limits how fast data moves to (up) and from (down) storage, in
// bytes per second; 0 means no limit. It paces whole objects (chunks are
// about 1 MB), which keeps the average rate at the limit without slowing
// small requests such as listings.
func Throttle(b Backend, up, down int64) Backend {
	if up <= 0 && down <= 0 {
		return b
	}
	return &throttled{Backend: b, up: newPacer(up), down: newPacer(down)}
}

type pacer struct {
	mu   sync.Mutex
	rate int64
	next time.Time
}

func newPacer(rate int64) *pacer {
	if rate <= 0 {
		return nil
	}
	return &pacer{rate: rate}
}

// wait reserves time for n bytes and sleeps until it's this transfer's turn.
func (p *pacer) wait(ctx context.Context, n int) error {
	if p == nil || n <= 0 {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	start := p.next
	if start.Before(now) {
		start = now
	}
	p.next = start.Add(time.Duration(float64(n) / float64(p.rate) * float64(time.Second)))
	p.mu.Unlock()
	if d := time.Until(start); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

type throttled struct {
	Backend
	up, down *pacer
}

func (t *throttled) Put(ctx context.Context, key string, data []byte) error {
	if err := t.up.wait(ctx, len(data)); err != nil {
		return err
	}
	return t.Backend.Put(ctx, key, data)
}

func (t *throttled) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := t.Backend.Get(ctx, key)
	if err == nil {
		err = t.down.wait(ctx, len(b))
	}
	return b, err
}

func (t *throttled) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := t.Backend.Open(ctx, key)
	if err != nil || t.down == nil {
		return rc, err
	}
	return &pacedReader{ReadCloser: rc, ctx: ctx, p: t.down}, nil
}

type pacedReader struct {
	io.ReadCloser
	ctx context.Context
	p   *pacer
}

func (r *pacedReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if werr := r.p.wait(r.ctx, n); werr != nil && err == nil {
		err = werr
	}
	return n, err
}
