package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCaptureFailedBodies(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   bool
	}{
		{name: "rejected", status: http.StatusBadRequest, want: true},
		{name: "upstream failure", status: http.StatusBadGateway, want: true},
		{name: "success", status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			var saved, seen string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				seen = string(body)
				w.WriteHeader(test.status)
			})
			save := func(body []byte) (string, error) { saved = string(body); return "/tmp/body.json", nil }
			handler := CaptureFailedBodies(next, 1<<10, save, slog.New(slog.DiscardHandler))
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"a":1}`)))
			if seen != `{"a":1}` || (saved != "") != test.want || (test.want && saved != seen) {
				t.Fatalf("seen=%q saved=%q", seen, saved)
			}
		})
	}
}

func TestCaptureFailedBodiesKeepsHandlerBodyLimit(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := DecodeMessagesRequest(w, r, 8)
		if err == nil || !strings.Contains(err.Error(), "body too large") {
			t.Errorf("err=%v", err)
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})
	save := func([]byte) (string, error) { return "", nil }
	handler := CaptureFailedBodies(next, 8, save, slog.New(slog.DiscardHandler))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 64))))
}

func TestCaptureFailedBodiesKeepsFlusher(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapped writer is not a Flusher")
		}
	})
	handler := CaptureFailedBodies(next, 8, nil, nil)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
}
