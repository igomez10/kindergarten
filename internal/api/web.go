package api

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// webHandler serves the dashboard's static export (apps/web/out). Next.js
// writes /instances as instances.html and one shell for every database at
// instances/_.html, so paths are resolved the way `next start` would.
func webHandler(dir string) http.Handler {
	fs := http.Dir(dir)
	exists := func(p string) bool {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		return err == nil && !st.IsDir()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		p := path.Clean("/" + r.URL.Path)
		var file string
		switch {
		case p == "/":
			file = "/index.html"
		case exists(p):
			file = p
		case exists(p + ".html"):
			file = p + ".html"
		case strings.HasPrefix(p, "/instances/") && strings.Count(p, "/") == 2:
			file = "/instances/_.html"
		default:
			w.WriteHeader(http.StatusNotFound)
			if f, err := fs.Open("/404.html"); err == nil {
				defer f.Close()
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.Copy(w, f)
			}
			return
		}
		if strings.HasPrefix(file, "/_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		// ServeContent, not FileServer: FileServer redirects any */index.html
		// back to the directory, which would loop for "/".
		f, err := fs.Open(file)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		http.ServeContent(w, r, path.Base(file), st.ModTime(), f)
	})
}

// csrfGuard refuses state-changing requests a third-party page could forge
// with the user's session cookie: they must be JSON (which a cross-site form
// cannot send without a CORS preflight the API never approves) and, when the
// browser says where they came from, from this origin.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "json_required", "requests must use Content-Type: application/json")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
