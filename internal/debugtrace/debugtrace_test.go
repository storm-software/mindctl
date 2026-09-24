package debugtrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesPrivateJSONLTraceInXDGCache(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)

	trace, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	path := trace.Path()
	trace.Logger().Debug("route.test", "model", "gpt-test")
	if err := trace.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wantDirectory := filepath.Join(cacheHome, "mindctl", "logs")
	if filepath.Dir(path) != wantDirectory {
		t.Fatalf("trace directory = %q; want %q", filepath.Dir(path), wantDirectory)
	}
	directoryInfo, err := os.Stat(wantDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("trace directory permissions = %o; want 700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("trace file permissions = %o; want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("trace is not JSONL: %v\n%s", err, body)
	}
	if event["msg"] != "route.test" || event["model"] != "gpt-test" {
		t.Fatalf("trace event = %#v", event)
	}
}
