package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	errRunLifecycleClosing         = errors.New("run lifecycle is closing")
	errRunLifecycleShutdownTimeout = errors.New("run lifecycle shutdown timed out")
)

const defaultRunShutdownTimeout = 10 * time.Second

// runLifecycle owns detached inline Run work. It gives request-independent work
// one App-level cancellation root and guarantees resources are released only
// after active drains have exited or the bounded shutdown deadline is reached.
type runLifecycle struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup

	closeOnce   sync.Once
	closeResult error
}

func newRunLifecycle() *runLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &runLifecycle{ctx: ctx, cancel: cancel}
}

func (l *runLifecycle) Start(parent context.Context) (context.Context, func(), error) {
	if l == nil {
		return nil, nil, errRunLifecycleClosing
	}
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return nil, nil, errRunLifecycleClosing
	}
	l.wg.Add(1)
	l.mu.Unlock()

	runCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stopRootCancel := context.AfterFunc(l.ctx, cancel)
	var once sync.Once
	release := func() {
		once.Do(func() {
			stopRootCancel()
			cancel()
			l.wg.Done()
		})
	}
	return runCtx, release, nil
}

func (l *runLifecycle) Close(timeout time.Duration) error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closing = true
		l.cancel()
		l.mu.Unlock()
		if timeout <= 0 {
			timeout = defaultRunShutdownTimeout
		}
		done := make(chan struct{})
		go func() {
			l.wg.Wait()
			close(done)
		}()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			l.closeResult = errRunLifecycleShutdownTimeout
		}
	})
	return l.closeResult
}
