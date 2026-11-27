package proof

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
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
