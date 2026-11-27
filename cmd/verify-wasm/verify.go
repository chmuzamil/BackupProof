//go:build js && wasm

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/chmuzamil/backupproof/internal/proof"
)

// verify checks a bundle exactly as `backupproof proof verify` does.
func verify(bundleJSON, trustedKeys string, requireTimestamp bool) string {
	b, err := proof.ParseBundle([]byte(bundleJSON))
	if err != nil {
		return result(nil, "this isn't a BackupProof proof bundle: "+err.Error())
	}
	keys, err := parseKeys(trustedKeys)
	if err != nil {
		return result(nil, err.Error())
	}
	return result(proof.VerifyBundle(b, proof.VerifyOptions{TrustedKeys: keys, RequireTimestamp: requireTimestamp}), "")
}

// parseKeys reads bpkey1:… keys, one per line; blank lines and # comments
// are skipped, as in the dashboard's public key list.
func parseKeys(s string) ([]proof.PublicKey, error) {
	var keys []proof.PublicKey
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := proof.ParsePublicKey(line)
		if err != nil {
			return nil, fmt.Errorf("trusted key on line %d: %w", i+1, err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func result(rep *proof.Report, errMsg string) string {
	out := struct {
		Report *proof.Report `json:"report,omitempty"`
		Error  string        `json:"error,omitempty"`
	}{rep, errMsg}
	if rep == nil && errMsg == "" {
		out.Error = errors.New("no result").Error()
	}
	b, _ := json.Marshal(out)
	return string(b)
}
