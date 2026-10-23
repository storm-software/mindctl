package oauth

// Provider is one public OAuth client. Secrets are not embedded here; tokens
// are written to the local auth directory after a login.
type Provider struct {
	Name, ClientID, AuthURL, TokenURL string
	DeviceAuthURL, DeviceTokenURL     string
	RedirectPath, APIBase, Protocol   string
	AccountProvider, Resource         string
	DefaultPort                       int
	Scopes                            []string
	Device                            bool
	Devin                             bool
	Discovery                         bool
	ExtraAuth                         [][2]string
}

// Providers returns the local login flows. The names match `mindctl login`.
func Providers() map[string]Provider {
	return map[string]Provider{
		"codex": {
			Name: "codex", ClientID: "app_EMoamEEZ73f0CkXaXp7hrann",
			AuthURL: "https://auth.openai.com/oauth/authorize", TokenURL: "https://auth.openai.com/oauth/token",
			RedirectPath: "/auth/callback", DefaultPort: 1455, APIBase: "https://chatgpt.com/backend-api/codex",
			Protocol: "codex", AccountProvider: "openai",
			Scopes: []string{"openid", "email", "profile", "offline_access"},
		},
		"codex-device": {
			Name: "codex-device", ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Device: true,
			DeviceAuthURL:  "https://auth.openai.com/api/accounts/deviceauth/usercode",
			DeviceTokenURL: "https://auth.openai.com/api/accounts/deviceauth/token",
			AuthURL:        "https://auth.openai.com/codex/device", APIBase: "https://chatgpt.com/backend-api/codex",
			Protocol: "codex", AccountProvider: "openai",
		},
		"claude": {
			Name: "claude", ClientID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
			AuthURL: "https://claude.ai/oauth/authorize", TokenURL: "https://platform.claude.com/v1/oauth/token",
			RedirectPath: "/callback", DefaultPort: 54545, APIBase: "https://api.anthropic.com",
			Protocol: "anthropic", AccountProvider: "anthropic",
			Scopes: []string{"user:profile", "user:inference", "user:sessions:claude_code", "user:mcp_servers", "user:file_upload"},
		},
		"antigravity": {
			Name: "antigravity", ClientID: "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
			AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
			RedirectPath: "/callback", DefaultPort: 51121, APIBase: "https://cloudcode-pa.googleapis.com",
			Protocol: "gemini", AccountProvider: "antigravity",
			Scopes: []string{
				"https://www.googleapis.com/auth/cloud-platform",
				"https://www.googleapis.com/auth/userinfo.email",
				"https://www.googleapis.com/auth/userinfo.profile",
				"https://www.googleapis.com/auth/cclog",
				"https://www.googleapis.com/auth/experimentsandconfigs",
			},
		},
		"kimi":    kimi("kimi", "https://auth.kimi.com", "https://api.kimi.com/coding"),
		"kimi-ai": kimi("kimi-ai", "https://auth.kimi.ai", "https://api.kimi.ai/coding"),
		"xai": {
			Name: "xai", ClientID: "b1a00492-073a-47ea-816f-4c329264a828", Device: true, Discovery: true,
			AuthURL: "https://auth.x.ai", APIBase: "https://api.x.ai/v1",
			Protocol: "openai", AccountProvider: "xai",
		},
		"devin": {
			Name: "devin", Devin: true, AuthURL: "https://app.devin.ai/auth/cli/continue",
			TokenURL: "https://api.devin.ai/auth/cli/token", RedirectPath: "/callback", DefaultPort: 0,
			APIBase: "https://api.devin.ai", Protocol: "openai", AccountProvider: "devin",
		},
		"meta": {
			Name: "meta", ClientID: "1031625952748946", Device: true,
			DeviceAuthURL:  "https://auth.meta.com/oidc/device/authorization/",
			DeviceTokenURL: "https://auth.meta.com/oidc/device/token/",
			APIBase:        "https://api.meta.ai/v1", Protocol: "openai", AccountProvider: "meta",
		},
		"siwc": {
			Name: "siwc", ClientID: "dynamic_agent_client",
			AuthURL:      "https://auth.openai.com/api/accounts/authorize",
			TokenURL:     "https://auth.openai.com/api/accounts/oauth/token",
			RedirectPath: "/auth/callback", DefaultPort: 1455, Resource: "https://api.openai.com/v1",
			APIBase: "https://api.openai.com/v1", Protocol: "openai", AccountProvider: "openai",
			Scopes: []string{"openid", "profile", "email", "offline_access"},
		},
	}
}

func kimi(name, authHost, apiBase string) Provider {
	return Provider{
		Name: name, ClientID: "17e5f671-d194-4dfb-9706-5516cb48c098", Device: true,
		DeviceAuthURL:  authHost + "/api/oauth/device_authorization",
		DeviceTokenURL: authHost + "/api/oauth/token",
		APIBase:        apiBase, Protocol: "openai", AccountProvider: "kimi",
	}
}

// Lookup returns one login provider by CLI name.
func Lookup(name string) (Provider, bool) {
	provider, ok := Providers()[name]
	return provider, ok
}
