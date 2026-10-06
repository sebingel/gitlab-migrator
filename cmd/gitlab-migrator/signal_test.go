package main

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

// TestWatchInterrupts_SecondInterruptExits sends two interrupts. The first
// one must cancel the context and must not exit, so that the migration can
// stop cleanly. The second one must exit at once, because a step that does
// not watch the context would otherwise keep the process running.
func TestWatchInterrupts_SecondInterruptExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal)
	stop := make(chan struct{})
	defer close(stop)
	exitCodes := make(chan int, 1)
	var out bytes.Buffer

	go watchInterrupts(sigs, stop, cancel, &out, func(code int) { exitCodes <- code })

	sigs <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the first interrupt did not cancel the context")
	}
	select {
	case code := <-exitCodes:
		t.Fatalf("the first interrupt exited with code %d", code)
	case <-time.After(20 * time.Millisecond):
	}

	sigs <- os.Interrupt
	select {
	case code := <-exitCodes:
		if code != interruptExitCode {
			t.Errorf("exit code = %d, want %d", code, interruptExitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second interrupt did not exit")
	}
}

// TestWatchInterrupts_ReturnsWhenStopped closes stop before and after the
// first interrupt. watchInterrupts must return without exit in both cases,
// and without cancel when no interrupt came.
func TestWatchInterrupts_ReturnsWhenStopped(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan os.Signal)
		stop := make(chan struct{})
		exited := false
		done := make(chan struct{})
		go func() {
			watchInterrupts(sigs, stop, cancel, &bytes.Buffer{}, func(int) { exited = true })
			close(done)
		}()

		if interrupt {
			sigs <- os.Interrupt
		}
		close(stop)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("interrupt=%v: watchInterrupts did not return after stop was closed", interrupt)
		}
		if exited {
			t.Errorf("interrupt=%v: watchInterrupts called exit", interrupt)
		}
		if got := ctx.Err() != nil; got != interrupt {
			t.Errorf("interrupt=%v: context canceled = %v, want %v", interrupt, got, interrupt)
		}
		cancel()
	}
}
