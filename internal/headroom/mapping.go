package headroom

import (
	"encoding/json"
	"errors"

	"github.com/storm-software/mindctl/internal/inference"
)

func cloneRequest(request inference.Request) (inference.Request, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return inference.Request{}, err
	}
	var copyRequest inference.Request
	if err := json.Unmarshal(data, &copyRequest); err != nil {
		return inference.Request{}, err
	}
	return copyRequest, nil
}

func eligibleMessages(request inference.Request) ([]compressionMessage, bool) {
	messages := []compressionMessage{}
	for index, item := range request.Input {
		if item.Type != "message" && item.Type != "function_call_output" && item.Type != "tool_result" {
			continue
		}
		if item.Text == "" || len(item.Content) > 0 || rawJSONPresent(item.Output) {
			continue
		}
		role := item.Role
		if role == "" {
			role = item.Type
		}
		if role == "user" {
			continue
		}
		messages = append(messages, compressionMessage{Index: index, Role: role, Text: item.Text})
	}
	return messages, len(messages) > 0
}

func applyMessages(request *inference.Request, messages []compressionMessage) error {
	seen := make(map[int]struct{}, len(messages))
	expected := make(map[int]compressionMessage, len(messages))
	eligible, _ := eligibleMessages(*request)
	for _, message := range eligible {
		expected[message.Index] = message
	}
	for _, message := range messages {
		original, ok := expected[message.Index]
		if !ok || message.Text == "" {
			return errors.New("invalid compressed message")
		}
		if _, ok := seen[message.Index]; ok {
			return errors.New("duplicate compressed message")
		}
		seen[message.Index] = struct{}{}
		if message.Role != original.Role {
			return errors.New("compressed message role changed")
		}
		request.Input[message.Index].Text = message.Text
	}
	if len(seen) != len(expected) {
		return errors.New("compressed message set changed")
	}
	return nil
}

func rawJSONPresent(value json.RawMessage) bool {
	return len(value) > 0 && string(value) != "null"
}
