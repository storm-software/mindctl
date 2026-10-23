package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/executor"
	"github.com/storm-software/mindctl/internal/forward"
	"github.com/storm-software/mindctl/internal/inference"
)

// Named serves protocol, media, and realtime routes that always require a
// model name. mindctl-auto stays on the Responses, Chat Completions, and
// optional Messages handlers.
type Named struct {
	Executor *executor.Service
	Forward  *forward.Forwarder
	Config   ResponsesConfig
	Aliases  []accounts.Alias
	Models   []domain.Model
}

// Mount registers the named-model routes. wrap applies gateway authentication.
func (n *Named) Mount(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	if n == nil || mux == nil || wrap == nil {
		return
	}
	route := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, wrap(handler))
	}
	route("GET /v1/models", n.listModels)
	route("GET /v1/models/{model}", n.modelDetail)
	route("POST /v1/completions", n.completions)
	route("POST /v1/messages/count_tokens", n.countTokens)
	route("POST /v1/responses/compact", n.forwardProtocol("openai", ""))
	route("GET /v1beta/models", n.geminiModels)
	route("GET /v1beta/models/{name...}", n.gemini)
	route("POST /v1beta/models/{name...}", n.gemini)
	route("POST /v1beta/interactions", n.forwardProtocol("gemini", "/v1beta/interactions"))
	route("/backend-api/codex/{path...}", n.codex)
	route("POST /v1/images/generations", n.forwardProtocol("openai", "/v1/images/generations"))
	route("POST /v1/images/edits", n.forwardProtocol("openai", "/v1/images/edits"))
	route("/openai/v1/videos", n.forwardProtocol("openai", "/openai/v1/videos"))
	route("/openai/v1/videos/{path...}", n.forwardProtocol("openai", ""))
	route("/v1/videos", n.forwardProtocol("openai", "/v1/videos"))
	route("/v1/videos/{path...}", n.forwardProtocol("openai", ""))
	route("POST /v1/audio/speech", n.forwardProtocol("openai", "/v1/audio/speech"))
	route("POST /v1/tts", n.forwardProtocol("openai", "/v1/tts"))
	route("/v1/live", n.realtime("/v1/live"))
	route("/v1/live/{path...}", n.realtime(""))
	route("/v1/realtime", n.realtime("/v1/realtime"))
	route("/v1/realtime/{path...}", n.realtime(""))
	route("/v1/ws/{path...}", n.attach)
}

// Responses upgrades a websocket on the existing responses path.
func (n *Named) Responses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocketRequest(r) {
			n.socket(w, r, "/v1/responses")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (n *Named) listModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modelList(n.catalog(), r))
}

func (n *Named) modelDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model")
	for _, entry := range n.catalog() {
		if entry.id == id {
			writeJSON(w, http.StatusOK, modelObject(entry, catalogShape(r)))
			return
		}
	}
	WriteError(w, inference.Invalid("model", "unknown model"))
}

func (n *Named) geminiModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		Name string `json:"name"`
	}
	models := make([]model, 0)
	for _, entry := range n.catalog() {
		if entry.id == inference.AutomaticModel {
			continue
		}
		models = append(models, model{Name: "models/" + entry.id})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (n *Named) gemini(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	model := name
	if index := strings.Index(model, ":"); index >= 0 {
		model = model[:index]
	}
	model = strings.TrimPrefix(model, "models/")
	body, err := readLimited(w, r, n.limit())
	if err != nil {
		WriteError(w, err)
		return
	}
	n.dispatch(w, r, model, "gemini", r.URL.Path, body)
}

func (n *Named) codex(w http.ResponseWriter, r *http.Request) {
	if websocketRequest(r) {
		n.socket(w, r, r.URL.Path)
		return
	}
	body, err := readLimited(w, r, n.limit())
	if err != nil {
		WriteError(w, err)
		return
	}
	model := r.URL.Query().Get("model")
	if model == "" && len(body) > 0 {
		model, _ = forward.ModelFromJSON(body)
	}
	n.dispatch(w, r, model, "codex", r.URL.Path, body)
}

func (n *Named) completions(w http.ResponseWriter, r *http.Request) {
	if n.Executor == nil {
		WriteError(w, inference.Invalid("model", "completions are unavailable"))
		return
	}
	body, err := readLimited(w, r, n.limit())
	if err != nil {
		WriteError(w, err)
		return
	}
	var decoded struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		WriteError(w, inference.Invalid("body", "must be JSON"))
		return
	}
	if err := rejectAutomatic(decoded.Model); err != nil {
		WriteError(w, err)
		return
	}
	clientID, _ := ClientID(r.Context())
	request := inference.Request{
		Model: decoded.Model,
		Input: []inference.Item{{Type: "message", Role: "user", Text: decoded.Prompt}},
	}
	input := n.executorInput(clientID, request)
	applyAccountSession(r, &input)
	output, err := n.Executor.Execute(r.Context(), input)
	if err != nil {
		WriteError(w, err)
		return
	}
	text := ""
	for _, item := range output.Result.Output {
		text += item.Text
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": output.Result.ID, "object": "text_completion", "model": output.Result.Model,
		"choices": []any{map[string]any{"text": text, "index": 0, "finish_reason": output.Result.StopReason}},
	})
}

func (n *Named) countTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(w, r, n.limit())
	if err != nil {
		WriteError(w, err)
		return
	}
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		WriteError(w, inference.Invalid("body", "must be JSON"))
		return
	}
	if err := rejectAutomatic(decoded.Model); err != nil {
		WriteError(w, err)
		return
	}
	if n.Forward == nil || !n.Forward.Eligible(decoded.Model) {
		WriteError(w, inference.Invalid("model", "unknown model"))
		return
	}
	tokens := utf8.RuneCount(body) / 4
	if tokens == 0 && len(body) > 0 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

func (n *Named) forwardProtocol(protocol, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if websocketRequest(r) {
			upstream := path
			if upstream == "" {
				upstream = r.URL.Path
			}
			n.socket(w, r, upstream)
			return
		}
		body, err := readLimited(w, r, n.limit())
		if err != nil {
			WriteError(w, err)
			return
		}
		model := r.URL.Query().Get("model")
		if model == "" {
			model = multipartModel(r.Header.Get("Content-Type"), body)
		}
		if model == "" && len(body) > 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			model, _ = forward.ModelFromJSON(body)
		}
		upstream := path
		if upstream == "" {
			upstream = r.URL.Path
		}
		n.dispatch(w, r, model, protocol, upstream, body)
	}
}

func (n *Named) realtime(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		upstream := path
		if upstream == "" {
			upstream = r.URL.Path
		}
		if websocketRequest(r) {
			n.socket(w, r, upstream)
			return
		}
		body, err := readLimited(w, r, n.limit())
		if err != nil {
			WriteError(w, err)
			return
		}
		model := r.URL.Query().Get("model")
		if model == "" && len(body) > 0 {
			model, _ = forward.ModelFromJSON(body)
		}
		n.dispatch(w, r, model, "openai", upstream, body)
	}
}

func (n *Named) attach(w http.ResponseWriter, r *http.Request) {
	n.socket(w, r, "/"+r.PathValue("path"))
}

func (n *Named) socket(w http.ResponseWriter, r *http.Request, upstream string) {
	model := r.URL.Query().Get("model")
	if err := rejectAutomatic(model); err != nil {
		WriteError(w, err)
		return
	}
	if n.Forward == nil {
		WriteError(w, inference.Invalid("model", "realtime is unavailable"))
		return
	}
	if err := n.Forward.ProxyWebSocket(r.Context(), w, r, model, "openai", upstream); err != nil {
		if !headersSent(w) {
			writeForwardError(w, err)
		}
	}
}

func (n *Named) dispatch(w http.ResponseWriter, r *http.Request, model, protocol, path string, body []byte) {
	if err := rejectAutomatic(model); err != nil {
		WriteError(w, err)
		return
	}
	if n.Forward == nil {
		WriteError(w, inference.Invalid("model", "account routing is unavailable"))
		return
	}
	clientID, _ := ClientID(r.Context())
	key, parent, guardian := accountSession(r, clientID, model)
	result, err := n.Forward.Do(r.Context(), forward.Call{
		Method: r.Method, Model: model, CallerProtocol: protocol, Path: path, Body: body,
		Header: r.Header, SessionKey: key, ParentSessionKey: parent, Guardian: guardian,
	})
	if err != nil {
		writeForwardError(w, err)
		return
	}
	if contentType := result.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	status := result.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_, _ = w.Write(result.Body)
}

func (n *Named) executorInput(clientID string, request inference.Request) executor.Input {
	cfg := n.Config
	var minTier, maxTier *domain.Tier
	if cfg.MinTier.Valid() {
		value := cfg.MinTier
		minTier = &value
	}
	if cfg.MaxTier.Valid() {
		value := cfg.MaxTier
		maxTier = &value
	}
	return executor.Input{
		ClientID: clientID, Request: request, Models: append([]domain.Model(nil), cfg.Models...),
		MinTier: minTier, MaxTier: maxTier, SafeFallbackTier: cfg.SafeFallbackTier,
		ProviderCredentials: cloneBools(cfg.ProviderCredentials), ProviderAvailability: cloneBools(cfg.ProviderAvailability),
		SavingsBaseline: cfg.SavingsBaseline, ExpectedOutputTokens: cfg.ExpectedOutputTokens, Session: cfg.Session,
	}
}

func (n *Named) limit() int64 {
	if n != nil && n.Config.MaxBodyBytes > 0 {
		return n.Config.MaxBodyBytes
	}
	return 1 << 20
}

type catalogEntry struct {
	id, provider, tier string
}

func (n *Named) catalog() []catalogEntry {
	entries := []catalogEntry{{id: inference.AutomaticModel, provider: "mindctl", tier: "auto"}}
	seen := map[string]struct{}{inference.AutomaticModel: {}}
	models := n.Models
	if len(models) == 0 && n.Forward != nil {
		models = n.Forward.Models
	}
	for _, model := range models {
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		entries = append(entries, catalogEntry{id: model.ID, provider: model.Provider, tier: model.Tier.String()})
	}
	for _, alias := range n.Aliases {
		if alias.Alias == "" {
			continue
		}
		if _, ok := seen[alias.Alias]; ok {
			continue
		}
		seen[alias.Alias] = struct{}{}
		provider := alias.Provider
		if provider == "" {
			provider = "mindctl"
		}
		entries = append(entries, catalogEntry{id: alias.Alias, provider: provider, tier: "alias"})
	}
	return entries
}

func modelList(entries []catalogEntry, r *http.Request) any {
	shape := catalogShape(r)
	data := make([]any, 0, len(entries))
	for _, entry := range entries {
		data = append(data, modelObject(entry, shape))
	}
	if shape == "anthropic" {
		return map[string]any{"data": data, "has_more": false}
	}
	return map[string]any{"object": "list", "data": data}
}

func modelObject(entry catalogEntry, shape string) any {
	switch shape {
	case "anthropic":
		return map[string]any{"type": "model", "id": entry.id, "display_name": entry.id}
	case "grok":
		return map[string]any{"id": entry.id, "object": "model", "owned_by": "xai"}
	default:
		owner := entry.provider
		if owner == "" {
			owner = "mindctl"
		}
		return map[string]any{"id": entry.id, "object": "model", "owned_by": owner}
	}
}

func catalogShape(r *http.Request) string {
	if r.Header.Get("anthropic-version") != "" || r.Header.Get("x-api-key") != "" {
		return "anthropic"
	}
	if r.Header.Get("x-grok-client") != "" || strings.Contains(strings.ToLower(r.UserAgent()), "grok") {
		return "grok"
	}
	return "openai"
}

func rejectAutomatic(model string) error {
	if strings.TrimSpace(model) == "" || model == inference.AutomaticModel {
		return inference.Invalid("model", "a named model is required")
	}
	return nil
}

func readLimited(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, inference.Invalid("body", "is too large or unreadable")
	}
	return body, nil
}

func multipartModel(contentType string, body []byte) string {
	if !strings.HasPrefix(contentType, "multipart/") {
		return ""
	}
	marker := []byte("name=\"model\"")
	index := indexBytes(body, marker)
	if index < 0 {
		return ""
	}
	rest := body[index+len(marker):]
	if split := indexBytes(rest, []byte("\r\n\r\n")); split >= 0 {
		rest = rest[split+4:]
	}
	if end := indexBytes(rest, []byte("\r\n")); end >= 0 {
		return strings.TrimSpace(string(rest[:end]))
	}
	return ""
}

func indexBytes(haystack, needle []byte) int {
	return strings.Index(string(haystack), string(needle))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func writeForwardError(w http.ResponseWriter, err error) {
	message := err.Error()
	switch {
	case strings.Contains(message, "named model"):
		WriteError(w, inference.Invalid("model", "a named model is required"))
	case strings.Contains(message, "unknown model"):
		WriteError(w, inference.Invalid("model", "unknown model"))
	case strings.Contains(message, "no eligible"):
		WriteError(w, inference.Invalid("model", "no eligible account"))
	default:
		WriteError(w, err)
	}
}

func websocketRequest(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket")
}

func headersSent(http.ResponseWriter) bool { return false }
