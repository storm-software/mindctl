package headroom

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

func TestRealHeadroomCompressionContract(t *testing.T) {
	if os.Getenv("MINDCTL_HEADROOM_INTEGRATION") != "1" {
		t.Skip("set MINDCTL_HEADROOM_INTEGRATION=1 to run the pinned Headroom contract")
	}
	cache := t.TempDir()
	executable, err := NewProvisioner(cache, &http.Client{}, nil).Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
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
	readyContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for readyContext.Err() == nil {
		if err := client.Ready(readyContext); err == nil {
			break
		}
		select {
		case <-readyContext.Done():
			t.Fatal("Headroom proxy did not become ready")
		case <-time.After(250 * time.Millisecond):
		}
	}
	request := inference.Request{Input: []inference.Item{
		{Type: "message", Role: "assistant", Text: "A long assistant context that should be safely compressed."},
		{Type: "function_call_output", Text: "A string tool result that should survive the round trip."},
	}}
	compressed, _, err := client.Compress(context.Background(), domain.Model{UpstreamID: "integration-model"}, "conversation", "provider", request)
	if err != nil {
		t.Fatal(err)
	}
	if compressed.Input[0].Text == "" || compressed.Input[1].Text == "" {
		t.Fatal("Headroom returned empty protected text")
	}
}
