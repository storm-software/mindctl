package translate

import (
	"strings"
	"testing"
)

func TestRequestTranslatesOpenAIToAnthropic(t *testing.T) {
	body, err := Request("openai", "anthropic", []byte(`{"model":"claude","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil || !strings.Contains(string(body), "hello") || !strings.Contains(string(body), "max_tokens") {
		t.Fatalf("body=%s err=%v", body, err)
	}
}

func TestSameFamilyIsUnchanged(t *testing.T) {
	original := []byte(`{"model":"gpt","messages":[]}`)
	body, err := Request("openai", "openai-compatible", original)
	if err != nil || string(body) != string(original) {
		t.Fatalf("body=%s err=%v", body, err)
	}
}
