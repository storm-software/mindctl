// Package translate converts a named-model request between caller and account
// protocols. Automatic routing does not use it.
package translate

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/storm-software/mindctl/internal/accounts"
)

// ErrUnsupported means this pair has no chat translation.
var ErrUnsupported = errors.New("translate: unsupported protocol pair")

type chat struct {
	Model     string    `json:"model,omitempty"`
	Messages  []message `json:"messages,omitempty"`
	Contents  []content `json:"contents,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

// Request rewrites body from the caller protocol into the account protocol.
// Equal families are returned unchanged.
func Request(from, to string, body []byte) ([]byte, error) {
	return convert(from, to, body, false)
}

// Response rewrites an account payload back into the caller protocol.
func Response(from, to string, body []byte) ([]byte, error) {
	return convert(from, to, body, true)
}

func convert(from, to string, body []byte, response bool) ([]byte, error) {
	source := accounts.ProtocolFamily(from)
	target := accounts.ProtocolFamily(to)
	if source == "" || target == "" || source == target {
		return body, nil
	}
	if len(body) == 0 {
		return body, nil
	}
	if response {
		text, model, err := responseText(source, body)
		if err != nil {
			return nil, err
		}
		return responseBody(target, model, text)
	}
	text, model, err := requestText(source, body)
	if err != nil {
		return nil, err
	}
	return requestBody(target, model, text)
}

func requestText(family string, body []byte) (string, string, error) {
	var decoded chat
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", ErrUnsupported
	}
	switch family {
	case "openai", "anthropic":
		if len(decoded.Messages) == 0 {
			return "", "", ErrUnsupported
		}
		return decoded.Messages[len(decoded.Messages)-1].Content, decoded.Model, nil
	case "gemini":
		if len(decoded.Contents) == 0 || len(decoded.Contents[0].Parts) == 0 {
			return "", "", ErrUnsupported
		}
		return decoded.Contents[0].Parts[0].Text, decoded.Model, nil
	default:
		return "", "", ErrUnsupported
	}
}

func requestBody(family, model, text string) ([]byte, error) {
	switch family {
	case "openai":
		return json.Marshal(chat{Model: model, Messages: []message{{Role: "user", Content: text}}})
	case "anthropic":
		payload := chat{Model: model, MaxTokens: 1024, Messages: []message{{Role: "user", Content: text}}}
		return json.Marshal(payload)
	case "gemini":
		return json.Marshal(chat{Contents: []content{{Role: "user", Parts: []part{{Text: text}}}}})
	default:
		return nil, ErrUnsupported
	}
}

func responseText(family string, body []byte) (string, string, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", ErrUnsupported
	}
	switch family {
	case "openai":
		choices, _ := decoded["choices"].([]any)
		if len(choices) == 0 {
			return "", "", ErrUnsupported
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		text, _ := message["content"].(string)
		model, _ := decoded["model"].(string)
		return text, model, nil
	case "anthropic":
		blocks, _ := decoded["content"].([]any)
		if len(blocks) == 0 {
			return "", "", ErrUnsupported
		}
		block, _ := blocks[0].(map[string]any)
		text, _ := block["text"].(string)
		model, _ := decoded["model"].(string)
		return text, model, nil
	case "gemini":
		candidates, _ := decoded["candidates"].([]any)
		if len(candidates) == 0 {
			return "", "", ErrUnsupported
		}
		candidate, _ := candidates[0].(map[string]any)
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		if len(parts) == 0 {
			return "", "", ErrUnsupported
		}
		part, _ := parts[0].(map[string]any)
		text, _ := part["text"].(string)
		return text, "", nil
	default:
		return "", "", ErrUnsupported
	}
}

func responseBody(family, model, text string) ([]byte, error) {
	switch family {
	case "openai":
		return json.Marshal(map[string]any{
			"model": model, "choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": text}}},
		})
	case "anthropic":
		return json.Marshal(map[string]any{
			"model": model, "type": "message", "role": "assistant",
			"content": []any{map[string]string{"type": "text", "text": text}},
		})
	case "gemini":
		return json.Marshal(map[string]any{
			"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": text}}}}},
		})
	default:
		return nil, ErrUnsupported
	}
}

// Family is the adapter family for a protocol name.
func Family(protocol string) string {
	return accounts.ProtocolFamily(strings.TrimSpace(protocol))
}
