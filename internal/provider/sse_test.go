package provider

import (
	"context"
	"strings"
	"testing"
)

func TestSSEReaderPreservesEventData(t *testing.T) {
	reader := NewSSEReader(strings.NewReader("event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n"))
	event, err := reader.Next(context.Background())
	if err != nil || event.Type != "response.output_text.delta" || string(event.Data) != `{"delta":"hello"}` {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}
