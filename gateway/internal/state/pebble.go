package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/cockroachdb/pebble/v2"

	"github.com/cduggn/ariadne/gateway/internal/decide"
)

// Pebble is the Store on a pebble directory, CockroachDB's pure-Go RocksDB.
// Keys are b/<tenant>, r/<run id> and p/<pod>. A value is one format byte
// then JSON. Writes go to the WAL without an fsync, so a crash may lose the
// last few milliseconds of balances and bindings, which refill and rebind
// on their own.
type Pebble struct {
	db *pebble.DB
}

const (
	bucketPrefix = "b/"
	runPrefix    = "r/"
	podPrefix    = "p/"
	format       = 1
)

// OpenPebble opens or creates the store in dir. Pebble's own messages go
// to the default slog logger.
func OpenPebble(dir string) (*Pebble, error) {
	db, err := pebble.Open(dir, &pebble.Options{Logger: slogger{}})
	if err != nil {
		return nil, fmt.Errorf("open state %s: %w", dir, err)
	}
	return &Pebble{db: db}, nil
}

// slogger is pebble's Logger on slog. Pebble's Fatalf is for a store it
// cannot go on with, so it ends the process as pebble's default does.
type slogger struct{}

func (slogger) Infof(format string, args ...any) {
	slog.Debug("pebble: " + fmt.Sprintf(format, args...))
}

func (slogger) Errorf(format string, args ...any) {
	slog.Error("pebble: " + fmt.Sprintf(format, args...))
}

func (slogger) Fatalf(format string, args ...any) {
	slog.Error("pebble: " + fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Load reads every kept entry. An entry that fails to decode is skipped.
func (p *Pebble) Load() (Snapshot, error) {
	snap := Snapshot{
		Buckets: make(map[string]Bucket),
		Runs:    make(map[decide.RunID]Run),
		Pods:    make(map[string]Pod),
	}
	errs := []error{
		scan(p.db, bucketPrefix, func(k string, v []byte) error {
			var b Bucket
			if err := decode(v, &b); err != nil {
				return err
			}
			snap.Buckets[k] = b
			return nil
		}),
		scan(p.db, runPrefix, func(k string, v []byte) error {
			var r Run
			if err := decode(v, &r); err != nil {
				return err
			}
			snap.Runs[decide.RunID(k)] = r
			return nil
		}),
		scan(p.db, podPrefix, func(k string, v []byte) error {
			var pd Pod
			if err := decode(v, &pd); err != nil {
				return err
			}
			snap.Pods[k] = pd
			return nil
		}),
	}
	return snap, errors.Join(errs...)
}

// Apply commits b as one atomic write.
func (p *Pebble) Apply(b Batch) error {
	wb := p.db.NewBatch()
	defer wb.Close()
	for name, v := range b.Buckets {
		if err := set(wb, bucketPrefix+name, v); err != nil {
			return err
		}
	}
	for id, v := range b.Runs {
		if err := set(wb, runPrefix+string(id), v); err != nil {
			return err
		}
	}
	for _, id := range b.DropRuns {
		if err := wb.Delete([]byte(runPrefix+string(id)), nil); err != nil {
			return err
		}
	}
	for name, v := range b.Pods {
		if err := set(wb, podPrefix+name, v); err != nil {
			return err
		}
	}
	return wb.Commit(pebble.NoSync)
}

// Close flushes and closes the store.
func (p *Pebble) Close() error {
	return p.db.Close()
}

func set(wb *pebble.Batch, key string, v any) error {
	out, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return wb.Set([]byte(key), append([]byte{format}, out...), nil)
}

func decode(v []byte, into any) error {
	if len(v) == 0 || v[0] != format {
		return fmt.Errorf("unknown value format %v", v[:min(len(v), 1)])
	}
	return json.Unmarshal(v[1:], into)
}

// scan calls fn with every entry under prefix, the key with its prefix
// removed. Entries fn rejects are collected, and the scan goes on.
func scan(db *pebble.DB, prefix string, fn func(key string, value []byte) error) error {
	upper := []byte(prefix)
	upper[len(upper)-1]++
	it, err := db.NewIter(&pebble.IterOptions{LowerBound: []byte(prefix), UpperBound: upper})
	if err != nil {
		return err
	}
	var errs []error
	for it.First(); it.Valid(); it.Next() {
		key := string(it.Key()[len(prefix):])
		if err := fn(key, it.Value()); err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", prefix, key, err))
		}
	}
	errs = append(errs, it.Close())
	return errors.Join(errs...)
}
