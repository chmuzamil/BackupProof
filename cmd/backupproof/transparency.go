package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chmuzamil/backupproof/internal/proof"
	"github.com/chmuzamil/backupproof/internal/transparency"
)

// cmdWitness runs a witness: it cosigns a dashboard's log checkpoints, but
// only ones that extend the last it saw. Run it on a machine the dashboard's
// operators don't control.
//
//	backupproof witness --name witness.example.com --log <log key> [--log ...] [--listen :8421] [--data DIR]
func cmdWitness(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("witness", flag.ExitOnError)
	listen := fs.String("listen", envOr("BP_WITNESS_LISTEN", ":8421"), "address to listen on (put it behind HTTPS)")
	data := fs.String("data", envOr("BP_WITNESS_DATA", "./backupproof-witness"), "folder for the witness's key and what it has cosigned")
	host, _ := os.Hostname()
	name := fs.String("name", envOr("BP_WITNESS_NAME", host), "the witness's name, ideally its own address (no spaces or +)")
	var logs multi
	fs.Var(&logs, "log", "verifier key of a log to cosign (repeatable); shown under Settings → Transparency log")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if !transparency.ValidName(*name) {
		return errors.New("--name must have no spaces or +")
	}
	if len(logs) == 0 {
		return errors.New("give at least one --log: the verifier key from the dashboard's Settings → Transparency log")
	}
	trusted := map[string]transparency.Verifier{}
	for _, k := range logs {
		v, err := transparency.ParseVerifier(k)
		if err != nil {
			return fmt.Errorf("--log %s: %w", k, err)
		}
		trusted[v.Name] = v
	}
	seed, err := transparency.LoadOrCreateKey(filepath.Join(*data, "witness.key"))
	if err != nil {
		return err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	w := &transparency.Witness{Signer: transparency.Signer{Name: *name, Key: priv}, Logs: trusted, State: filepath.Join(*data, "witnessed.json")}
	fmt.Printf("witness %s listening on %s\n", *name, *listen)
	fmt.Printf("its key, to add on the dashboard (Settings → Transparency log):\n  %s\n", transparency.WitnessVerifierKey(*name, priv.Public().(ed25519.PublicKey)))
	srv := &http.Server{Addr: *listen, Handler: w, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cmdVerifyLog checks a dashboard's transparency log: its checkpoint is signed
// by the log's key and cosigned by every witness given, and, with --bundle,
// that the bundle's ledger entry is in the log.
//
//	backupproof proof verify-log --url https://backup.example.com/log --log-key <key> [--witness <key>]... [--bundle FILE] [--since FILE]
func cmdVerifyLog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proof verify-log", flag.ExitOnError)
	logURL := fs.String("url", "", "the log's address: the dashboard's address followed by /log")
	logKey := fs.String("log-key", "", "the log's verifier key (Settings → Transparency log)")
	var witnesses multi
	fs.Var(&witnesses, "witness", "verifier key of a witness that must have cosigned (repeatable)")
	bundle := fs.String("bundle", "", "a proof bundle whose ledger entry must be in the log")
	since := fs.String("since", "", "a checkpoint saved earlier: the log must extend it (it's updated to the new one)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if *logURL == "" || *logKey == "" {
		return errors.New("usage: backupproof proof verify-log --url https://DASHBOARD/log --log-key KEY [--witness KEY]... [--bundle FILE] [--since FILE]\n(a private log needs an API token in BP_TOKEN)")
	}
	lv, err := transparency.ParseVerifier(*logKey)
	if err != nil {
		return fmt.Errorf("--log-key: %w", err)
	}
	var wvs []transparency.Verifier
	for _, k := range witnesses {
		v, err := transparency.ParseVerifier(k)
		if err != nil {
			return fmt.Errorf("--witness %s: %w", k, err)
		}
		wvs = append(wvs, v)
	}
	base := strings.TrimSuffix(*logURL, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	fetch := func(ctx context.Context, path string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+path, nil)
		if err != nil {
			return nil, err
		}
		if tok := os.Getenv("BP_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", path, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	}
	note, err := fetch(ctx, "checkpoint")
	if err != nil {
		return err
	}
	cp, cosigned, err := transparency.VerifyNote(string(note), lv, wvs)
	if err != nil {
		return fmt.Errorf("checkpoint NOT valid: %w", err)
	}
	fmt.Printf("checkpoint: %d entries, signed by the log %s\n", cp.Size, cp.Origin)
	for _, w := range wvs {
		fmt.Printf("  cosigned by %s at %s\n", w.Name, cosigned[w.Name].Format(time.RFC3339))
	}
	if *since != "" {
		if raw, err := os.ReadFile(*since); err == nil {
			older, _, err := transparency.VerifyNote(string(raw), lv, nil)
			if err != nil {
				return fmt.Errorf("the saved checkpoint: %w", err)
			}
			if err := transparency.VerifyConsistency(ctx, fetch, older, cp); err != nil {
				return fmt.Errorf("the log does NOT extend the saved checkpoint (%d entries): %w", older.Size, err)
			}
			fmt.Printf("extends the saved checkpoint of %d entries: nothing earlier was changed\n", older.Size)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(*since, note, 0o644); err != nil {
			return err
		}
	}
	if *bundle != "" {
		raw, err := os.ReadFile(*bundle)
		if err != nil {
			return err
		}
		b, err := proof.ParseBundle(raw)
		if err != nil {
			return err
		}
		entry, ok := bundleEntry(b)
		if !ok {
			return errors.New("the bundle has no ledger entry for its proof")
		}
		if entry.Seq < 1 || entry.Seq > cp.Size {
			return fmt.Errorf("the bundle's ledger entry #%d is newer than the checkpoint (%d entries); try again after the next checkpoint", entry.Seq, cp.Size)
		}
		if err := transparency.VerifyInclusion(ctx, fetch, cp, entry.Seq-1, transparency.LeafData(entry.Hash)); err != nil {
			return fmt.Errorf("ledger entry #%d is NOT in the log: %w", entry.Seq, err)
		}
		fmt.Printf("the bundle's ledger entry #%d is in the log\n", entry.Seq)
	}
	fmt.Println("valid")
	return nil
}

// bundleEntry finds the ledger entry recording the bundle's own proof.
func bundleEntry(b *proof.Bundle) (proof.LedgerEntry, bool) {
	if b.Envelope == nil {
		return proof.LedgerEntry{}, false
	}
	d := b.Envelope.Digest()
	for _, e := range b.Ledger {
		if e.EnvelopeDigest == d {
			return e, true
		}
	}
	return proof.LedgerEntry{}, false
}
