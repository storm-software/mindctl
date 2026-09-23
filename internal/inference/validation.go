package inference

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidRequest classifies malformed or unsupported portable requests.
var ErrInvalidRequest = errors.New("invalid inference request")

// ValidationError identifies a caller-controlled invalid field without
// carrying provider or persistence details.
type ValidationError struct {
	Param, Message string
}

func (e *ValidationError) Error() string {
	if e.Param == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Param, e.Message)
}

func (e *ValidationError) Is(target error) bool { return target == ErrInvalidRequest }

func Invalid(param, message string) error { return &ValidationError{Param: param, Message: message} }

var responseID = regexp.MustCompile(`^resp_[A-Za-z0-9_-]+$`)

// ValidateRequest enforces the canonical portable request contract. Existence
// and client ownership of PreviousResponseID are deliberately deferred to the
// conversation service.
func ValidateRequest(req Request) error {
	if strings.TrimSpace(req.Model) == "" {
		return Invalid("model", "is required")
	}
	if len(req.Input) == 0 {
		return Invalid("input", "is required")
	}
	if req.MaxOutputTokens < 0 {
		return Invalid("max_output_tokens", "must not be negative")
	}
	if req.PreviousResponseID != "" && !responseID.MatchString(req.PreviousResponseID) {
		return Invalid("previous_response_id", "has invalid format")
	}
	if req.TextVerbosity != "" && req.TextVerbosity != "low" && req.TextVerbosity != "medium" && req.TextVerbosity != "high" {
		return Invalid("text.verbosity", "must be low, medium, or high")
	}
	for i, item := range req.Input {
		if err := validateItem(item); err != nil {
			return Invalid(fmt.Sprintf("input[%d]", i), err.Error())
		}
	}
	for i, tool := range req.Tools {
		switch tool.Type {
		case "function":
			if strings.TrimSpace(tool.Name) == "" {
				return Invalid(fmt.Sprintf("tools[%d]", i), "must be a named function")
			}
		case "custom":
			if strings.TrimSpace(tool.Name) == "" || tool.Format == nil || strings.TrimSpace(tool.Format.Type) == "" || strings.TrimSpace(tool.Format.Definition) == "" {
				return Invalid(fmt.Sprintf("tools[%d]", i), "must be a named custom tool with a format")
			}
		default:
			return Invalid(fmt.Sprintf("tools[%d]", i), "has unsupported type")
		}
	}
	if format := req.TextFormat; format != nil {
		if format.Type != "json_schema" || strings.TrimSpace(format.Name) == "" {
			return Invalid("text.format", "must be a named json_schema format")
		}
		if !jsonObject(format.Schema) {
			return Invalid("text.format.schema", "must be a non-null JSON object")
		}
	}
	return nil
}

func jsonObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return len(raw) != 0 && json.Unmarshal(raw, &object) == nil && object != nil
}

func validateItem(item Item) error {
	switch item.Type {
	case "message":
		if item.Role != "user" && item.Role != "assistant" && item.Role != "system" && item.Role != "developer" {
			return errors.New("message role is invalid")
		}
		if item.Text == "" && len(item.ImageURL) == 0 {
			return errors.New("message content is required")
		}
	case "input_image":
		if len(item.ImageURL) == 0 {
			return errors.New("image_url is required")
		}
	case "function_call":
		if item.CallID == "" || item.Name == "" || len(item.Arguments) == 0 {
			return errors.New("function call requires call_id, name, and arguments")
		}
	case "function_call_output":
		if item.CallID == "" || len(item.Output) == 0 {
			return errors.New("function output requires call_id and output")
		}
	default:
		return fmt.Errorf("unsupported item type %q", item.Type)
	}
	return nil
}
