package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// The error B2 gave on 29 Oct 2026: a 503 whose body failed its checksum,
// wrapped by the AWS SDK with its own retries turned off.
func TestClassifySDK503(t *testing.T) {
	err := &smithy.OperationError{ServiceID: "S3", OperationName: "GetObject", Err: &retry.MaxAttemptsError{Attempt: 1,
		Err: &awshttp.ResponseError{RequestID: "x", ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 503, Header: http.Header{}}},
			Err:      &smithy.DeserializationError{Err: errors.New("failed to copy error response body, checksum did not match")},
		}}}}
	retry, _, throttled := Classify(err)
	t.Logf("retry=%v throttled=%v: %v", retry, throttled, err)
	if !retry {
		t.Error("a 503 from the SDK must be retried")
	}
}

// Waits are at least half their step, so a busy service gets time to
// recover, and each retry is reported for the job log.
func TestBackoffAndRetryLog(t *testing.T) {
	for a := 1; a <= 5; a++ {
		step := BackoffBase << (a - 1)
		for i := 0; i < 200; i++ {
			if d := backoff(a); d < step/2 || d > step {
				t.Fatalf("attempt %d: waited %v, want %v..%v", a, d, step/2, step)
			}
		}
	}
	old := BackoffBase
	BackoffBase = time.Millisecond
	defer func() { BackoffBase = old }()
	var lines []string
	ctx := WithLogger(context.Background(), func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	n := 0
	_ = Do(ctx, func(context.Context) error {
		if n++; n < 3 {
			return &StatusError{Code: 503}
		}
		return nil
	})
	if len(lines) != 2 || !strings.Contains(lines[0], "returned HTTP 503") || !strings.Contains(lines[0], "attempt 2 of") {
		t.Errorf("retry log: %q", lines)
	}
}
