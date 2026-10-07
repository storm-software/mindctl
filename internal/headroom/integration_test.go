package headroom

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/savings"
)

func TestRealHeadroomCompressionContract(t *testing.T) {
	if os.Getenv("MINDCTL_HEADROOM_INTEGRATION") != "1" {
		t.Skip("set MINDCTL_HEADROOM_INTEGRATION=1 to run the pinned Headroom contract")
	}
	executable := os.Getenv("MINDCTL_HEADROOM_EXECUTABLE")
	if executable == "" {
		var err error
		executable, err = NewProvisioner(t.TempDir(), &http.Client{}, nil).Ensure(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	command := exec.Command(executable, "proxy", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--mode", "cache", "--no-ccr")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "HEADROOM_TELEMETRY=off", "HEADROOM_BEACON=off", "HEADROOM_LOG_MESSAGES=0"}
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	client := NewClient(baseURL, "integration-token", &http.Client{Timeout: 10 * time.Second})
	readyContext, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var lastReadyErr error
	for readyContext.Err() == nil {
		lastReadyErr = client.Ready(readyContext)
		if lastReadyErr == nil {
			break
		}
		select {
		case <-readyContext.Done():
			t.Fatalf("Headroom proxy did not become ready: %v", lastReadyErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
	request := inference.Request{Input: []inference.Item{
		{Type: "message", Role: "user", Text: "Summarize the tool output."},
		{Type: "function_call_output", CallID: "call-1", Output: jsonString(t, strings.Repeat("INFO 2026-01-01 request completed successfully, duration=12ms, status=200\n", 1500))},
	}}
	model := domain.Model{UpstreamID: "gpt-4o", Pricing: domain.Pricing{InputPerMillion: 1}}
	compressed, metrics, err := client.Compress(context.Background(), model, "conversation", "provider", request)
	if err != nil {
		t.Fatal(err)
	}
	if compressed.Input[0].Text != request.Input[0].Text || len(compressed.Input[1].Output) == 0 || string(compressed.Input[1].Output) == string(request.Input[1].Output) || metrics.TokensSaved <= 0 || metrics.TokensBefore-metrics.TokensAfter != metrics.TokensSaved {
		t.Fatalf("Headroom did not compress eligible tool output: metrics=%+v", metrics)
	}
	record := savings.Compute(savings.Input{Served: model, Compression: savings.Compression{TokensBefore: metrics.TokensBefore, TokensSaved: metrics.TokensSaved}})
	if record.CompressionTokensSaved <= 0 || record.CompressionSavings <= 0 {
		t.Fatalf("nonzero savings not recorded: %+v", record)
	}
}

func jsonString(t *testing.T, value string) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
