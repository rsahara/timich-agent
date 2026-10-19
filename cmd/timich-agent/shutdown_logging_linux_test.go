package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type shutdownPipeWriter struct {
	entered chan struct{}
	once    sync.Once
}

func (w *shutdownPipeWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return os.Stderr.Write(p)
}

func TestShutdownWithFullStderrPipe(t *testing.T) {
	if scenario := os.Getenv("TIMICH_TEST_SHUTDOWN_FULL_STDERR"); scenario != "" {
		r := &shutdownTestRuntime{closeHook: func(context.Context) error {
			fmt.Fprintln(os.Stdout, "CLOSED")
			if scenario == "cleanup_error" {
				return errors.New("cleanup failed")
			}
			return nil
		}}
		if strings.Contains(scenario, "deadline") || scenario == "logger_locked" {
			_, _, _ = r.work.Acquire()
		}
		if scenario == "logger_locked" {
			writer := &shutdownPipeWriter{entered: make(chan struct{})}
			log.SetOutput(writer)
			go log.Print("hold the logger mutex while stderr is full")
			<-writer.entered
		}
		c := newShutdownCoordinator(r, &http.Server{}, &http.Server{})
		c.drainTimeout, c.totalTimeout = 10*time.Millisecond, 150*time.Millisecond
		if scenario == "signal_clean_short_budget" {
			c.totalTimeout = 20 * time.Millisecond
		}
		var err error
		switch {
		case strings.HasPrefix(scenario, "signal"):
			signalCh := make(chan os.Signal, 1)
			signalCh <- syscall.SIGTERM
			err = waitForShutdown(signalCh, nil, nil, c)
		case strings.HasPrefix(scenario, "restart"):
			restart := make(chan struct{}, 1)
			restart <- struct{}{}
			err = waitForShutdown(nil, restart, nil, c)
		case scenario == "listen_error":
			failed := make(chan error, 1)
			failed <- errors.New("listen failed")
			err = waitForShutdown(nil, nil, failed, c)
		default:
			err = c.shutdown()
		}
		if err != nil {
			exitCLIError(err)
		}
		os.Exit(0)
	}

	for _, scenario := range []struct {
		name   string
		code   int
		closed bool
	}{
		{"deadline", 1, false},
		{"logger_locked", 1, false},
		{"signal_deadline", 1, false},
		{"restart_deadline", 1, false},
		{"signal_clean", 0, true},
		{"signal_clean_short_budget", 0, true},
		{"restart_clean", 0, true},
		{"listen_error", 1, true},
		{"cleanup_error", 1, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			fd := int(writer.Fd())
			if err := syscall.SetNonblock(fd, true); err != nil {
				t.Fatal(err)
			}
			chunk := bytes.Repeat([]byte{'x'}, 4096)
			for {
				_, err := syscall.Write(fd, chunk)
				if err == syscall.EAGAIN {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := syscall.SetNonblock(fd, false); err != nil {
				t.Fatal(err)
			}
			// No reader drains this pipe until the test ends. Passing the file
			// directly also keeps exec.Cmd from waiting for a stderr copy worker.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownWithFullStderrPipe$")
			cmd.Env = append(os.Environ(), "TIMICH_TEST_SHUTDOWN_FULL_STDERR="+scenario.name,
				"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
			cmd.Stderr = writer
			var output bytes.Buffer
			cmd.Stdout = &output
			err = cmd.Run()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if ctx.Err() != nil || code != scenario.code {
				t.Fatalf("exit=%d, want %d; watchdog=%v; output=%s", code, scenario.code, ctx.Err(), output.String())
			}
			if closed := strings.Contains(output.String(), "CLOSED"); closed != scenario.closed {
				t.Fatalf("cleanup=%v, want %v; output=%s", closed, scenario.closed, output.String())
			}
		})
	}
}
