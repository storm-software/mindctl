package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/storm-software/mindctl/internal/provider"
)

func TestErrorDiagnosticClassifiesFailuresWithoutLoggingCauses(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  string
	}{
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"truncated", io.ErrUnexpectedEOF, "unexpected_eof"},
		{"eof", io.EOF, "eof"},
		{"reset", syscall.ECONNRESET, "connection_reset"},
		{"refused", syscall.ECONNREFUSED, "connection_refused"},
		{"pipe", syscall.EPIPE, "broken_pipe"},
		{"dns", &net.DNSError{Err: "SECRET", Name: "SECRET"}, "dns"},
		{"timeout", &net.DNSError{Err: "SECRET", IsTimeout: true}, "timeout"},
		{"json", &json.SyntaxError{}, "invalid_json"},
		{"unknown", errors.New("SECRET"), "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := &url.Error{Op: "POST", URL: "https://SECRET", Err: test.cause}
			diagnostic := errorDiagnostic(fmt.Errorf("attempt: %w", &provider.Error{
				Kind: provider.ErrorRetryable, FailureStage: "stream_read", Err: cause,
			}))
			attributes := make(map[string]any)
			for index := 0; index < len(diagnostic); index += 2 {
				attributes[diagnostic[index].(string)] = diagnostic[index+1]
			}
			if attributes["failure_stage"] != "stream_read" || attributes["failure_cause"] != test.want {
				t.Fatalf("diagnostic=%v", attributes)
			}
			if strings.Contains(fmt.Sprint(diagnostic), "SECRET") {
				t.Fatal("diagnostic leaked underlying error content")
			}
		})
	}
}
