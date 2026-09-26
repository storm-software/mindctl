package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/inference"
)

type messagesSSEWriter struct {
	response                            http.ResponseWriter
	metadata                            executor.StreamMetadata
	wrote, started, completed, usedTool bool
	blocks                              map[string]int
	open                                string
	nextIndex                           int
	usage                               inference.Usage
}

func (w *messagesSSEWriter) MessagesEvents() bool { return true }

func (w *messagesSSEWriter) Start(metadata executor.StreamMetadata) error {
	w.metadata = metadata
	if !w.wrote {
		w.started, w.completed, w.usedTool = false, false, false
		w.blocks, w.open, w.nextIndex = nil, "", 0
		w.usage = inference.Usage{}
	}
	return nil
}

func (w *messagesSSEWriter) WriteEvent(ctx context.Context, event inference.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.Usage.Known {
		w.usage = event.Usage
	}
	if !w.started {
		w.started = true
		if err := w.write("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": w.metadata.ResponseID, "type": "message", "role": "assistant", "model": w.metadata.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": messageUsage{InputTokens: w.usage.InputTokens, OutputTokens: 0},
		}}); err != nil {
			return err
		}
	}
	key := event.ItemID
	if key == "" {
		key = "text"
	}
	switch event.Type {
	case "content_block_start":
		if event.ItemType == "thinking" && w.metadata.Provider != "anthropic" {
			return nil
		}
		return w.startBlock(key, event.ItemType, event.CallID, event.Name)
	case "content_block_stop":
		return w.stopBlock(key)
	case "content_block_delta", "response.output_text.delta", "response.function_call_arguments.delta":
		blockType := event.ItemType
		if event.ArgumentsDelta != "" {
			blockType = "tool_use"
		} else if event.Thinking != "" || event.Signature != "" {
			blockType = "thinking"
		} else if blockType == "" {
			blockType = "text"
		}
		if blockType == "thinking" && w.metadata.Provider != "anthropic" {
			return nil
		}
		if err := w.startBlock(key, blockType, event.CallID, event.Name); err != nil {
			return err
		}
		var delta map[string]any
		switch {
		case event.ArgumentsDelta != "":
			delta = map[string]any{"type": "input_json_delta", "partial_json": event.ArgumentsDelta}
		case event.Signature != "":
			delta = map[string]any{"type": "signature_delta", "signature": event.Signature}
		case event.Thinking != "":
			delta = map[string]any{"type": "thinking_delta", "thinking": event.Thinking}
		case event.Delta != "":
			delta = map[string]any{"type": "text_delta", "text": event.Delta}
		default:
			return nil
		}
		return w.write("content_block_delta", map[string]any{"type": "content_block_delta", "index": w.blocks[key], "delta": delta})
	case "response.output_item.added":
		if event.ItemType == "function_call" && event.CallID != "" && event.Name != "" {
			return w.startBlock(key, "tool_use", event.CallID, event.Name)
		}
	case "response.output_item.done":
		if event.ItemType == "function_call" || event.ItemType == "message" {
			return w.stopBlock(key)
		}
	case "response.completed":
		if err := w.stopBlock(w.open); err != nil {
			return err
		}
		stopReason := event.StopReason
		if stopReason == "" {
			switch {
			case event.Status == "incomplete":
				stopReason = "max_tokens"
			case w.usedTool:
				stopReason = "tool_use"
			default:
				stopReason = "end_turn"
			}
		}
		if err := w.write("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": w.usage.OutputTokens}}); err != nil {
			return err
		}
		w.completed = true
		return w.write("message_stop", map[string]any{"type": "message_stop"})
	}
	return nil
}

func (w *messagesSSEWriter) startBlock(key, blockType, callID, name string) error {
	if _, exists := w.blocks[key]; exists {
		return nil
	}
	switch blockType {
	case "tool_use", "function_call":
		if callID == "" || name == "" {
			return inference.Invalid("output", "tool call lacks an identifier or name")
		}
	case "text", "thinking":
	default:
		return inference.Invalid("output", "cannot translate stream block")
	}
	if w.blocks == nil {
		w.blocks = make(map[string]int)
	}
	if err := w.stopBlock(w.open); err != nil {
		return err
	}
	index := w.nextIndex
	w.nextIndex++
	w.blocks[key], w.open = index, key
	var block map[string]any
	switch blockType {
	case "tool_use", "function_call":
		w.usedTool = true
		block = map[string]any{"type": "tool_use", "id": callID, "name": name, "input": map[string]any{}}
	case "thinking":
		block = map[string]any{"type": "thinking", "thinking": ""}
	default:
		block = map[string]any{"type": "text", "text": ""}
	}
	return w.write("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
}

func (w *messagesSSEWriter) stopBlock(key string) error {
	if key == "" {
		return nil
	}
	index, exists := w.blocks[key]
	if !exists {
		return nil
	}
	delete(w.blocks, key)
	if w.open == key {
		w.open = ""
	}
	return w.write("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (w *messagesSSEWriter) WriteTerminalError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, detail := errorDetail(err)
	return w.write("error", map[string]any{"type": "error", "error": map[string]string{"type": detail.Type, "message": detail.Message}})
}

func (w *messagesSSEWriter) write(event string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !w.wrote {
		setRoutingHeaders(w.response, w.metadata)
		w.response.Header().Set("Content-Type", "text/event-stream")
		w.response.Header().Set("Cache-Control", "no-cache")
	}
	if _, err := w.response.Write([]byte("event: " + event + "\ndata: ")); err != nil {
		return err
	}
	if _, err := w.response.Write(encoded); err != nil {
		return err
	}
	if _, err := w.response.Write([]byte("\n\n")); err != nil {
		return err
	}
	w.wrote = true
	if flusher, ok := w.response.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
