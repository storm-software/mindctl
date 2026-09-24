package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/upstreamauth"
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
	if err != nil || len(got.Input) != 3 || got.Input[1].CallID != "call_1" || len(got.Input[0].Content) != 2 || got.Input[0].Content[0].Type != "input_text" || got.Input[0].Content[1].Type != "input_image" || got.Tools[0].Name != "lookup" || got.TextFormat == nil || got.TextFormat.Name != "answer" || got.MaxOutputTokens != 123 {
		t.Fatalf("request=%+v controls=%+v err=%v", got, controls, err)
	}
	if got.PreviousResponseID != "resp_prior_1" {
		t.Fatalf("previous response ID = %q", got.PreviousResponseID)
	}
}

func TestDecodeResponseRequestPreservesCodexWebSearchTool(t *testing.T) {
	got, _, err := decode(t, `{
  "model":"mindctl-auto",
  "input":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
  "tools":[{"type":"web_search","external_web_access":true}]
}`, 1<<20)
	if err != nil || len(got.Tools) != 1 || got.Tools[0].Type != "web_search" || got.Tools[0].ExternalWebAccess == nil || !*got.Tools[0].ExternalWebAccess {
		t.Fatalf("request=%+v err=%v", got, err)
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
		`{"model":"mindctl-auto","input":"hello","store":true}`,
		`{"model":"mindctl-auto","input":"hello","tool_choice":"required"}`,
		`{"model":"mindctl-auto","input":"hello","text":{"verbosity":"loud"}}`,
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
	h := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), "Authorization", staticTokens{{ID: "client-a", Value: "client-a"}})
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
		h := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), "Authorization", staticTokens{{ID: "client-a", Value: "client-a"}})
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

func TestAuthenticateSupportsDedicatedRawTokenHeader(t *testing.T) {
	gotClient := ""
	h := Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotClient, _ = ClientID(r.Context())
	}), "X-Mindctl-Token", staticTokens{{ID: "client-a", Value: "gateway-secret"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Mindctl-Token", "gateway-secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || gotClient != "client-a" {
		t.Fatalf("status=%d client=%q", rr.Code, gotClient)
	}
}

func TestAuthenticateRejectsDuplicateGatewayHeaders(t *testing.T) {
	called := false
	h := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), "X-Mindctl-Token", staticTokens{{ID: "client-a", Value: "gateway-secret"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Add("X-Mindctl-Token", "gateway-secret")
	req.Header.Add("X-Mindctl-Token", "gateway-secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || called {
		t.Fatalf("status=%d called=%v", rr.Code, called)
	}
}

func TestCaptureChatGPTOAuthRequiresOneCompleteCredential(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		authorization, account               string
		extraAuthorization, extraAccount, ok bool
	}{
		{"valid", "bearer oauth.jwt", "account-1", false, false, true},
		{"missing token", "", "account-1", false, false, false},
		{"token whitespace", "Bearer oauth token", "account-1", false, false, false},
		{"account whitespace", "Bearer oauth.jwt", " ", false, false, false},
		{"duplicate token", "Bearer first", "account-1", true, false, false},
		{"duplicate account", "Bearer oauth.jwt", "account-1", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got upstreamauth.ChatGPTCredential
			var present bool
			h := CaptureChatGPTOAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, present = upstreamauth.ChatGPT(r.Context())
			}))
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tc.authorization != "" {
				req.Header.Add("Authorization", tc.authorization)
			}
			if tc.extraAuthorization {
				req.Header.Add("Authorization", "Bearer second")
			}
			if tc.account != "" {
				req.Header.Add("ChatGPT-Account-Id", tc.account)
			}
			if tc.extraAccount {
				req.Header.Add("ChatGPT-Account-Id", "account-2")
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if present != tc.ok {
				t.Fatalf("present=%v want=%v", present, tc.ok)
			}
			if tc.ok && (got.AccessToken != "oauth.jwt" || got.AccountID != "account-1") {
				t.Fatalf("credential=%+v", got)
			}
		})
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
