package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	bpcrypto "github.com/chmuzamil/backupproof/internal/crypto"
	"github.com/chmuzamil/backupproof/internal/repo"
	"github.com/chmuzamil/backupproof/internal/snapshot"
	"github.com/chmuzamil/backupproof/internal/transfer"
)

// Want is a chunk a Fetcher will be asked for, with a guess at its size
// (for the memory cap) when one is known.
type Want struct {
	ID   bpcrypto.ID
	Hint int64
}

// FetchOptions adjust a Fetcher for its job.
type FetchOptions struct {
	// MaxConcurrency caps adaptive concurrency (health checks use less).
	MaxConcurrency int
	// KeepGoing returns each chunk's error to its reader instead of stopping
	// everything at the first failure (health checks count bad chunks).
	KeepGoing bool
}

// appendWants lists a file's chunks, guessing each one's size as the file's
// average chunk size.
func appendWants(wants []Want, e *snapshot.Entry) []Want {
	hint := int64(0)
	if len(e.Chunks) > 0 {
		hint = e.Size / int64(len(e.Chunks))
	}
	for _, cs := range e.Chunks {
		if id, err := bpcrypto.ParseID(cs); err == nil {
			wants = append(wants, Want{ID: id, Hint: hint})
		}
	}
	return wants
}

// AppendWants is appendWants for other packages (the dashboard's zip download).
func AppendWants(wants []Want, e *snapshot.Entry) []Want { return appendWants(wants, e) }

// defaultHint is the guessed size of a chunk whose size isn't known.
const defaultHint = 1 << 20

// Fetcher downloads chunks ahead of use, several at a time, and hands them
// out in the order they were listed. It is the one place restores, restore
// tests, copies, zip downloads and health checks read chunks:
//
//   - downloads run in parallel under a transfer.Controller (adaptive, or
//     one at a time with concurrency 1, which reads exactly like older
//     versions did: one chunk, when asked for it);
//   - decrypting and verifying run on separate workers, at most one per
//     CPU, so network and CPU overlap;
//   - chunk data waiting to be used is capped in bytes (the oldest listed
//     chunk may always go over, so the reader can never be starved);
//   - a chunk listed twice while still waiting is downloaded once;
//   - every chunk is verified (authentication tag and content hash) before
//     it is handed out, and the first failure cancels all other work and
//     is returned naming the chunk.
type Fetcher struct {
	r     *repo.Repo
	wants []Want
	seq   bool
	keep  bool

	ctx    context.Context
	cancel context.CancelFunc
	ctrl   *transfer.Controller
	cpu    chan struct{}
	wg     sync.WaitGroup

	mu         sync.Mutex
	cond       *sync.Cond
	cap        int64
	inflight   int64
	peak       int64
	items      []*fetched
	pending    map[bpcrypto.ID]*fetched
	dispatched int
	next       int
	err        error
}

type fetched struct {
	id       bpcrypto.ID
	done     chan struct{}
	data     []byte
	err      error
	reserved int64
	refs     int
}

// NewFetcher starts downloading wants, using the transfer settings in ctx.
// Call Close when done.
func NewFetcher(ctx context.Context, r *repo.Repo, wants []Want, opts FetchOptions) *Fetcher {
	s := transfer.SettingsFrom(ctx)
	ctx, cancel := context.WithCancel(ctx)
	f := &Fetcher{r: r, wants: wants, seq: s.Sequential(), keep: opts.KeepGoing, ctx: ctx, cancel: cancel,
		cap: s.Inflight(), items: make([]*fetched, len(wants)), pending: map[bpcrypto.ID]*fetched{}}
	f.cond = sync.NewCond(&f.mu)
	if f.seq {
		return f
	}
	f.ctrl = transfer.NewController(s, opts.MaxConcurrency)
	f.cpu = make(chan struct{}, runtime.GOMAXPROCS(0))
	// Wake anything waiting for room or for a chunk when the work stops.
	context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		f.dispatch()
	}()
	return f
}

func (f *Fetcher) dispatch() {
	for i, w := range f.wants {
		f.mu.Lock()
		if it := f.pending[w.ID]; it != nil { // already on its way: share it
			it.refs++
			f.items[i] = it
			f.dispatched = i + 1
			f.cond.Broadcast()
			f.mu.Unlock()
			continue
		}
		est := w.Hint
		if est <= 0 {
			est = defaultHint
		}
		for f.inflight > 0 && f.inflight+est > f.cap && f.ctx.Err() == nil {
			f.cond.Wait()
		}
		if f.ctx.Err() != nil {
			f.mu.Unlock()
			return
		}
		it := &fetched{id: w.ID, done: make(chan struct{}), reserved: est, refs: 1}
		f.add(est)
		f.pending[w.ID] = it
		f.items[i] = it
		f.dispatched = i + 1
		f.cond.Broadcast()
		f.mu.Unlock()
		if err := f.ctrl.Acquire(f.ctx); err != nil {
			f.finish(it, nil, err)
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.fetch(w.ID, it)
		}()
	}
}

func (f *Fetcher) fetch(id bpcrypto.ID, it *fetched) {
	start := time.Now()
	raw, err := f.r.FetchBlob(transfer.WithController(f.ctx, f.ctrl), id)
	f.ctrl.Release(int64(len(raw)), time.Since(start), err == nil)
	var data []byte
	if err == nil {
		select {
		case f.cpu <- struct{}{}:
			data, err = f.r.OpenBlob(id, raw)
			<-f.cpu
		case <-f.ctx.Done():
			err = f.ctx.Err()
		}
	}
	if err != nil {
		err = fmt.Errorf("chunk %s: %w", id, err)
	}
	f.finish(it, data, err)
}

// finish stores a chunk and settles its memory reservation at its real size.
// A chunk that is bigger than guessed waits for room unless it is the next
// one the reader needs.
func (f *Fetcher) finish(it *fetched, data []byte, err error) {
	f.mu.Lock()
	if delta := int64(len(data)) - it.reserved; delta > 0 {
		for f.ctx.Err() == nil && f.inflight+delta > f.cap && !f.isNext(it) {
			f.cond.Wait()
		}
		f.add(delta)
	} else {
		f.add(delta)
	}
	it.reserved = int64(len(data))
	it.data, it.err = data, err
	if err != nil && !f.keep && f.err == nil && !errors.Is(err, context.Canceled) {
		f.err = err
		f.cancel()
	}
	f.cond.Broadcast()
	f.mu.Unlock()
	close(it.done)
}

func (f *Fetcher) isNext(it *fetched) bool { return f.next < len(f.items) && f.items[f.next] == it }

func (f *Fetcher) add(n int64) {
	f.inflight += n
	if f.inflight > f.peak {
		f.peak = f.inflight
	}
}

// Get returns chunk id, verified. When it is among the next chunks listed it
// comes from the download-ahead (listed chunks before it that weren't needed
// after all are dropped); otherwise it is read directly.
func (f *Fetcher) Get(ctx context.Context, id bpcrypto.ID) ([]byte, error) {
	if f.seq {
		data, err := f.r.GetBlob(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("chunk %s: %w", id, err)
		}
		return data, nil
	}
	f.mu.Lock()
	k := -1
	for j := f.next; j < len(f.wants) && j < f.next+1024; j++ {
		if f.wants[j].ID == id {
			k = j
			break
		}
	}
	f.mu.Unlock()
	if k < 0 {
		data, err := f.r.GetBlob(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("chunk %s: %w", id, err)
		}
		return data, nil
	}
	for {
		data, err, at, ferr := f.take(ctx)
		if ferr != nil {
			return nil, ferr
		}
		if at == k {
			return data, err
		}
	}
}

// take waits for the next listed chunk and hands it out.
func (f *Fetcher) take(ctx context.Context) (data []byte, err error, at int, fatal error) {
	f.mu.Lock()
	for f.dispatched <= f.next {
		if f.err != nil {
			e := f.err
			f.mu.Unlock()
			return nil, nil, 0, e
		}
		if err := ctx.Err(); err != nil {
			f.mu.Unlock()
			return nil, nil, 0, err
		}
		if f.ctx.Err() != nil {
			f.mu.Unlock()
			return nil, nil, 0, f.ctx.Err()
		}
		f.cond.Wait()
	}
	it := f.items[f.next]
	f.mu.Unlock()
	select {
	case <-it.done:
	case <-ctx.Done():
		return nil, nil, 0, ctx.Err()
	}
	f.mu.Lock()
	at = f.next
	f.items[f.next] = nil
	f.next++
	it.refs--
	if it.refs == 0 {
		f.inflight -= it.reserved
		if f.pending[it.id] == it {
			delete(f.pending, it.id)
		}
	}
	if it.err != nil && f.err != nil && !f.keep {
		// Report the chunk that failed first, not the cancellation it caused.
		e := f.err
		f.cond.Broadcast()
		f.mu.Unlock()
		return nil, nil, at, e
	}
	f.cond.Broadcast()
	f.mu.Unlock()
	return it.data, it.err, at, nil
}

// Peak is the most chunk data held at once, for tests.
func (f *Fetcher) Peak() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// fetchClosed lets tests see each Fetcher when it is closed.
var fetchClosed func(*Fetcher)

// Close stops downloading and waits for every worker to stop.
func (f *Fetcher) Close() {
	f.cancel()
	f.wg.Wait()
	if fetchClosed != nil {
		fetchClosed(f)
	}
}
