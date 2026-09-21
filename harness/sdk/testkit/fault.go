package testkit

import (
	"context"
	"errors"
	"time"
)

// FaultKind labels a canned failure mode injected into a scripted stream.
type FaultKind string

const (
	// FaultTimeout returns context.DeadlineExceeded after Delay.
	FaultTimeout FaultKind = "timeout"
	// FaultPanic panics inside the Fault callable. The MockEngine catches it
	// and marks the stream terminal.
	FaultPanic FaultKind = "panic"
	// FaultStreamBreak short-circuits the stream to io.EOF after Delay.
	FaultStreamBreak FaultKind = "stream_break"
	// FaultBackpressure sleeps for Delay before returning, exercising the
	// caller's timeout / cancellation path.
	FaultBackpressure FaultKind = "backpressure"
)

// Fault is a single scripted failure. Used with StreamPolicy or a MockTool
// script to reproduce edge cases.
type Fault struct {
	Kind    FaultKind
	Delay   time.Duration
	Message string
}

// ErrScriptedPanic is what a FaultPanic recovers to when propagated back
// through a defer/recover boundary.
var ErrScriptedPanic = errors.New("testkit: scripted panic")

// Apply runs the fault under ctx. Callers use it inside their own hook points
// (extension implementations, custom tools, etc.) to inject deterministic
// failures.
func (f Fault) Apply(ctx context.Context) error {
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	switch f.Kind {
	case FaultTimeout:
		return context.DeadlineExceeded
	case FaultPanic:
		panic(ErrScriptedPanic)
	case FaultStreamBreak:
		return errors.New("testkit: stream break")
	case FaultBackpressure:
		return nil
	}
	if f.Message != "" {
		return errors.New(f.Message)
	}
	return nil
}
