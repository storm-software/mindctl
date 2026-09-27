package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/storm-software/mindctl/internal/inference"
)

func TestMetaCompatiblePattern(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"null escape in class", `^[^\0]*$`, `^[^\x00]*$`},
		{"bare null escape", `\0`, `\x00`},
		{"longer octal escape untouched", `\012`, `\012`},
		{"escaped backslash before zero", `\\0`, `\\0`},
		{"no escapes", `^[0-9a-f]{32}$`, `^[0-9a-f]{32}$`},
		{"trailing backslash", `abc\`, `abc\`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := metaCompatiblePattern(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestMetaCompatibleSchemaRewritesNestedPatternsOnly(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string","pattern":"^[^\\0]*$"}},"id":{"type":"string","pattern":"^[0-9a-f]{32}$"},"v":{"anyOf":[{"const":"x"},{"type":"string","pattern":"\\0"}]}}}`)
	rewritten, changed := metaCompatibleSchema(schema)
	if !changed {
		t.Fatal("expected rewrite")
	}
	var decoded struct {
		Properties struct {
			Paths struct {
				Items struct{ Pattern string }
			}
			ID struct{ Pattern string }
			V  struct {
				AnyOf []struct{ Pattern string }
			}
		}
	}
	if err := json.Unmarshal(rewritten, &decoded); err != nil {
		t.Fatal(err)
	}
	props := decoded.Properties
	if props.Paths.Items.Pattern != `^[^\x00]*$` || props.ID.Pattern != `^[0-9a-f]{32}$` || props.V.AnyOf[1].Pattern != `\x00` {
		t.Fatalf("rewritten=%s", rewritten)
	}

	untouched := json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","pattern":"^[0-9a-f]{32}$"}}}`)
	if got, changed := metaCompatibleSchema(untouched); changed || string(got) != string(untouched) {
		t.Fatalf("changed=%v got=%s", changed, got)
	}
}

func TestMetaRequestRewritesToolPatternAndOpenAIDoesNot(t *testing.T) {
	schema := `{"type":"object","properties":{"path":{"type":"string","pattern":"^[^\\0]*$"}},"required":["path"]}`
	for _, tc := range []struct{ provider, want string }{{"meta", `^[^\x00]*$`}, {"openai", `^[^\0]*$`}} {
		t.Run(tc.provider, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Tools []struct {
						Parameters struct {
							Properties struct {
								Path struct{ Pattern string }
							}
						} `json:"parameters"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				got = body.Tools[0].Parameters.Properties.Path.Pattern
				_, _ = io.WriteString(w, `{"id":"upstream","status":"completed","model":"m","output":[]}`)
			}))
			defer server.Close()

			model := openAIModel()
			model.Provider = tc.provider
			request := textRequest()
			request.Tools = []inference.Tool{{Type: "function", Name: "Artifact", Parameters: json.RawMessage(schema)}}
			if _, err := newClient(server.URL).Execute(context.Background(), model, request); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("pattern=%q want %q", got, tc.want)
			}
		})
	}
}
