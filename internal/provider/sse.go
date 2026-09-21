package provider

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
)

// SSEEvent is one decoded server-sent event frame.
type SSEEvent struct {
	Type string
	Data []byte
}

// SSEReader reads SSE frames without Scanner's token-size ceiling.
type SSEReader struct{ reader *bufio.Reader }

// NewSSEReader builds a line-safe reader for an SSE byte stream.
func NewSSEReader(reader io.Reader) *SSEReader { return &SSEReader{reader: bufio.NewReader(reader)} }

// Next returns the next complete SSE frame. A canceled context is observed
// before and after each line read; callers that need to interrupt a blocked
// read must close the underlying response body.
func (r *SSEReader) Next(ctx context.Context) (SSEEvent, error) {
	var event SSEEvent
	var data [][]byte
	seen := false
	for {
		if err := ctx.Err(); err != nil {
			return SSEEvent{}, err
		}
		line, err := r.reader.ReadString('\n')
		if len(line) != 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if seen {
					event.Data = bytes.Join(data, []byte("\n"))
					return event, nil
				}
			} else if !strings.HasPrefix(line, ":") {
				field, value, hasValue := strings.Cut(line, ":")
				if hasValue {
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "event":
					event.Type = value
					seen = true
				case "data":
					data = append(data, []byte(value))
					seen = true
				}
			}
		}
		if err != nil {
			if err == io.EOF && seen {
				event.Data = bytes.Join(data, []byte("\n"))
				return event, nil
			}
			return SSEEvent{}, err
		}
		if err := ctx.Err(); err != nil {
			return SSEEvent{}, err
		}
	}
}
