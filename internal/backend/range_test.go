package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"
)

// checkRanges stores an object and reads parts of it back: the start, the
// middle, the end, and one byte past the end, which must fail.
func checkRanges(t *testing.T, be Backend) {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, 3<<20)
	rand.Read(data)
	key := "packs/ab/range-test"
	if err := be.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	defer be.Delete(ctx, key)
	for _, c := range []struct{ off, n int64 }{{0, 1}, {0, 4096}, {1 << 20, 123457}, {int64(len(data)) - 10, 10}} {
		got, err := be.GetRange(ctx, key, c.off, c.n)
		if err != nil {
			t.Fatalf("range %d+%d: %v", c.off, c.n, err)
		}
		if !bytes.Equal(got, data[c.off:c.off+c.n]) {
			t.Fatalf("range %d+%d: wrong bytes", c.off, c.n)
		}
	}
	if _, err := be.GetRange(ctx, key, int64(len(data))-5, 10); err == nil {
		t.Error("a range past the end was read without an error")
	}
	if _, err := be.GetRange(ctx, "packs/ab/missing", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing object: %v, want ErrNotFound", err)
	}
}

func TestLocalGetRange(t *testing.T) {
	be, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkRanges(t, be)
}

// Against real S3-compatible storage, when one is given:
//
//	BP_TEST_S3_ENDPOINT=http://127.0.0.1:9000 BP_TEST_S3_BUCKET=test \
//	BP_TEST_S3_ACCESS_KEY=... BP_TEST_S3_SECRET_KEY=... go test -run S3GetRange ./internal/backend/
func TestS3GetRange(t *testing.T) {
	endpoint := os.Getenv("BP_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set BP_TEST_S3_ENDPOINT to test against S3-compatible storage")
	}
	be, err := NewS3(context.Background(), Config{Type: "s3", Endpoint: endpoint, Bucket: os.Getenv("BP_TEST_S3_BUCKET"), Region: "us-east-1", Prefix: "range-test"},
		Credentials{AccessKey: os.Getenv("BP_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("BP_TEST_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	checkRanges(t, be)
}
