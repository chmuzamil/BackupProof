package proof

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/digitorus/timestamp"
)

// Timestamp is an RFC 3161 token proving that a digest existed no later than
// Time, as attested by an independent Time-Stamping Authority.
type Timestamp struct {
	TSA   string    `json:"tsa"`
	Time  time.Time `json:"time"`
	Token string    `json:"token"` // base64 DER TimeStampToken
}

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

// Verify checks the token signature and that it covers data. When roots is
// non-nil the TSA certificate must also chain to one of them.
func (t *Timestamp) Verify(data []byte, roots *x509.CertPool) (time.Time, error) {
	raw, err := base64.StdEncoding.DecodeString(t.Token)
	if err != nil {
		return time.Time{}, err
	}
	ts, err := timestamp.Parse(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp token invalid: %w", err)
	}
	if ts.HashAlgorithm != crypto.SHA256 {
		return time.Time{}, errors.New("timestamp uses an unexpected hash algorithm")
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(ts.HashedMessage, sum[:]) {
		return time.Time{}, errors.New("timestamp does not cover this data")
	}
	if roots != nil {
		if len(ts.Certificates) == 0 {
			return time.Time{}, errors.New("timestamp token carries no certificate")
		}
		signer, inter := ts.Certificates[0], x509.NewCertPool()
		for _, c := range ts.Certificates {
			inter.AddCert(c)
			for _, u := range c.ExtKeyUsage {
				if u == x509.ExtKeyUsageTimeStamping {
					signer = c
				}
			}
		}
		if _, err := signer.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: inter, CurrentTime: ts.Time,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
		}); err != nil {
			return time.Time{}, fmt.Errorf("TSA certificate not trusted: %w", err)
		}
	}
	return ts.Time, nil
}
