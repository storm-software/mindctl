package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/domain"
)

const validResponse = `{"model":"jev-1.13.0","answers":{"minimum_tier":{"type":"choice","choice":"T4","probabilities":{"T4":0.9,"T5":0.1},"confidence":0.8},"task_type":{"type":"choice","choice":"coding","probabilities":{"coding":1},"confidence":1},"reasoning_required":{"type":"score","score":4,"legend":{"4":"average"},"probabilities":{"4":1},"confidence":0.61},"coding_required":{"type":"score","score":4,"legend":{"4":"normal"},"confidence":0.72},"blast_radius":{"type":"score","score":2,"legend":{"2":"moderate"},"confidence":0.83},"underspecified":{"type":"noul","noul":0.2}},"usage":{"input_tokens":120,"output_tokens":20}}`

func sampleInput() classifier.Input {
	return classifier.Input{Prompt: "private prompt: implement a parser", Features: domain.RequestFeatures{InputTokens: 120, ContextTokens: 400, MaxOutputTokens: 200, NeedsText: true, NeedsFunctions: true, HostedToolTypes: []string{"search"}}, CurrentModel: "current", AvailableModels: []string{"current", "other"}}
}

func testClient(url string, retries int, timeout time.Duration) *Client {
	return NewClient(config.JevConfig{BaseURL: url, Model: "jev-latest", MaxRetries: retries, Timeout: timeout}, "secret", nil)
}

// Catches wrong endpoint/auth, missing context/questions, and lossy answer mapping.
func TestClientClassifyMapsTypedAnswers(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request headers/path")
		}
		var req struct {
			Model string `json:"model"`
			State struct {
				Prompt          string                 `json:"prompt"`
				Features        domain.RequestFeatures `json:"features"`
				CurrentModel    string                 `json:"current_model"`
				AvailableModels []string               `json:"available_models"`
			} `json:"state"`
			Questions map[string]struct {
				Type         string          `json:"type"`
				Instructions string          `json:"instructions"`
				Criteria     json.RawMessage `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		input := sampleInput()
		if req.Model != "jev-latest" || req.State.Prompt != input.Prompt || !reflect.DeepEqual(req.State.Features, input.Features) || req.State.CurrentModel != input.CurrentModel || !reflect.DeepEqual(req.State.AvailableModels, input.AvailableModels) {
			t.Error("request lost model or execution context")
		}
		expected := map[string]string{"minimum_tier": "choice", "task_type": "choice", "reasoning_required": "score", "coding_required": "score", "blast_radius": "score", "underspecified": "noul"}
		if len(req.Questions) != 6 {
			t.Errorf("questions=%d", len(req.Questions))
		}
		for name, kind := range expected {
			q := req.Questions[name]
			if q.Type != kind || q.Instructions == "" {
				t.Errorf("invalid question %s", name)
			}
			if kind == "score" {
				var levels []string
				if json.Unmarshal(q.Criteria, &levels) != nil || len(levels) < 5 {
					t.Errorf("missing ordered score criteria for %s", name)
				}
			}
		}
		var tiers map[string]string
		if err := json.Unmarshal(req.Questions["minimum_tier"].Criteria, &tiers); err != nil {
			t.Fatalf("decode tier criteria: %v", err)
		}
		wantTiers := map[string]string{
			"T0": "Trivial work, extraction, and classification",
			"T1": "Normal generation",
			"T2": "Very easy reasoning and very simple coding",
			"T3": "Easy reasoning and simple coding",
			"T4": "Average reasoning and normal coding",
			"T5": "Difficult reasoning and complex coding",
			"T6": "Very difficult reasoning and very complex coding",
		}
		if !reflect.DeepEqual(tiers, wantTiers) {
			t.Errorf("tier criteria=%v, want %v", tiers, wantTiers)
		}
		var tasks map[string]any
		if json.Unmarshal(req.Questions["task_type"].Criteria, &tasks) != nil || len(tasks) != 8 {
			t.Error("invalid task criteria")
		}
		io.WriteString(w, validResponse)
	}))
	defer server.Close()
	got, err := testClient(server.URL+"/", 0, time.Second).Classify(context.Background(), sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	if got.MinimumTier != domain.T4 || got.TierConfidence != .8 || !reflect.DeepEqual(got.TierProbabilities, map[domain.Tier]float64{domain.T4: .9, domain.T5: .1}) || got.TaskType != domain.TaskCoding || got.TaskTypeConfidence != 1 || !reflect.DeepEqual(got.TaskTypeProbabilities, map[domain.TaskType]float64{domain.TaskCoding: 1}) || got.ReasoningScore != 4 || got.ReasoningConfidence != .61 || got.CodingScore != 4 || got.CodingConfidence != .72 || got.BlastRadius != 2 || got.BlastRadiusConfidence != .83 || got.Underspecified != .2 || got.ResolvedModel != "jev-1.13.0" || got.InputTokens != 120 || got.OutputTokens != 20 || got.Latency <= 0 || calls.Load() != 1 {
		t.Fatalf("judgment=%+v calls=%d", got, calls.Load())
	}
}

// Catches retrying permanent failures, unbounded retries, and missing retries.
func TestClientHTTPRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int
		wantCalls int
		success   bool
	}{
		{"429 and 529 exhausted", []int{429, 529, 529}, 3, false},
		{"retry then success", []int{429, 529, 200}, 3, true},
		{"400", []int{400}, 1, false}, {"401", []int{401}, 1, false}, {"500", []int{500}, 1, false}, {"503", []int{503}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1)) - 1
				if n >= len(tc.statuses) {
					t.Error("extra attempt")
					w.WriteHeader(500)
					return
				}
				w.WriteHeader(tc.statuses[n])
				if tc.statuses[n] == 200 {
					io.WriteString(w, validResponse)
				} else {
					io.WriteString(w, "sensitive response body")
				}
			}))
			defer server.Close()
			_, err := testClient(server.URL, 2, 2*time.Second).Classify(context.Background(), sampleInput())
			if (err == nil) != tc.success || (!tc.success && !errors.Is(err, classifier.ErrUnavailable)) || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
			if err != nil && (strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "private prompt")) {
				t.Fatalf("leaked error: %v", err)
			}
		})
	}
}

// Catches silently treating missing/null/malformed fields as zero-valued signals.
func TestClientRejectsMalformedAnswers(t *testing.T) {
	for _, tc := range []struct{ name, old, new string }{
		{"missing answer", `"underspecified":{"type":"noul","noul":0.2}`, `"unrelated":{}`},
		{"missing score", `"score":4`, `"ignored":4`}, {"null score", `"score":4`, `"score":null`},
		{"wrong answer type", `"type":"choice"`, `"type":"score"`},
		{"unknown tier", `"choice":"T4"`, `"choice":"T7"`}, {"unknown task", `"choice":"coding"`, `"choice":"other"`},
		{"missing confidence", `"confidence":0.8`, `"ignored":0.8`}, {"bad confidence", `"confidence":0.8`, `"confidence":1.1`},
		{"missing probabilities", `"probabilities":{"T4":0.9,"T5":0.1}`, `"ignored":{}`},
		{"invalid probability", `"T4":0.9`, `"T4":-0.9`}, {"bad sum", `"T4":0.9`, `"T4":0.5`},
		{"unknown distribution choice", `"T5":0.1`, `"T7":0.1`},
		{"missing chosen probability", `"choice":"T4"`, `"choice":"T3"`},
		{"bad score", `"score":4`, `"score":99`}, {"negative noul", `"noul":0.2`, `"noul":-0.2`}, {"large noul", `"noul":0.2`, `"noul":1.2`},
		{"missing score legend", `"legend":{"4":"average"}`, `"ignored":{}`},
		{"missing resolved model", `"model":"jev-1.13.0"`, `"model":""`},
		{"negative usage", `"input_tokens":120`, `"input_tokens":-1`}, {"missing usage", `"input_tokens":120`, `"ignored":120`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, strings.Replace(validResponse, tc.old, tc.new, 1))
			}))
			defer server.Close()
			got, err := testClient(server.URL, 2, time.Second).Classify(context.Background(), sampleInput())
			if !errors.Is(err, classifier.ErrUnavailable) || calls.Load() != 1 || !reflect.DeepEqual(got, domain.JevJudgment{}) {
				t.Fatalf("got=%+v err=%v calls=%d", got, err, calls.Load())
			}
		})
	}
}

// Catches missing outer timeout or cancellation while waiting between attempts.
func TestClientHonorsDeadlines(t *testing.T) {
	for _, mode := range []string{"caller", "outer", "backoff", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "backoff" {
					w.WriteHeader(429)
					return
				}
				select {
				case <-r.Context().Done():
				case <-time.After(200 * time.Millisecond):
					io.WriteString(w, validResponse)
				}
			}))
			defer server.Close()
			ctx := context.Background()
			cancel := func() {}
			timeout := time.Second
			if mode == "outer" {
				timeout = 10 * time.Millisecond
			} else {
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
			}
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			start := time.Now()
			_, err := testClient(server.URL, 20, timeout).Classify(ctx, sampleInput())
			if !errors.Is(err, classifier.ErrUnavailable) || time.Since(start) > 150*time.Millisecond {
				t.Fatalf("elapsed=%s err=%v", time.Since(start), err)
			}
			if mode == "canceled" && calls.Load() != 0 {
				t.Fatal("sent request after cancellation")
			}
			if mode == "backoff" && calls.Load() != 1 {
				t.Fatalf("backoff calls=%d", calls.Load())
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type temporaryError struct{}

func (temporaryError) Error() string   { return "private transport payload" }
func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }

// Inject only transport failures which a local HTTP server cannot reliably cause.
func TestClientRetriesOnlyTemporaryTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		calls int
	}{
		{"temporary", temporaryError{}, 3}, {"permanent", errors.New("private transport payload"), 1}, {"canceled", context.Canceled, 1}, {"deadline", context.DeadlineExceeded, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := NewClient(config.JevConfig{BaseURL: "https://example.invalid", Model: "jev-latest", MaxRetries: 2, Timeout: time.Second}, "secret", &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, tc.err })})
			_, err := client.Classify(context.Background(), sampleInput())
			if !errors.Is(err, classifier.ErrUnavailable) || calls != tc.calls || strings.Contains(err.Error(), "private") {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
