// web/static.go — Embedded SvelteKit frontend static file server
//
// The built SvelteKit dist/ directory is copied into web/dist/ by
// bootstrap-installer.sh before `go build`, so the resulting binary
// carries the entire frontend with no external runtime dependency.
//
// Embed path constraint: //go:embed paths must be within the module root.
// The frontend source lives at dashboard/frontend/ (outside the Go module at
// dashboard/backend/), so bootstrap-installer.sh copies
//   frontend/dist/ → backend/web/dist/
// before invoking `go build`. web/dist/.gitkeep ensures the directory
// exists in the repo so the directive compiles on a clean checkout.
//
// SPA fallback: SvelteKit uses client-side routing. Paths like /vault,
// /system, /update do not correspond to real files in dist/. Any request
// whose path is not found in the embedded FS is served index.html so the
// SvelteKit router can take over.
//
// Cache headers:
//   index.html                  → Cache-Control: no-cache
//   /assets/** and /_app/**     → Cache-Control: public, max-age=31536000, immutable
//   everything else             → Cache-Control: no-cache

package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// StaticFS returns the embedded dist/ sub-filesystem.
// The returned fs.FS is rooted at dist/ so callers see index.html directly,
// not dist/index.html.
func StaticFS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}

// Handler returns an http.Handler that:
//   - Serves static files from the embedded dist/ FS
//   - Falls back to index.html for unknown paths (SPA routing support)
//   - Applies aggressive cache headers for hashed Vite assets
//   - Applies no-cache for index.html and unknown-extension files
func Handler() (http.Handler, error) {
	sub, err := StaticFS()
	if err != nil {
		return nil, err
	}
	return &spaHandler{fs: sub}, nil
}

// spaHandler wraps the embedded FS with SPA fallback + cache header injection.
type spaHandler struct {
	fs fs.FS
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip leading slash for fs.Stat lookup.
	clean := strings.TrimPrefix(r.URL.Path, "/")
	if clean == "" {
		clean = "index.html"
	}

	// Determine whether the path maps to a real file in the FS.
	info, err := fs.Stat(h.fs, clean)
	servingIndex := err != nil || info.IsDir()
	if servingIndex {
		clean = "index.html"
	}

	// Set cache headers before the file is written.
	setCacheHeaders(w, clean)

	// Open and serve the file.
	f, err := h.fs.Open(clean)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	finfo, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// http.ServeContent handles Range, If-Modified-Since, ETag automatically.
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// embed.FS files always implement ReadSeeker; this is a safety guard.
		w.Header().Set("Content-Type", contentType(clean))
		io.Copy(w, f) //nolint:errcheck
		return
	}
	http.ServeContent(w, r, finfo.Name(), finfo.ModTime(), rs)
}

// setCacheHeaders applies the HisnOS cache policy:
//
//	index.html            → no-cache (entry point must always reflect current version)
//	/assets/, /_app/      → immutable (Vite content-hashes all filenames here)
//	everything else       → no-cache (conservative default)
func setCacheHeaders(w http.ResponseWriter, path string) {
	switch {
	case path == "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasPrefix(path, "assets/") || strings.HasPrefix(path, "_app/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
}

// contentType returns a minimal MIME type for known frontend extensions.
// Used only as a fallback when http.ServeContent cannot be used.
func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript"
	case strings.HasSuffix(name, ".css"):
		return "text/css"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}
