package transfer

import (
	"context"
	"sync"
	"time"
)

// Controller limits how many requests run at once. With adaptive settings it
// works like TCP congestion control (AIMD): after each round of requests it
// adds one while throughput holds up and latency doesn't climb, and halves
// when the storage service pushes back (429, 503, SlowDown) or latency more
// than doubles, which means requests are queueing somewhere.
type Controller struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int
	lo, hi int
	fixed  bool
	active int

	roundStart time.Time
	roundBytes int64
	roundDone  int
	throttled  bool
	lastRate   float64
	lat, base  time.Duration
}

// NewController returns a controller for s. max caps adaptive concurrency
// (0 means MaxConcurrency); an explicit Concurrency is used as is.
func NewController(s Settings, max int) *Controller {
	c := &Controller{}
	c.cond = sync.NewCond(&c.mu)
	if s.Concurrency > 0 {
		c.limit, c.lo, c.hi, c.fixed = s.Concurrency, s.Concurrency, s.Concurrency, true
		return c
	}
	if max <= 0 {
		max = MaxConcurrency
	}
	c.hi = max
	c.lo = min(MinConcurrency, max)
	c.limit = min(StartConcurrency, max)
	return c
}

// Limit is the current number of requests allowed at once.
func (c *Controller) Limit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

// Acquire waits for a free slot.
func (c *Controller) Acquire(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.active >= c.limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.roundStart.IsZero() {
		c.roundStart = time.Now()
	}
	c.active++
	return nil
}

// Release frees a slot after a request that moved n bytes in d.
func (c *Controller) Release(n int64, d time.Duration, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active--
	defer c.cond.Broadcast()
	if c.fixed {
		return
	}
	if ok {
		c.roundBytes += n
		if c.lat == 0 {
			c.lat = d
		} else {
			c.lat = (c.lat*7 + d) / 8
		}
		if c.base == 0 || c.lat < c.base {
			c.base = c.lat
		}
	}
	c.roundDone++
	if c.roundDone < c.limit {
		return
	}
	elapsed := time.Since(c.roundStart).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(c.roundBytes) / elapsed
	}
	switch {
	case c.throttled, c.base > 0 && c.lat > 2*c.base && rate <= c.lastRate*1.05:
		c.limit = max(c.lo, c.limit/2)
		c.base = c.lat // latency at the new level is the new baseline
	case rate >= c.lastRate*0.95 && (c.base == 0 || c.lat <= c.base*3/2):
		c.limit = min(c.hi, c.limit+1)
	}
	c.lastRate = rate
	c.roundStart, c.roundBytes, c.roundDone, c.throttled = time.Now(), 0, 0, false
}

// Throttled records that the storage service asked to slow down.
func (c *Controller) Throttled() {
	c.mu.Lock()
	c.throttled = true
	c.mu.Unlock()
}

type controllerKey struct{}

// WithController lets the retry layer below report throttling to c.
func WithController(ctx context.Context, c *Controller) context.Context {
	return context.WithValue(ctx, controllerKey{}, c)
}

func controllerFrom(ctx context.Context) *Controller {
	c, _ := ctx.Value(controllerKey{}).(*Controller)
	return c
}
