package transfer

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Retry policy for one storage request: exponential backoff with jitter, so
// parallel workers don't retry in lockstep. Waits grow from about 0.5 s to
// 16 s, giving a busy storage node 15-30 s to recover over six attempts.
var (
	Attempts       = 6
	BackoffBase    = 500 * time.Millisecond
	BackoffCap     = 30 * time.Second
	AttemptTimeout = 5 * time.Minute // per request, not per job
)

// StatusError is an HTTP-like failure from a storage service, for backends
// (and tests) that don't use the AWS SDK.
type StatusError struct {
	Code       int
	RetryAfter time.Duration
	Err        error
}

func (e *StatusError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "storage returned HTTP " + strconv.Itoa(e.Code)
}
func (e *StatusError) Unwrap() error { return e.Err }

// Do runs op, retrying transient failures (408, 429, 5xx, dropped
// connections) and honouring Retry-After. Each attempt has its own timeout.
// Permanent failures, such as a missing object, return at once.
func Do(ctx context.Context, op func(ctx context.Context) error) error {
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, AttemptTimeout)
		err := op(actx)
		cancel()
		if err == nil {
			return nil
		}
		retry, after, throttled := Classify(err)
		if throttled {
			if c := controllerFrom(ctx); c != nil {
				c.Throttled()
			}
		}
		if !retry || attempt >= Attempts || ctx.Err() != nil {
			return err
		}
		wait := backoff(attempt)
		if after > wait {
			wait = after + time.Duration(rand.Int64N(int64(250*time.Millisecond)))
		}
		if log := loggerFrom(ctx); log != nil {
			log("storage %s, trying again in %s (attempt %d of %d)", failureWords(err), wait.Round(100*time.Millisecond), attempt+1, Attempts)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// Logger receives a line for each retry, for the job log.
type Logger func(format string, args ...any)

type loggerKey struct{}

// WithLogger makes Do report its retries to log.
func WithLogger(ctx context.Context, log Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

func loggerFrom(ctx context.Context) Logger {
	l, _ := ctx.Value(loggerKey{}).(Logger)
	return l
}

// failureWords says briefly what went wrong with a request.
func failureWords(err error) string {
	var se *StatusError
	if errors.As(err, &se) && se.Code != 0 {
		return "returned HTTP " + strconv.Itoa(se.Code)
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		return "returned HTTP " + strconv.Itoa(re.HTTPStatusCode())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "didn't answer in time"
	}
	return "connection failed"
}

// backoff is the wait before attempt+1: between half and all of
// base·2^(attempt-1), capped.
func backoff(attempt int) time.Duration {
	d := BackoffBase << (attempt - 1)
	if d <= 0 || d > BackoffCap {
		d = BackoffCap
	}
	// "Equal jitter": at least half the step, so retries never come back
	// within a moment, and random within it, so workers spread out.
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// Classify reports whether err is worth retrying, how long the service
// asked to wait, and whether it was the service asking to slow down.
func Classify(err error) (retry bool, after time.Duration, throttled bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return false, 0, false
	}
	code := 0
	var se *StatusError
	if errors.As(err, &se) {
		code, after = se.Code, se.RetryAfter
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		code = re.HTTPStatusCode()
		after = parseRetryAfter(re.Response.Header.Get("Retry-After"))
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "SlowDown", "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequests":
			return true, after, true
		case "RequestTimeout", "InternalError", "ServiceUnavailable":
			return true, after, false
		}
	}
	switch code {
	case 429, 503:
		return true, after, true
	case 408, 500, 502, 504:
		return true, after, false
	}
	if code != 0 {
		return false, 0, false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true, 0, false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true, 0, false
	}
	msg := err.Error()
	for _, s := range []string{"connection reset", "broken pipe", "unexpected EOF", "use of closed network connection", "http2: "} {
		if strings.Contains(msg, s) {
			return true, 0, false
		}
	}
	return false, 0, false
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, 2*time.Minute)
	}
	if t, err := time.Parse(time.RFC1123, v); err == nil {
		return min(max(time.Until(t), 0), 2*time.Minute)
	}
	return 0
}
