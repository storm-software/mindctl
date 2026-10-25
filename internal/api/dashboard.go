package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dashboard
var dashboardFiles embed.FS

// Dashboard serves the management web client under /dashboard/. The page is
// static; it calls the management API with the token the viewer enters or,
// when signIn is set, with an access token from that issuer.
func Dashboard(signIn *OIDC) http.Handler {
	files, _ := fs.Sub(dashboardFiles, "dashboard")
	static := http.StripPrefix("/dashboard/", http.FileServerFS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; "+
			"img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-cache")
		switch {
		case signIn != nil && r.URL.Path == "/dashboard/oidc" && r.Method == http.MethodGet:
			signIn.serveSignIn(w, r)
		case signIn != nil && r.URL.Path == "/dashboard/oidc/token" && r.Method == http.MethodPost:
			signIn.exchange(w, r)
		default:
			static.ServeHTTP(w, r)
		}
	})
}
