package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSSEReaderPreservesEventData(t *testing.T) {
	reader := NewSSEReader(strings.NewReader("event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n"))
	event, err := reader.Next(context.Background())
	if err != nil || event.Type != "response.output_text.delta" || string(event.Data) != `{"delta":"hello"}` {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}

func TestSSEReaderAcceptsEventLargerThanScannerLimit(t *testing.T) {
	payload := strings.Repeat("x", 128<<10)
	reader := NewSSEReader(strings.NewReader("event: response.output_text.delta\ndata: {\"delta\":\"" + payload + "\"}\n\n"))
	event, err := reader.Next(context.Background())
	if err != nil || len(event.Data) < len(payload) {
		t.Fatalf("len=%d err=%v", len(event.Data), err)
	}
}

func TestSSEReaderCloseInterruptsBlockedRead(t *testing.T) {
	body := newBlockingBody()
	reader := NewSSEReader(body)
	done := make(chan error, 1)
	go func() {
		_, err := reader.Next(context.Background())
		done <- err
	}()
	body.waitUntilRead(t)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked SSE read was not interrupted by Close")
	}
	if !body.Closed() {
		t.Fatal("upstream body was not closed")
	}
}

type blockingBody struct {
	started   chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
}

func newBlockingBody() *blockingBody {
	return &blockingBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *blockingBody) Closed() bool {
	select {
	case <-b.closed:
		return true
	default:
		return false
	}
}

func (b *blockingBody) waitUntilRead(t *testing.T) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("SSE reader did not start reading")
	}
}
