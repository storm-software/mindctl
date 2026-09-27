package headroom

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/config"
)

type fakeProvisioner struct{}

func (fakeProvisioner) Ensure(context.Context) (string, error) { return "headroom", nil }

type fakeProcess struct {
	done chan struct{}
	stop func()
}

func (process *fakeProcess) Wait() error {
	<-process.done
	return nil
}

func (process *fakeProcess) Kill() error {
	select {
	case <-process.done:
	default:
		process.stop()
		close(process.done)
	}
	return nil
}

// slowRunner binds the requested port only after delay, like the Python
// runtime does after launch.
type slowRunner struct {
	delay   time.Duration
	exitNow bool
}

func (runner slowRunner) Start(_ context.Context, _ string, args []string, _ []string) (Process, error) {
	var port string
	for index, arg := range args {
		if arg == "--port" {
			port = args[index+1]
		}
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[],"metrics":{}}`))
	})}
	process := &fakeProcess{done: make(chan struct{}), stop: func() { _ = server.Close() }}
	if runner.exitNow {
		close(process.done)
		return process, nil
	}
	go func() {
		time.Sleep(runner.delay)
		listener, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err == nil {
			_ = server.Serve(listener)
		}
	}()
	return process, nil
}

func TestManagerStartWaitsForSlowRuntime(t *testing.T) {
	manager := NewManager(config.HeadroomConfig{Enabled: true}, fakeProvisioner{}, &http.Client{}, slowRunner{delay: 600 * time.Millisecond})
	defer manager.Close()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestManagerStartFailsWhenRuntimeExits(t *testing.T) {
	manager := NewManager(config.HeadroomConfig{Enabled: true}, fakeProvisioner{}, &http.Client{}, slowRunner{exitNow: true})
	defer manager.Close()
	started := time.Now()
	err := manager.Start(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("start did not stop polling after the runtime exited")
	}
}
