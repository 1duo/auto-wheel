// Package calendarui serves the read-only weekly results calendar.
package calendarui

import (
	"embed"
	"net/http"
)

//go:embed calendar.html calendar.js
var assets embed.FS

func Register(mux *http.ServeMux) {
	for _, asset := range []struct{ route, file, contentType string }{
		{"GET /calendar", "calendar.html", "text/html; charset=utf-8"},
		{"GET /calendar.js", "calendar.js", "text/javascript; charset=utf-8"},
	} {
		mux.HandleFunc(asset.route, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", asset.contentType)
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
			data, err := assets.ReadFile(asset.file)
			if err != nil {
				http.Error(w, "calendar unavailable", http.StatusInternalServerError)
				return
			}
			if r.Method != http.MethodHead {
				w.Write(data)
			}
		})
	}
}
