//go:build !js

package proof

// Requesting timestamps needs the network; the browser verifier
// (cmd/verify-wasm) only checks them, so this file is left out of it.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/digitorus/timestamp"
)

// DefaultTSAs are free public authorities; tokens are requested from the
// first that answers. Configure your own for production.
var DefaultTSAs = []string{"https://freetsa.org/tsr", "http://timestamp.digicert.com"}

// RequestTimestamp obtains a token over SHA-256(data) from the first working TSA.
func RequestTimestamp(ctx context.Context, data []byte, tsas []string) (*Timestamp, error) {
	if len(tsas) == 0 {
		tsas = DefaultTSAs
	}
	req, err := timestamp.CreateRequest(bytes.NewReader(data), &timestamp.RequestOptions{Hash: crypto.SHA256, Certificates: true})
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, url := range tsas {
		ts, err := requestOne(ctx, url, req, data)
		if err == nil {
			return ts, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", url, err))
	}
	return nil, errors.Join(errs...)
}

func requestOne(ctx context.Context, url string, req, data []byte) (*Timestamp, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/timestamp-query")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	ts, err := timestamp.ParseResponse(body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(ts.HashedMessage, sum[:]) {
		return nil, errors.New("TSA returned a token for a different digest")
	}
	return &Timestamp{TSA: url, Time: ts.Time, Token: base64.StdEncoding.EncodeToString(ts.RawToken)}, nil
}
