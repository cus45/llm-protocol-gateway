//go:build desktop_ui

package main

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed all:assets
var embeddedUI embed.FS

// materializeEmbeddedUI unpacks the bundled console into the user cache and
// returns that path. The gateway can only serve the UI from a real directory,
// and the release artifacts (single .exe, AppImage, .app) may be copied away
// from any side files, so the assets travel inside the binary.
func materializeEmbeddedUI() string {
	dist, err := fs.Sub(embeddedUI, "assets")
	if err != nil {
		return ""
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return ""
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(index)
	base := filepath.Join(cache, "llm-protocol-gateway")
	target := filepath.Join(base, fmt.Sprintf("web-dist-%x", sum[:6]))
	if info, statErr := os.Stat(filepath.Join(target, "index.html")); statErr == nil && !info.IsDir() {
		return target
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return ""
	}
	tmp, err := os.MkdirTemp(base, ".web-dist-*")
	if err != nil {
		return ""
	}
	writeErr := fs.WalkDir(dist, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		out := filepath.Join(tmp, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		data, err := fs.ReadFile(dist, p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
	if writeErr == nil {
		writeErr = os.Rename(tmp, target)
	}
	if writeErr != nil {
		_ = os.RemoveAll(tmp)
		// Another instance may have won the race; reuse its copy.
		if info, statErr := os.Stat(filepath.Join(target, "index.html")); statErr == nil && !info.IsDir() {
			return target
		}
		slog.Warn("unpack embedded web UI failed", "error", writeErr)
		return ""
	}
	return target
}
