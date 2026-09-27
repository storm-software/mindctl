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
	// Raw, when set, is an Anthropic block relayed verbatim.
	Raw json.RawMessage `json:"-"`
}

func (content messageContent) MarshalJSON() ([]byte, error) {
	if len(content.Raw) != 0 {
		return content.Raw, nil
	}
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
			var native messagesNativeData
			var block messageContent
			if json.Unmarshal(item.ProviderData, &native) != nil {
				return messageResponseBody{}, inference.Invalid("output", "cannot translate native content")
			}
			// A textless carrier holds only native blocks.
			carrier := item.Type == "message" && item.Text == "" && len(native.Content) == 0
			if !carrier && (json.Unmarshal(native.Content, &block) != nil || block.Type != content.Type) {
				return messageResponseBody{}, inference.Invalid("output", "cannot translate native content")
			}
			for _, raw := range native.Prefix {
				if err := appendNativeContent(&body, raw); err != nil {
					return messageResponseBody{}, err
				}
			}
			if !carrier {
				// The provider's block keeps fields such as text citations.
				content.Raw = append(json.RawMessage(nil), native.Content...)
				body.Content = append(body.Content, content)
			}
			for _, raw := range native.Suffix {
				if err := appendNativeContent(&body, raw); err != nil {
					return messageResponseBody{}, err
				}
			}
			continue
		}
		body.Content = append(body.Content, content)
	}
	return body, nil
}

// appendNativeContent relays an Anthropic-only block, such as signed thinking
// or server_tool_use, exactly as the provider returned it.
func appendNativeContent(body *messageResponseBody, raw json.RawMessage) error {
	var block messageContent
	if json.Unmarshal(raw, &block) != nil || block.Type == "" || (block.Type == "thinking" && block.Signature == "") {
		return inference.Invalid("output", "cannot translate native content")
	}
	block.Raw = append(json.RawMessage(nil), raw...)
	body.Content = append(body.Content, block)
	return nil
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
