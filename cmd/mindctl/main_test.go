package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/config"
	"gopkg.in/yaml.v3"
)

func mainConfig(t *testing.T) string {
	t.Helper()
	t.Setenv("MAIN_GATEWAY_TOKEN", "private-gateway-token")
	t.Setenv("MAIN_JEV_KEY", "private-jev-key")
	t.Setenv("MAIN_PROVIDER_KEY", "private-provider-key")
	t.Setenv("MAIN_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	cfg := config.Config{
		Listen: "127.0.0.1:0", ClientAuth: config.ClientAuthConfig{TokenEnv: "MAIN_GATEWAY_TOKEN"},
		Jev:        config.JevConfig{BaseURL: "https://jev.example.com", Model: "jev", APIKeyEnv: "MAIN_JEV_KEY"},
		SQLite:     config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "app.db")},
		Encryption: config.EncryptionConfig{ActiveKeyID: "active", Keys: map[string]string{"active": "MAIN_ENCRYPTION_KEY"}},
		Routing:    config.RoutingConfig{MinTier: "T0", MaxTier: "T6"},
		Providers:  []config.ProviderConfig{{ID: "openai", BaseURL: "https://provider.example.com", APIKeyEnv: "MAIN_PROVIDER_KEY"}},
		Models:     []config.ModelConfig{{ID: "model", Provider: "openai", Tier: "T4", Available: true, SuccessPrior: 1}},
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRejectsBadFlagsAndMissingSecrets(t *testing.T) {
	path := mainConfig(t)
	t.Setenv("MAIN_ENCRYPTION_KEY", "")
	for _, args := range [][]string{{"-unknown"}, {"unexpected"}, {"-config", path}} {
		var stderr bytes.Buffer
		err := run(context.Background(), args, &stderr)
		if err == nil {
			t.Fatalf("run(%v) succeeded", args)
		}
		if len(args) == 2 && !strings.Contains(err.Error(), "MAIN_ENCRYPTION_KEY") {
			t.Fatalf("missing key error = %v", err)
		}
		if strings.Contains(err.Error()+stderr.String(), "private-") {
			t.Fatal("startup exposed a secret")
		}
	}
}

func TestServeGracefullyWaitsForActiveRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, shuttingDown := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})}
	server.RegisterOnShutdown(func() { close(shuttingDown) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, time.Second) }()
	response := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			response <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		response <- string(body)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-shuttingDown:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown returned while request was active: %v", err)
	default:
	}
	close(release)
	select {
	case body := <-response:
		if body != "finished" {
			t.Fatalf("response = %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestServeShutdownDeadlineAndServeFailureAreErrors(t *testing.T) {
	t.Run("serve failure", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_ = listener.Close()
		if err := serve(context.Background(), &http.Server{}, listener, time.Second); err == nil {
			t.Fatal("serve failure was ignored")
		}
	})
	t.Run("shutdown deadline", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(started); <-release })}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		done := make(chan error, 1)
		go func() { done <- serve(ctx, server, listener, 20*time.Millisecond) }()
		go func() {
			resp, err := http.Get("http://" + listener.Addr().String())
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not start")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "shutdown") {
				t.Fatalf("shutdown error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown was unbounded")
		}
	})
}

func TestBinarySignalsAndContentFreeStartup(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mindctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			path := mainConfig(t)
			cmd := exec.Command(binary, "-config", path)
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			lines := make(chan string, 8)
			go func() {
				scanner := bufio.NewScanner(stderr)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
				close(lines)
			}()
			var logs, address string
			deadline := time.After(5 * time.Second)
			for address == "" {
				select {
				case line, ok := <-lines:
					if !ok {
						t.Fatalf("process stopped before listening: %s", logs)
					}
					logs += line + "\n"
					if strings.HasPrefix(line, "mindctl: listening on ") {
						address = strings.TrimPrefix(line, "mindctl: listening on ")
					}
				case <-deadline:
					t.Fatal("process did not start")
				}
			}
			if !strings.Contains(logs, "unlimited") || !strings.Contains(logs, "disk") || !strings.Contains(logs, "governance") || strings.Contains(logs, "private-") {
				t.Fatalf("startup logs = %q", logs)
			}
			client := &http.Client{Timeout: time.Second}
			for _, route := range []string{"/healthz", "/readyz"} {
				resp, err := client.Get("http://" + address + route)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("%s status = %d", route, resp.StatusCode)
				}
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(7 * time.Second):
				t.Fatal("process ignored shutdown signal")
			}
		})
	}
	t.Run("startup failure exits nonzero", func(t *testing.T) {
		path := mainConfig(t)
		t.Setenv("MAIN_ENCRYPTION_KEY", "")
		cmd := exec.Command(binary, "-config", path)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("missing key exited successfully")
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "MAIN_ENCRYPTION_KEY") || strings.Contains(stderr.String(), "private-") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})
}
