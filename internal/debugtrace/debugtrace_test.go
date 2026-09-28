package debugtrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestSaveBodyWritesPrivateFileBesideTrace(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	trace, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer trace.Close()
	first, err := trace.SaveBody([]byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("SaveBody: %v", err)
	}
	second, err := trace.SaveBody([]byte(`{"b":2}`))
	if err != nil || first == second {
		t.Fatalf("second=%q err=%v", second, err)
	}
	if want := strings.TrimSuffix(trace.Path(), ".jsonl") + "-bodies"; filepath.Dir(first) != want {
		t.Fatalf("body directory = %q; want %q", filepath.Dir(first), want)
	}
	info, err := os.Stat(first)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("body file info=%v err=%v", info, err)
	}
	directory, err := os.Stat(filepath.Dir(first))
	if err != nil || directory.Mode().Perm() != 0700 {
		t.Fatalf("body directory info=%v err=%v", directory, err)
	}
	if body, _ := os.ReadFile(first); string(body) != `{"a":1}` {
		t.Fatalf("body = %s", body)
	}
}
