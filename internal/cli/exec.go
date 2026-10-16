package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// outcome is how a wrapped command ended.
type outcome struct {
	code        int   // its exit code, or 128 + the signal that killed it
	interrupted bool  // a signal was forwarded to it
	err         error // it could not start
	ioErr       error // its output could not be passed on in full
}

// exit reports a command that could not start and returns the exit code
// track-record ends with.
func (o outcome) exit(stderr io.Writer) int {
	if o.err != nil {
		fmt.Fprintf(stderr, "track-record: %v\n", o.err)
		return 2
	}
	return o.code
}

// execute runs argv with the given streams, forwarding SIGINT and SIGTERM to
// it while it runs.
func execute(argv []string, stdin io.Reader, stdout, stderr io.Writer) outcome {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return outcome{err: err}
	}
	var interrupted atomic.Bool
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-signals:
				interrupted.Store(true)
				cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	o := outcome{code: cmd.ProcessState.ExitCode(), interrupted: interrupted.Load()}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		o.code = 128 + int(ws.Signal())
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		o.ioErr = err
	}
	return o
}
