package api

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/SnowballSH/snowdeploy/web"
)

// UI serves the embedded single-page app. Unknown paths fall back to
// index.html so a deep link like /service/web survives a page reload, but a
// missing asset under /assets/ stays a 404 rather than silently returning HTML.
func UI() http.Handler {
	assets, err := web.Assets()
	if err != nil {
		slog.Error("embedded web assets are unavailable", "error", err)
		return http.NotFoundHandler()
	}
	files := http.FileServerFS(assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "/" {
			serveIndex(w, r, assets)
			return
		}
		if _, err := fs.Stat(assets, strings.TrimPrefix(clean, "/")); err != nil {
			if !errors.Is(err, fs.ErrNotExist) || strings.HasPrefix(clean, "/assets/") {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, assets)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, _ *http.Request, assets fs.FS) {
	body, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "web UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}
