package memorybacking

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrRegistryDraining = errors.New("memory backing registry is draining")

// Backing owns only the device. Successful Close must drain I/O and disconnect,
// including cancellation cleanup. Close errors quarantine the source and key;
// the registry alone closes the source after a successful device disconnect.
// Ready closes after capacity, identity, and connection checks. Done broadcasts
// terminal service completion; Err must be safe to call after Done closes.
type Backing interface {
	Path() string
	Ready() <-chan struct{}
	Done() <-chan struct{}
	Err() error
	Close(context.Context) error
}

type SourceOpener func(context.Context, SnapshotIdentity) (SnapshotSource, error)
type Factory func(context.Context, SnapshotSource) (Backing, error)

type Registry struct {
	mu          sync.Mutex
	entries     map[SnapshotIdentity]*entry
	open        SourceOpener
	factory     Factory
	draining    bool
	drainClosed bool
	drained     chan struct{}
	closeErr    error
}

type entry struct {
	id                  SnapshotIdentity
	ctx                 context.Context
	cancel              context.CancelFunc
	ready, closed, done chan struct{}
	refs                int
	finished, closing   bool
	cleanupDone         bool
	err, closeErr       error
	source              SnapshotSource
	backing             Backing
	open                SourceOpener
	leases              map[*Lease]struct{}
}

func NewRegistry(open SourceOpener, factory Factory) *Registry {
	return &Registry{entries: make(map[SnapshotIdentity]*entry), open: open, factory: factory, drained: make(chan struct{})}
}

// Acquire reserves a reference while waiting, independently of other callers'
// contexts. DRAINING entries are never reused: callers wait for disconnect and
// source close before retrying creation, preventing NBD number retarget races.
func (r *Registry) Acquire(ctx context.Context, id SnapshotIdentity) (*Lease, error) {
	return r.AcquireWithSource(ctx, id, r.open)
}

// AcquireWithSource uses open only if this caller creates the entry. The opener
// receives the entry's lifetime context; it must not capture a caller context or
// transfer a source that the caller may independently close. Existing entries
// reuse their original immutable source without opening another template view.
func (r *Registry) AcquireWithSource(ctx context.Context, id SnapshotIdentity, open SourceOpener) (*Lease, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		if r.draining {
			r.mu.Unlock()
			return nil, ErrRegistryDraining
		}
		e := r.entries[id]
		if e != nil && e.closing {
			if e.cleanupDone && e.closeErr != nil {
				err := fmt.Errorf("memory backing quarantined after unconfirmed cleanup; operator confirmation or process restart required: %w", e.closeErr)
				r.mu.Unlock()
				return nil, err
			}
			closed := e.closed
			r.mu.Unlock()
			select {
			case <-closed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if e == nil {
			createCtx, cancel := context.WithCancel(context.Background())
			e = &entry{id: id, ctx: createCtx, cancel: cancel, ready: make(chan struct{}), closed: make(chan struct{}), done: make(chan struct{}), leases: make(map[*Lease]struct{}), open: open}
			r.entries[id] = e
			go r.create(e)
		}
		e.refs++
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			r.drop(e)
			return nil, ctx.Err()
		case <-e.ready:
		}
		r.mu.Lock()
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			r.drop(e)
			return nil, err
		}
		if e.err != nil {
			err := e.err
			r.mu.Unlock()
			r.drop(e)
			return nil, err
		}
		l := &Lease{registry: r, entry: e, failure: make(chan error, 1)}
		e.leases[l] = struct{}{}
		r.mu.Unlock()
		return l, nil
	}
}

func (r *Registry) create(e *entry) {
	var source SnapshotSource
	var backing Backing
	var err error
	if e.open == nil || r.factory == nil {
		err = errors.New("memory backing opener and factory are required")
	} else {
		source, err = e.open(e.ctx, e.id)
		if err == nil && source == nil {
			err = errors.New("nil snapshot source")
		}
		if err == nil && source.Identity() != e.id {
			err = errors.New("snapshot source identity differs from requested manifest")
		}
		if err == nil {
			backing, err = r.factory(e.ctx, source)
		}
		if err == nil && backing == nil {
			err = errors.New("nil memory backing")
		}
		if err == nil {
			select {
			case <-e.ctx.Done():
				err = e.ctx.Err()
			case <-backing.Done():
				err = backing.Err()
				if err == nil {
					err = errors.New("memory backing stopped before ready")
				}
			case <-backing.Ready():
				if backing.Path() == "" {
					err = errors.New("ready memory backing has no path")
				}
			}
		}
	}
	r.mu.Lock()
	e.source, e.backing, e.finished = source, backing, true
	if err != nil {
		r.failLocked(e, fmt.Errorf("create memory backing: %w", err))
		e.closing = true
	}
	close(e.ready)
	closing := e.closing
	r.mu.Unlock()
	if closing {
		r.cleanup(e)
		return
	}
	go func() {
		select {
		case <-e.ctx.Done():
			return
		case <-backing.Done():
			err := backing.Err()
			if err == nil {
				err = errors.New("memory backing stopped unexpectedly")
			}
			r.mu.Lock()
			if !e.closing {
				r.failLocked(e, err)
			}
			r.mu.Unlock()
		}
	}()
}

func (r *Registry) failLocked(e *entry, err error) {
	if e.err != nil {
		return
	}
	e.err = err
	close(e.done)
	for l := range e.leases {
		l.failure <- err
		close(l.failure)
	}
}

func (r *Registry) drop(e *entry) bool {
	r.mu.Lock()
	e.refs--
	last := e.refs == 0
	if last && !e.closing {
		e.closing = true
		if e.finished {
			go r.cleanup(e)
		} else {
			// Cancel an unfinished open so its creator can reach cleanup. A
			// running transport must remain alive until Close disconnects it.
			e.cancel()
		}
	}
	r.mu.Unlock()
	return last
}

func (r *Registry) cleanup(e *entry) {
	var err error
	if e.backing != nil {
		err = e.backing.Close(context.Background())
	}
	if err == nil {
		// Close must disconnect while the transport still owns the device.
		// On failure retain its context as well as its source for quarantine.
		e.cancel()
	}
	// A failed disconnect may leave kernel I/O referencing this source. Keep
	// the source and identity quarantined rather than retargeting/reusing them.
	if err == nil && e.source != nil {
		err = errors.Join(err, e.source.Close())
	}
	r.mu.Lock()
	e.closeErr = err
	e.cleanupDone = true
	if err != nil {
		r.failLocked(e, fmt.Errorf("memory backing cleanup unconfirmed: %w", err))
	}
	if e.err == nil {
		close(e.done)
	}
	for l := range e.leases {
		if e.err == nil {
			close(l.failure)
		}
	}
	if err == nil && r.entries[e.id] == e {
		delete(r.entries, e.id)
	}
	r.closeErr = errors.Join(r.closeErr, err)
	close(e.closed)
	r.finishDrainLocked()
	r.mu.Unlock()
}

func (r *Registry) finishDrainLocked() {
	if !r.draining || r.drainClosed {
		return
	}
	for _, e := range r.entries {
		if !e.cleanupDone {
			return
		}
	}
	r.drainClosed = true
	close(r.drained)
}

// Drain rejects new users immediately, then waits for leases/exporters to release.
// A canceled caller stops waiting; cleanup continues and Drain may be retried.
// A nonnil result may mean disconnect is unconfirmed: the entry and source stay
// quarantined and cannot be retargeted. Drain returning an error is not evidence
// of safe device teardown; recovery requires operator confirmation or restart.
func (r *Registry) Drain(ctx context.Context) error {
	r.mu.Lock()
	if !r.draining {
		r.draining = true
		r.finishDrainLocked()
	}
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.drained:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}

type Lease struct {
	registry *Registry
	entry    *entry
	failure  chan error
	once     sync.Once
	last     bool
}

func (l *Lease) Path() string               { return l.entry.backing.Path() }
func (l *Lease) Identity() SnapshotIdentity { return l.entry.id }
func (l *Lease) Ready() <-chan struct{}     { return l.entry.ready }
func (l *Lease) Done() <-chan struct{}      { return l.entry.done }
func (l *Lease) Failure() <-chan error      { return l.failure }
func (l *Lease) Err() error                 { l.registry.mu.Lock(); defer l.registry.mu.Unlock(); return l.entry.err }

// Release relinquishes the mapping/exporter reference exactly once. Call only
// after every mapping and I/O user of this lease has stopped. Final release waits
// for full disconnect; a timeout never aborts background cleanup.
func (l *Lease) Release(ctx context.Context) error {
	l.once.Do(func() {
		l.registry.mu.Lock()
		delete(l.entry.leases, l)
		if l.entry.err == nil {
			close(l.failure)
		}
		l.registry.mu.Unlock()
		l.last = l.registry.drop(l.entry)
	})
	if !l.last {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.entry.closed:
		return l.entry.closeErr
	}
}
