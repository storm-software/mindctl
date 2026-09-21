package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

// Controls are gateway-specific request controls carried in HTTP headers.
type Controls struct {
	MinTier, MaxTier *domain.Tier
	AllowEscalation  *bool
}

// DecodeResponseRequest strictly decodes the supported portable Responses
// subset. It never accepts gateway or provider credentials in the JSON body.
func DecodeResponseRequest(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) (inference.Request, Controls, error) {
	if maxBodyBytes <= 0 {
		return inference.Request{}, Controls{}, inference.Invalid("body", "limit must be positive")
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer body.Close()
	var wire responseRequest
	if err := decodeOne(body, &wire); err != nil {
		return inference.Request{}, Controls{}, decodeError(err)
	}
	request, err := wire.request()
	if err != nil {
		return inference.Request{}, Controls{}, err
	}
	controls, err := decodeControls(r)
	if err != nil {
		return inference.Request{}, Controls{}, err
	}
	if err := inference.ValidateRequest(request); err != nil {
		return inference.Request{}, Controls{}, err
	}
	return request, controls, nil
}

type responseRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions"`
	Input              json.RawMessage `json:"input"`
	Tools              []wireTool      `json:"tools"`
	Text               *wireText       `json:"text"`
	Stream             bool            `json:"stream"`
	MaxOutputTokens    int64           `json:"max_output_tokens"`
	PreviousResponseID string          `json:"previous_response_id"`
}

type wireText struct {
	Format *wireJSONSchemaFormat `json:"format"`
}

type wireJSONSchemaFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Strict      bool            `json:"strict"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type wireInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Text      string          `json:"text"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Content   json.RawMessage `json:"content"`
	ImageURL  json.RawMessage `json:"image_url"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

type wireContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

func (wire responseRequest) request() (inference.Request, error) {
	input, err := decodeInput(wire.Input)
	if err != nil {
		return inference.Request{}, err
	}
	request := inference.Request{Model: wire.Model, Instructions: wire.Instructions, Input: input, Stream: wire.Stream, MaxOutputTokens: wire.MaxOutputTokens, PreviousResponseID: wire.PreviousResponseID}
	for _, tool := range wire.Tools {
		request.Tools = append(request.Tools, inference.Tool{Type: tool.Type, Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, Strict: tool.Strict})
	}
	if wire.Text != nil && wire.Text.Format != nil {
		format := wire.Text.Format
		request.TextFormat = &inference.JSONSchemaFormat{Type: format.Type, Name: format.Name, Description: format.Description, Schema: format.Schema, Strict: format.Strict}
	}
	return request, nil
}

func decodeInput(raw json.RawMessage) ([]inference.Item, error) {
	if len(raw) == 0 {
		return nil, inference.Invalid("input", "is required")
	}
	var text string
	if err := decodeStrict(raw, &text); err == nil {
		return []inference.Item{{Type: "message", Role: "user", Text: text}}, nil
	}
	var values []json.RawMessage
	if err := decodeStrict(raw, &values); err != nil {
		return nil, inference.Invalid("input", "must be a string or item array")
	}
	items := make([]inference.Item, 0, len(values))
	for index, value := range values {
		var item wireInputItem
		if err := decodeStrict(value, &item); err != nil {
			return nil, inference.Invalid(fmt.Sprintf("input[%d]", index), "contains an unknown or malformed field")
		}
		decoded, err := item.canonical()
		if err != nil {
			return nil, inference.Invalid(fmt.Sprintf("input[%d]", index), err.Error())
		}
		items = append(items, decoded...)
	}
	return items, nil
}

func (wire wireInputItem) canonical() ([]inference.Item, error) {
	switch wire.Type {
	case "message":
		return decodeMessage(wire)
	case "function_call":
		return []inference.Item{{Type: wire.Type, CallID: wire.CallID, Name: wire.Name, Arguments: wire.Arguments}}, nil
	case "function_call_output":
		return []inference.Item{{Type: wire.Type, CallID: wire.CallID, Output: wire.Output}}, nil
	default:
		return nil, fmt.Errorf("unsupported item type %q", wire.Type)
	}
}

func decodeMessage(wire wireInputItem) ([]inference.Item, error) {
	var text string
	if err := decodeStrict(wire.Content, &text); err == nil {
		return []inference.Item{{Type: "message", Role: wire.Role, Text: text}}, nil
	}
	var parts []json.RawMessage
	if err := decodeStrict(wire.Content, &parts); err != nil {
		return nil, errors.New("message content must be text or content parts")
	}
	items := make([]inference.Item, 0, len(parts))
	for _, raw := range parts {
		var part wireContentPart
		if err := decodeStrict(raw, &part); err != nil {
			return nil, errors.New("message content contains an unknown or malformed field")
		}
		switch part.Type {
		case "input_text", "output_text":
			items = append(items, inference.Item{Type: "message", Role: wire.Role, Text: part.Text})
		case "input_image":
			items = append(items, inference.Item{Type: "input_image", Role: wire.Role, ImageURL: part.ImageURL})
		default:
			return nil, fmt.Errorf("unsupported content type %q", part.Type)
		}
	}
	return items, nil
}

func decodeControls(r *http.Request) (Controls, error) {
	controls := Controls{}
	for _, value := range []struct {
		header string
		dest   **domain.Tier
	}{
		{"X-Mindctl-Min-Tier", &controls.MinTier},
		{"X-Mindctl-Max-Tier", &controls.MaxTier},
	} {
		text := r.Header.Get(value.header)
		if text == "" {
			continue
		}
		tier, err := domain.ParseTier(text)
		if err != nil {
			return Controls{}, inference.Invalid(strings.ToLower(value.header), "must be T0 through T6")
		}
		*value.dest = &tier
	}
	if controls.MinTier != nil && controls.MaxTier != nil && controls.MinTier.Rank() > controls.MaxTier.Rank() {
		return Controls{}, inference.Invalid("tier", "minimum tier exceeds maximum tier")
	}
	if text := r.Header.Get("X-Mindctl-Allow-Escalation"); text != "" {
		if text != "true" && text != "false" {
			return Controls{}, inference.Invalid("x-mindctl-allow-escalation", "must be true or false")
		}
		allow, _ := strconv.ParseBool(text)
		controls.AllowEscalation = &allow
	}
	return controls, nil
}

func decodeOne(reader io.Reader, into any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeStrict(raw json.RawMessage, into any) error {
	return decodeOne(bytes.NewReader(raw), into)
}

func decodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Errorf("%w: limit %d", ErrBodyTooLarge, tooLarge.Limit)
	}
	return inference.Invalid("body", "must contain one valid JSON object")
}
