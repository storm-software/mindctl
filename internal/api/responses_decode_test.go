package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

type staticTokens []Token

func (s staticTokens) Tokens() []Token { return s }

func TestDecodeResponseRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
      "model":"mindctl-auto","instructions":"be concise","input":"hello","stream":false
    }`))
	got, controls, err := DecodeResponseRequest(httptest.NewRecorder(), req, 1<<20)
	if err != nil || got.Model != "mindctl-auto" || len(got.Input) != 1 || got.Input[0].Text != "hello" || controls.AllowEscalation != nil {
		t.Fatalf("request=%+v controls=%+v err=%v", got, controls, err)
	}
}

func TestDecodeResponseRequestPreservesPortableItems(t *testing.T) {
	got, controls, err := decode(t, `{
  "model":"mindctl-auto",
  "previous_response_id":"resp_prior_1",
  "input":[
    {"type":"message","role":"user","content":[
      {"type":"input_text","text":"look"},
      {"type":"input_image","image_url":"https://example.test/image.png"}
    ]},
    {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"mindctl\"}"},
    {"type":"function_call_output","call_id":"call_1","output":"{\"result\":true}"}
  ],
  "tools":[{"type":"function","name":"lookup","description":"find","parameters":{"type":"object"}}],
  "text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}},
  "max_output_tokens":123
}`, 1<<20)
	if err != nil || len(got.Input) != 4 || got.Input[2].CallID != "call_1" || got.Tools[0].Name != "lookup" || got.TextFormat == nil || got.TextFormat.Name != "answer" || got.MaxOutputTokens != 123 {
		t.Fatalf("request=%+v controls=%+v err=%v", got, controls, err)
	}
	if got.PreviousResponseID != "resp_prior_1" {
		t.Fatalf("previous response ID = %q", got.PreviousResponseID)
	}
}

func TestDecodeRejectsUnknownMalformedAndOversizedBodies(t *testing.T) {
	for _, body := range []string{`{"model":"mindctl-auto","wat":1}`, `{`, ``} {
		_, _, err := decode(t, body, 1<<20)
		if err == nil {
			t.Fatalf("body %q unexpectedly passed", body)
		}
	}
	_, _, err := decode(t, `{"model":"mindctl-auto","input":"0123456789"}`, 8)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestDecodeRejectsUnknownNestedFieldsAndMultipleDocuments(t *testing.T) {
	for _, body := range []string{
		`{"model":"mindctl-auto","input":[{"type":"message","role":"user","content":"hello","wat":1}]}`,
		`{"model":"mindctl-auto","input":"hello"} {}`,
		`{"model":"mindctl-auto","input":"hello","previous_response_id":"not-a-response-id"}`,
	} {
		_, _, err := decode(t, body, 1<<20)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("body %q err=%v", body, err)
		}
	}
}

func TestDecodeRejectsJSONSchemaFormatWithoutObjectSchema(t *testing.T) {
	for _, body := range []string{
		`{"model":"mindctl-auto","input":"hello","text":{"format":{"type":"json_schema","name":"answer"}}}`,
		`{"model":"mindctl-auto","input":"hello","text":{"format":{"type":"json_schema","name":"answer","schema":null}}}`,
		`{"model":"mindctl-auto","input":"hello","text":{"format":{"type":"json_schema","name":"answer","schema":[]}}}`,
	} {
		_, _, err := decode(t, body, 1<<20)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("body %q err=%v", body, err)
		}
	}
}

func TestAuthenticateUsesConstantTimeTokenMatch(t *testing.T) {
	called := false
	h := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), staticTokens{{ID: "client-a", Value: "client-a"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-a")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status=%d called=%v", rr.Code, called)
	}
}

func TestAuthenticateRejectsMissingOrInvalidBearerTokens(t *testing.T) {
	for _, header := range []string{"", "Basic client-a", "Bearer wrong"} {
		called := false
		h := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), staticTokens{{ID: "client-a", Value: "client-a"}})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || called {
			t.Fatalf("header=%q status=%d called=%v", header, rr.Code, called)
		}
	}
}

func TestDecodeControlsValidatesTierAndEscalationHeaders(t *testing.T) {
	req := validRequest()
	req.Header.Set("X-Mindctl-Min-Tier", "T5")
	req.Header.Set("X-Mindctl-Max-Tier", "T3")
	if _, _, err := DecodeResponseRequest(httptest.NewRecorder(), req, 1<<20); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected invalid bounds, got %v", err)
	}

	req = validRequest()
	req.Header.Set("X-Mindctl-Min-Tier", "T3")
	req.Header.Set("X-Mindctl-Max-Tier", "T5")
	req.Header.Set("X-Mindctl-Allow-Escalation", "true")
	_, controls, err := DecodeResponseRequest(httptest.NewRecorder(), req, 1<<20)
	if err != nil || controls.MinTier == nil || *controls.MinTier != domain.T3 || controls.MaxTier == nil || *controls.MaxTier != domain.T5 || controls.AllowEscalation == nil || !*controls.AllowEscalation {
		t.Fatalf("controls=%+v err=%v", controls, err)
	}
}

func TestWriteErrorDoesNotExposeUnexpectedErrorText(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteError(rr, errors.New("SQLite password=private"))
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "private") || !strings.Contains(rr.Body.String(), `"type":"server_error"`) {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func decode(t *testing.T, body string, limit int64) (request inference.Request, controls Controls, err error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	return DecodeResponseRequest(httptest.NewRecorder(), req, limit)
}

func validRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"mindctl-auto","input":"hello"}`))
}
