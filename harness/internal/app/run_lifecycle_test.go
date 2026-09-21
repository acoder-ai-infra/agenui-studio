package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLifecycleCloseCancelsAndWaits(t *testing.T) {
	lifecycle := newRunLifecycle()
	runCtx, release, err := lifecycle.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer release()
		<-runCtx.Done()
	}()

	if err := lifecycle.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before the active run exited")
	}
	if _, _, err := lifecycle.Start(context.Background()); !errors.Is(err, errRunLifecycleClosing) {
		t.Fatalf("Start after Close error = %v, want lifecycle closing", err)
	}
}

func TestRunLifecycleCloseTimeoutIsBounded(t *testing.T) {
	lifecycle := newRunLifecycle()
	_, release, err := lifecycle.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	started := time.Now()
	err = lifecycle.Close(20 * time.Millisecond)
	if !errors.Is(err, errRunLifecycleShutdownTimeout) {
		t.Fatalf("Close error = %v, want shutdown timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Close timeout was not bounded: %v", elapsed)
	}
}

func TestAppCloseDrainsRunsBeforeResources(t *testing.T) {
	lifecycle := newRunLifecycle()
	runCtx, release, err := lifecycle.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var resourcesClosed atomic.Bool
	var observedClosedResource atomic.Bool
	go func() {
		<-runCtx.Done()
		observedClosedResource.Store(resourcesClosed.Load())
		release()
	}()

	application := &App{
		runLifecycle: lifecycle,
		closeResources: func() error {
			resourcesClosed.Store(true)
			return nil
		},
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if observedClosedResource.Load() {
		t.Fatal("run observed resources closed before it exited")
	}
	if !resourcesClosed.Load() {
		t.Fatal("resources were not closed after run drain")
	}
}

func TestAppCloseDoesNotReleaseResourcesWhenRunDrainTimesOut(t *testing.T) {
	lifecycle := newRunLifecycle()
	_, release, err := lifecycle.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var resourcesClosed atomic.Bool
	application := &App{
		runLifecycle:    lifecycle,
		runDrainTimeout: 20 * time.Millisecond,
		closeResources: func() error {
			resourcesClosed.Store(true)
			return nil
		},
	}
	if err := application.Close(); !errors.Is(err, errRunLifecycleShutdownTimeout) {
		t.Fatalf("Close error = %v, want shutdown timeout", err)
	}
	if resourcesClosed.Load() {
		t.Fatal("resources were closed while an active run could still use them")
	}
}
