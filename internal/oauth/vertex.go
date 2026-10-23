package oauth

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/storm-software/mindctl/internal/accounts"
)

// ImportVertex reads a Google service-account JSON file and stores it as one
// local Vertex account. The private key is written only to the auth file.
func ImportVertex(dir, path, prefix string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var decoded struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
		ProjectID   string `json:"project_id"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", err
	}
	if decoded.Type != "service_account" || decoded.ClientEmail == "" || decoded.PrivateKey == "" {
		return "", errors.New("oauth: vertex import expects a service-account JSON key")
	}
	id := "vertex-" + sanitizeID(decoded.ClientEmail)
	return accounts.WriteAuthFile(dir, accounts.Account{
		ID: id, Provider: "vertex", Protocol: "gemini", Kind: accounts.KindServiceAccount,
		BaseURL: "https://aiplatform.googleapis.com", Prefix: prefix, Enabled: true, Weight: 1,
	}, accounts.Secret{
		ClientEmail: decoded.ClientEmail, PrivateKey: decoded.PrivateKey, TokenURI: decoded.TokenURI,
		AccountID: decoded.ProjectID,
	})
}

func sanitizeID(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = out[:48]
	}
	if out == "" {
		return "account"
	}
	return out
}
