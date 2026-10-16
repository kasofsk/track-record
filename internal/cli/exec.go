package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
)

// outcome is how a wrapped command ended.
type outcome struct {
	code  int   // its exit code, or 128 + the signal that killed it
	err   error // it could not start
	ioErr error // its output could not be passed on in full
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

// interrupts catches SIGINT and SIGTERM until stop, passing each on to the
// command execute is running.
type interrupts struct {
	signals chan os.Signal
	seen    bool
	stopped bool
}

func catchInterrupts() *interrupts {
	i := &interrupts{signals: make(chan os.Signal, 1)}
	signal.Notify(i.signals, os.Interrupt, syscall.SIGTERM)
	return i
}

// stop ends the catching and reports whether a signal was caught. Once it
// returns, SIGINT and SIGTERM take their default action again.
func (i *interrupts) stop() bool {
	if !i.stopped {
		i.stopped = true
		// Stop returns only once every signal already received has been
		// delivered to the channel, so this receive sees any that arrived
		// before it.
		signal.Stop(i.signals)
		select {
		case <-i.signals:
			i.seen = true
		default:
		}
	}
	return i.seen
}

// watch returns a context that is cancelled once a signal is caught, and a
// function that ends the watch, which must be called before stop.
func (i *interrupts) watch(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	if i.seen {
		cancel()
		return ctx, cancel
	}
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-i.signals:
			i.seen = true
			cancel()
		case <-done:
		}
	}()
	return ctx, func() {
		close(done)
		<-watched
		cancel()
	}
}

// childEnv is the environment a command runs with: this process's, without
// the store's token, which would otherwise reach a log that every token in
// the namespace can read, and with TRACK_RECORD_KEY set to the key it runs
// under, or unset if there is none.
func childEnv(key string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "TRACK_RECORD_TOKEN" || name == "TRACK_RECORD_TOKEN_FILE" || name == "TRACK_RECORD_KEY"
	})
	if key != "" {
		env = append(env, "TRACK_RECORD_KEY="+key)
	}
	return env
}

// execute runs argv with env and the given streams, passing on each signal
// caught while it runs.
func (i *interrupts) execute(argv, env []string, stdin io.Reader, stdout, stderr io.Writer) outcome {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return outcome{err: err}
	}
	done, forwarded := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(forwarded)
		for {
			select {
			case s := <-i.signals:
				i.seen = true
				cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	<-forwarded
	o := outcome{code: cmd.ProcessState.ExitCode()}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		o.code = 128 + int(ws.Signal())
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		o.ioErr = err
	}
	return o
}

// execute runs argv, under no key, with the given streams, passing on
// SIGINT and SIGTERM while it runs.
func execute(argv []string, stdin io.Reader, stdout, stderr io.Writer) outcome {
	i := catchInterrupts()
	defer i.stop()
	return i.execute(argv, childEnv(""), stdin, stdout, stderr)
}
