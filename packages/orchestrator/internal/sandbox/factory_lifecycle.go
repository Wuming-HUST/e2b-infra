package sandbox

import (
	"context"
	"errors"
	"sync"
)

var errFactoryDraining = errors.New("sandbox factory is shutting down")

// factoryLifecycle fences startup before shutdown takes a snapshot of running
// sandboxes. Startups either finish cleanup or atomically become tracked users;
// shutdown cannot miss a VM between startup and normal lifecycle registration.
// Close callbacks must return only after their process/mappings have exited.
type factoryLifecycle struct {
	mu        sync.Mutex
	closing   bool
	starts    map[*factoryStartup]struct{}
	running   map[*factoryStartup]func(context.Context) error
	changed   chan struct{}
	drainOnce sync.Once
	drainErr  error
	closeErr  error
}

type factoryStartup struct {
	owner  *factoryLifecycle
	cancel context.CancelCauseFunc
	once   sync.Once
}

func (l *factoryLifecycle) initLocked() {
	if l.changed == nil {
		l.changed = make(chan struct{})
		l.starts = make(map[*factoryStartup]struct{})
		l.running = make(map[*factoryStartup]func(context.Context) error)
	}
}

func (l *factoryLifecycle) notifyLocked() { close(l.changed); l.changed = make(chan struct{}) }

func (l *factoryLifecycle) begin(ctx context.Context) (context.Context, *factoryStartup, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.initLocked()
	if l.closing {
		return nil, nil, errFactoryDraining
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	startupCtx, cancel := context.WithCancelCause(ctx)
	s := &factoryStartup{owner: l, cancel: cancel}
	l.starts[s] = struct{}{}
	return startupCtx, s, nil
}

// finish must run after any startup-error cleanup. attach publishes the removal
// hook before shutdown can observe the new sandbox; it must only assign the hook.
func (s *factoryStartup) finish(closeSandbox func(context.Context) error, attach func(func(error)), cleanupErrors ...error) {
	s.once.Do(func() {
		l := s.owner
		l.mu.Lock()
		for _, err := range cleanupErrors {
			l.closeErr = errors.Join(l.closeErr, err)
		}
		if closeSandbox != nil {
			remove := func(err error) {
				l.mu.Lock()
				if _, exists := l.running[s]; exists {
					delete(l.running, s)
					l.closeErr = errors.Join(l.closeErr, err)
				}
				l.notifyLocked()
				l.mu.Unlock()
			}
			if attach != nil {
				attach(remove)
			}
			l.running[s] = func(ctx context.Context) error { err := closeSandbox(ctx); remove(err); return err }
		}
		delete(l.starts, s)
		l.notifyLocked()
		l.mu.Unlock()
		s.cancel(nil)
	})
}

func (l *factoryLifecycle) beginShutdown() {
	l.mu.Lock()
	l.initLocked()
	l.closing = true
	cancels := make([]context.CancelCauseFunc, 0, len(l.starts))
	for s := range l.starts {
		cancels = append(cancels, s.cancel)
	}
	l.mu.Unlock()
	for _, cancel := range cancels {
		cancel(errFactoryDraining)
	}
}

func (l *factoryLifecycle) drain(ctx context.Context) error {
	// Even ForceStop/canceled RPC contexts must not release backing resources
	// while FC still maps them. Callers wait for safe cleanup, without a timeout.
	ctx = context.WithoutCancel(ctx)
	l.beginShutdown()
	l.drainOnce.Do(func() {
		l.mu.Lock()
		for len(l.starts) != 0 {
			changed := l.changed
			l.mu.Unlock()
			<-changed
			l.mu.Lock()
		}
		closers := make([]func(context.Context) error, 0, len(l.running))
		for _, closeSandbox := range l.running {
			closers = append(closers, closeSandbox)
		}
		l.mu.Unlock()
		for _, closeSandbox := range closers {
			_ = closeSandbox(ctx)
		}
		l.mu.Lock()
		l.drainErr = l.closeErr
		l.mu.Unlock()
	})
	return l.drainErr
}
