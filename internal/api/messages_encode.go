package api

import (
	"encoding/json"
	"net/http"

	"github.com/storm-software/mindctl/internal/inference"
)

type messageResponseBody struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []messageContent `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *messageUsage    `json:"usage,omitempty"`
}

type messageContent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

func (content messageContent) MarshalJSON() ([]byte, error) {
	if content.Type == "thinking" {
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{content.Type, content.Thinking, content.Signature})
	}
	type portableContent messageContent
	return json.Marshal(portableContent(content))
}

type messageUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func messageFromResult(result inference.Result, providerID string) (messageResponseBody, error) {
	body := messageResponseBody{ID: result.ID, Type: "message", Role: "assistant", Model: result.Model, Content: []messageContent{}}
	if result.Usage.Known {
		body.Usage = &messageUsage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens}
	}
	if result.StopReason != "" && providerID == "anthropic" {
		body.StopReason = result.StopReason
	} else if result.Status == "incomplete" {
		body.StopReason = "max_tokens"
	} else {
		body.StopReason = "end_turn"
	}
	for _, item := range result.Output {
		var content messageContent
		switch item.Type {
		case "message":
			content = messageContent{Type: "text", Text: item.Text}
		case "function_call":
			content = messageContent{Type: "tool_use", ID: item.CallID, Name: item.Name, Input: item.Arguments}
			if result.StopReason == "" {
				body.StopReason = "tool_use"
			}
		default:
			return messageResponseBody{}, inference.Invalid("output", "cannot translate provider output")
		}
		if providerID == "anthropic" && len(item.ProviderData) > 0 {
			var native struct {
				Content messageContent   `json:"anthropic_content_block"`
				Prefix  []messageContent `json:"anthropic_prefix"`
			}
			if json.Unmarshal(item.ProviderData, &native) != nil || native.Content.Type != content.Type {
				return messageResponseBody{}, inference.Invalid("output", "cannot translate native content")
			}
			for _, prefix := range native.Prefix {
				if prefix.Type != "thinking" || prefix.Signature == "" {
					return messageResponseBody{}, inference.Invalid("output", "cannot translate native content")
				}
				body.Content = append(body.Content, prefix)
			}
			content.CacheControl = native.Content.CacheControl
		}
		body.Content = append(body.Content, content)
	}
	return body, nil
}

func writeMessagesError(w http.ResponseWriter, err error) {
	status, detail := errorDetail(err)
	if detail.Param == "model" {
		detail.Message = "unknown or incompatible model"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code,omitempty"`
		} `json:"error"`
	}{Type: "error", Error: struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    string `json:"code,omitempty"`
	}{Type: detail.Type, Message: detail.Message, Code: detail.Code}})
}

// WriteMessagesError emits Anthropic-shaped errors for outer middleware.
func WriteMessagesError(w http.ResponseWriter, err error) {
	writeMessagesError(w, err)
}
