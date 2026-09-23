package laya

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/domain"
)

const validResponse = `{"schema_version":"mindctl.classifier.v1","classifier":{"name":"laya","repository":"convaiinnovations/laya","revision":"5e7b2b1b8ca2ecdd3f2322d94069c9b6ce7e844b","variant":"typed-decisions"},"answers":{"minimum_tier":{"type":"choice","choice":"T4","confidence":0.9,"probabilities":{"T0":0,"T1":0,"T2":0,"T3":0.1,"T4":0.9,"T5":0,"T6":0}},"task_type":{"type":"choice","choice":"coding","confidence":1,"probabilities":{"unknown":0,"extraction":0,"classification":0,"generation":0,"reasoning":0,"coding":1,"tool_use":0,"multimodal":0}},"reasoning_required":{"type":"score","score":3,"confidence":0.8,"probabilities":{"0":0,"1":0,"2":0,"3":1,"4":0,"5":0,"6":0},"legend":{"0":"None","1":"Minimal","2":"Simple","3":"Moderate","4":"Substantial","5":"Difficult","6":"Exceptional"}},"coding_required":{"type":"score","score":3,"confidence":0.8,"probabilities":{"0":0,"1":0,"2":0,"3":1,"4":0,"5":0,"6":0},"legend":{"0":"None","1":"Trivial edit","2":"Simple code","3":"Bounded implementation","4":"Substantial implementation","5":"Complex architecture","6":"Exceptional engineering difficulty"}},"blast_radius":{"type":"score","score":1,"confidence":0.8,"probabilities":{"0":0,"1":1,"2":0,"3":0,"4":0},"legend":{"0":"Negligible","1":"Local and readily reversible","2":"Moderate shared impact","3":"Broad or costly impact","4":"Critical or irreversible impact"}},"underspecified":{"type":"noul","noul":0.1}}}`

func TestClientClassifyMapsLayaContract(t *testing.T) {
	input := classifier.Input{Prompt: "private prompt", Features: domain.RequestFeatures{NeedsText: true}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/classify" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var got struct {
			SchemaVersion string           `json:"schema_version"`
			State         classifier.Input `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.SchemaVersion != "mindctl.classifier.v1" || !reflect.DeepEqual(got.State, input) {
			t.Fatalf("request=%+v", got)
		}
		_, _ = io.WriteString(w, validResponse)
	}))
	defer server.Close()
	client := NewClient(config.ClassifierConfig{Endpoint: server.URL, Timeout: time.Second}, "secret", nil)
	got, err := client.Classify(context.Background(), input)
	if err != nil || got.Classifier != "laya" || got.ResolvedModel != "convaiinnovations/laya/typed-decisions" || got.MinimumTier != domain.T4 || got.TierConfidence != .9 || !reflect.DeepEqual(got.TierProbabilities, map[domain.Tier]float64{domain.T0: 0, domain.T1: 0, domain.T2: 0, domain.T3: .1, domain.T4: .9, domain.T5: 0, domain.T6: 0}) || got.TaskType != domain.TaskCoding || got.TaskTypeConfidence != 1 || !reflect.DeepEqual(got.TaskTypeProbabilities, map[domain.TaskType]float64{domain.TaskUnknown: 0, domain.TaskExtraction: 0, domain.TaskClassification: 0, domain.TaskGeneration: 0, domain.TaskReasoning: 0, domain.TaskCoding: 1, domain.TaskToolUse: 0, domain.TaskMultimodal: 0}) || got.ReasoningScore != 3 || got.CodingScore != 3 || got.BlastRadius != 1 || got.Underspecified != .1 || got.ModelRevision == "" || got.Latency <= 0 {
		t.Fatalf("judgment=%+v err=%v", got, err)
	}
}

func TestParseJudgmentRejectsUnsafeContract(t *testing.T) {
	missingLegend := strings.Replace(validResponse, `"legend":{"0":"None","1":"Minimal","2":"Simple","3":"Moderate","4":"Substantial","5":"Difficult","6":"Exceptional"}`, `"legend":{}`, 1)
	for name, body := range map[string]string{
		"schema mismatch":              strings.Replace(validResponse, "mindctl.classifier.v1", "other.v1", 1),
		"wrong classifier":             strings.Replace(validResponse, `"name":"laya"`, `"name":"other"`, 1),
		"empty repository":             strings.Replace(validResponse, `"repository":"convaiinnovations/laya"`, `"repository":""`, 1),
		"empty revision":               strings.Replace(validResponse, `"revision":"5e7b2b1b8ca2ecdd3f2322d94069c9b6ce7e844b"`, `"revision":""`, 1),
		"wrong variant":                strings.Replace(validResponse, `"variant":"typed-decisions"`, `"variant":"other"`, 1),
		"unknown field":                strings.TrimSuffix(validResponse, "}") + `,"unexpected":true}`,
		"missing selected probability": strings.Replace(validResponse, `,"T4":0.9`, "", 1),
		"probabilities do not sum":     strings.Replace(validResponse, `"T3":0.1`, `"T3":0.2`, 1),
		"nan score":                    strings.Replace(validResponse, `"score":3`, `"score":NaN`, 1),
		"illegal tier":                 strings.Replace(validResponse, `"choice":"T4"`, `"choice":"T9"`, 1),
		"illegal task":                 strings.Replace(validResponse, `"choice":"coding"`, `"choice":"chat"`, 1),
		"wrong answer type":            strings.Replace(validResponse, `"minimum_tier":{"type":"choice"`, `"minimum_tier":{"type":"score"`, 1),
		"missing score legend":         missingLegend,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseJudgment([]byte(body))
			if !errors.Is(err, classifier.ErrUnavailable) || !reflect.DeepEqual(got, domain.ClassifierJudgment{}) {
				t.Fatalf("judgment=%+v err=%v", got, err)
			}
		})
	}
}

func TestClientClassifyRejectsUnsafeTransportResponsesWithoutLeakingRequestData(t *testing.T) {
	tests := []struct {
		name  string
		token string
		http  http.HandlerFunc
	}{
		{"non-200 body", "secret", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "private prompt upstream response", http.StatusBadGateway)
		}},
		{"redirect", "secret", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.invalid/private-prompt", http.StatusTemporaryRedirect)
		}},
		{"invalid bearer token", "", func(w http.ResponseWriter, _ *http.Request) { t.Fatal("request should not be sent") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.http)
			defer server.Close()
			got, err := NewClient(config.ClassifierConfig{Endpoint: server.URL, Timeout: time.Second}, tc.token, nil).Classify(context.Background(), classifier.Input{Prompt: "private prompt"})
			if !errors.Is(err, classifier.ErrUnavailable) || !reflect.DeepEqual(got, domain.ClassifierJudgment{}) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "upstream") {
				t.Fatalf("judgment=%+v err=%v", got, err)
			}
		})
	}
}
