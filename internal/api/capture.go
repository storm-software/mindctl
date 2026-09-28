package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
)

// CaptureFailedBodies saves the caller body of every request answered with a
// 4xx or 5xx status so the failure can be replayed. Streams that fail after
// their 200 header is sent are not captured. The body is buffered up to
// maxBodyBytes+1 so the wrapped handler still enforces its own limit.
func CaptureFailedBodies(next http.Handler, maxBodyBytes int64, save func([]byte) (string, error), logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		_ = r.Body.Close()
		if err != nil {
			body = nil
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status < http.StatusBadRequest || len(body) == 0 {
			return
		}
		path, err := save(body)
		if err != nil {
			logger.Debug("request.body.capture_failed", "path", r.URL.Path, "status", recorder.status, "error", err.Error())
			return
		}
		logger.Debug("request.body.captured", "path", r.URL.Path, "status", recorder.status, "body_path", path, "body_bytes", len(body))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusRecorder) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
