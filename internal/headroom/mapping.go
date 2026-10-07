package headroom

import (
	"encoding/json"
	"errors"
	"strings"

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

type messageTarget struct {
	message   compressionMessage
	itemIndex int
	partIndex int
	output    bool
}

func eligibleMessages(request inference.Request) ([]compressionMessage, bool) {
	targets := messageTargets(request)
	messages := make([]compressionMessage, 0, len(targets))
	for _, target := range targets {
		messages = append(messages, target.message)
	}
	return messages, len(messages) > 0
}

func messageTargets(request inference.Request) []messageTarget {
	var targets []messageTarget
	for index, item := range request.Input {
		if rawJSONPresent(item.ProviderData) || rawJSONPresent(item.EncryptedContent) || rawJSONPresent(item.ImageURL) || rawJSONPresent(item.Summary) {
			continue
		}
		if item.Type == "message" && (item.Role == "user" || item.Role == "assistant") && !rawJSONPresent(item.Output) {
			if len(item.Content) == 0 && item.Text != "" {
				targets = append(targets, messageTarget{message: compressionMessage{Role: item.Role, Content: item.Text}, itemIndex: index, partIndex: -1})
				continue
			}
			if item.Text != "" || len(item.Content) == 0 {
				continue
			}
			valid := true
			for _, part := range item.Content {
				if (part.Type != "input_text" && part.Type != "output_text") || part.Text == "" || part.EncryptedContent != "" || rawJSONPresent(part.ImageURL) {
					valid = false
					break
				}
			}
			if valid {
				for partIndex, part := range item.Content {
					targets = append(targets, messageTarget{message: compressionMessage{Role: item.Role, Content: part.Text}, itemIndex: index, partIndex: partIndex})
				}
			}
			continue
		}
		if !strings.HasSuffix(item.Type, "_call_output") || item.CallID == "" || item.Text != "" || len(item.Content) != 0 {
			continue
		}
		var output string
		if json.Unmarshal(item.Output, &output) == nil && output != "" {
			targets = append(targets, messageTarget{message: compressionMessage{Role: "tool", Content: output, ToolCallID: item.CallID}, itemIndex: index, output: true})
		}
	}
	return targets
}

func applyMessages(request *inference.Request, messages []compressionMessage) error {
	targets := messageTargets(*request)
	if len(messages) != len(targets) {
		return errors.New("compressed message count changed")
	}
	for index, message := range messages {
		original := targets[index].message
		if message.Content == "" || message.Role != original.Role || message.ToolCallID != original.ToolCallID || (message.Role == "user" && message.Content != original.Content) {
			return errors.New("invalid compressed message")
		}
	}
	for index, target := range targets {
		item := &request.Input[target.itemIndex]
		if target.output {
			encoded, err := json.Marshal(messages[index].Content)
			if err != nil {
				return err
			}
			item.Output = encoded
		} else if target.partIndex >= 0 {
			item.Content[target.partIndex].Text = messages[index].Content
		} else {
			item.Text = messages[index].Content
		}
	}
	return nil
}

func rawJSONPresent(value json.RawMessage) bool {
	return len(value) > 0 && string(value) != "null"
}
