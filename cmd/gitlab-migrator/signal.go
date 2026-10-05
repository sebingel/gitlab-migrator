package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

// interruptExitCode is the exit code after a second interrupt: 128 plus the
// number of SIGINT, like a shell reports a process that SIGINT ended.
const interruptExitCode = 130

// watchInterrupts cancels the migration at the first signal from sigs, so
// that it can stop cleanly, and calls exit at the second one. A step that does
// not watch the context can keep the process running after the first signal;
// the second one ends it at once, without deferred cleanup. watchInterrupts
// returns when stop is closed.
func watchInterrupts(sigs <-chan os.Signal, stop <-chan struct{}, cancel context.CancelFunc, out io.Writer, exit func(code int)) {
	select {
	case <-sigs:
	case <-stop:
		return
	}
	_, _ = fmt.Fprintln(out, "Interrupt received, stopping the migration. Press Ctrl+C again to stop at once.")
	cancel()

	select {
	case <-sigs:
		_, _ = fmt.Fprintln(out, "Second interrupt received, stopping at once.")
		exit(interruptExitCode)
	case <-stop:
	}
}
