package server

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/chmuzamil/backupproof/internal/transparency"
	"golang.org/x/mod/sumdb/tlog"
)

// The proof ledger is also a C2SP transparency log: ledger entry N (from 1) is
// log entry N-1, whose data is the entry's hash. The RFC 6962 stored hashes
// live in tlog_hashes, written in the same transaction as the ledger entry.

const tlogSchema = `CREATE TABLE IF NOT EXISTS tlog_hashes (idx INTEGER PRIMARY KEY, hash BLOB NOT NULL);
CREATE TRIGGER IF NOT EXISTS tlog_no_update BEFORE UPDATE ON tlog_hashes BEGIN SELECT RAISE(ABORT, 'the transparency log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS tlog_no_delete BEFORE DELETE ON tlog_hashes BEGIN SELECT RAISE(ABORT, 'the transparency log is append-only'); END;`

const settingTlogSize = "tlog-size" // ledger entries already in the log

// queryer is a *sql.DB or *sql.Tx.
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// hashReader reads stored hashes through db or a transaction.
type hashReader struct{ q queryer }

func (h hashReader) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	out := make([]tlog.Hash, len(indexes))
	for i, x := range indexes {
		var b []byte
		if err := h.q.QueryRow("SELECT hash FROM tlog_hashes WHERE idx=?", x).Scan(&b); err != nil {
			return nil, fmt.Errorf("stored hash %d: %w", x, err)
		}
		if len(b) != tlog.HashSize {
			return nil, fmt.Errorf("stored hash %d is damaged", x)
		}
		copy(out[i][:], b)
	}
	return out, nil
}

// appendTlog adds ledger entry seq (from 1) with entryHash to the log.
func appendTlog(q queryer, seq int64, entryHash string) error {
	hashes, err := tlog.StoredHashes(seq-1, transparency.LeafData(entryHash), hashReader{q})
	if err != nil {
		return err
	}
	first := tlog.StoredHashIndex(0, seq-1)
	for i, h := range hashes {
		if _, err := q.Exec("INSERT INTO tlog_hashes(idx,hash) VALUES(?,?)", first+int64(i), h[:]); err != nil {
			return err
		}
	}
	_, err = q.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", settingTlogSize, strconv.FormatInt(seq, 10))
	return err
}

// tlogSize is how many ledger entries are in the log.
func tlogSize(q queryer) int64 {
	var v string
	if q.QueryRow("SELECT value FROM settings WHERE key=?", settingTlogSize).Scan(&v) != nil {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// backfillTlog adds ledger entries written before the log existed (an
// upgraded dashboard), oldest first, each in its own transaction.
func (s *Store) backfillTlog() error {
	if _, err := s.db.Exec(tlogSchema); err != nil {
		return err
	}
	for {
		s.wmu.Lock()
		err := func() error {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			next := tlogSize(tx) + 1
			var hash string
			err = tx.QueryRow("SELECT hash FROM ledger WHERE seq=?", next).Scan(&hash)
			if errors.Is(err, sql.ErrNoRows) {
				return errDone
			}
			if err != nil {
				return err
			}
			if err := appendTlog(tx, next, hash); err != nil {
				return err
			}
			return tx.Commit()
		}()
		s.wmu.Unlock()
		if errors.Is(err, errDone) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("adding the ledger to the transparency log: %w", err)
		}
	}
}

var errDone = errors.New("done")

// TlogSize and TlogRoot describe the log now.
func (s *Store) TlogSize() int64 { return tlogSize(s.db) }

func (s *Store) TlogRoot(n int64) (tlog.Hash, error) { return tlog.TreeHash(n, hashReader{s.db}) }

// TlogTile serves a C2SP tile or entry bundle of the log at size n.
func (s *Store) TlogTile(path string, n int64) ([]byte, error) {
	var ferr error
	b, err := transparency.ServeTile(path, n, hashReader{s.db}, func(i int64) []byte {
		var hash string
		if err := s.db.QueryRow("SELECT hash FROM ledger WHERE seq=?", i+1).Scan(&hash); err != nil {
			ferr = err
		}
		return transparency.LeafData(hash)
	})
	if err == nil {
		err = ferr
	}
	return b, err
}

// TlogConsistency proves the log at size old is a prefix of the log at n.
func (s *Store) TlogConsistency(n, old int64) (tlog.TreeProof, error) {
	return tlog.ProveTree(n, old, hashReader{s.db})
}
