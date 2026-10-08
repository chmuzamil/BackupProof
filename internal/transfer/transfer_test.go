package transfer

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		err              error
		retry, throttled bool
	}{
		{&StatusError{Code: 503}, true, true},
		{&StatusError{Code: 429}, true, true},
		{&StatusError{Code: 500}, true, false},
		{&StatusError{Code: 404}, false, false},
		{&StatusError{Code: 403}, false, false},
		{io.ErrUnexpectedEOF, true, false},
		{errors.New("read tcp: connection reset by peer"), true, false},
		{context.Canceled, false, false},
		{errors.New("authentication failed (corrupt or tampered)"), false, false},
	} {
		retry, _, thr := Classify(c.err)
		if retry != c.retry || thr != c.throttled {
			t.Errorf("%v: retry %v throttled %v, want %v %v", c.err, retry, thr, c.retry, c.throttled)
		}
	}
	if _, after, _ := Classify(&StatusError{Code: 503, RetryAfter: 3 * time.Second}); after != 3*time.Second {
		t.Errorf("Retry-After: %v", after)
	}
}

func TestDoRetriesThenGivesUp(t *testing.T) {
	old := BackoffBase
	BackoffBase = time.Millisecond
	defer func() { BackoffBase = old }()
	n := 0
	err := Do(context.Background(), func(context.Context) error {
		n++
		if n < 3 {
			return &StatusError{Code: 503}
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("after two 503s: %v, %d attempts", err, n)
	}
	n = 0
	if err := Do(context.Background(), func(context.Context) error { n++; return &StatusError{Code: 500} }); err == nil || n != Attempts {
		t.Fatalf("persistent 500: %v after %d attempts, want %d", err, n, Attempts)
	}
	n = 0
	if err := Do(context.Background(), func(context.Context) error { n++; return &StatusError{Code: 404} }); err == nil || n != 1 {
		t.Fatalf("404 was retried: %d attempts", n)
	}
}

func TestControllerAIMD(t *testing.T) {
	c := NewController(Settings{}, 0)
	if c.Limit() != StartConcurrency {
		t.Fatalf("starts at %d", c.Limit())
	}
	round := func(throttle bool) {
		n := c.Limit()
		for i := 0; i < n; i++ {
			if err := c.Acquire(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if throttle {
			c.Throttled()
		}
		for i := 0; i < n; i++ {
			c.Release(1<<20, 10*time.Millisecond, true)
		}
	}
	round(false)
	if c.Limit() != StartConcurrency+1 {
		t.Errorf("after a good round: %d", c.Limit())
	}
	round(true)
	if c.Limit() != (StartConcurrency+1)/2 && c.Limit() != MinConcurrency {
		t.Errorf("after throttling: %d", c.Limit())
	}
	for i := 0; i < 5; i++ {
		round(true)
	}
	if c.Limit() != MinConcurrency {
		t.Errorf("never below the minimum: %d", c.Limit())
	}
	if f := NewController(Settings{Concurrency: 1}, 0); f.Limit() != 1 {
		t.Errorf("concurrency 1: %d", f.Limit())
	}
	if h := NewController(Settings{}, CheckConcurrency); h.Limit() > CheckConcurrency {
		t.Errorf("health checks: %d", h.Limit())
	}
}
