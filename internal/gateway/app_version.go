package gateway

import (
	"net/http"
	"sync"
	"time"
)

// BuildVersion is the version of the running gateway, stamped at build time
// (see scripts/build-version.sh). It is what a console tab compares its own
// version against to learn that a new release is live, so a user with an old
// page open is told to upgrade instead of silently running half-updated code
// after a release swapped the assets underneath them.
//
// The zero value is a development build: the console then never shows the
// upgrade prompt, because there is no release to upgrade to.
var BuildVersion = ""

// SetBuildVersion records the version of the running binary. Called from main
// before the server starts so the value the console sees cannot race a request.
func SetBuildVersion(version string) {
	buildVersionMu.Lock()
	BuildVersion = version
	buildVersionMu.Unlock()
}

// buildVersionMu guards BuildVersion against a concurrent SetBuildVersion and
// a request reading it. The value is written once at start-up in practice, but
// tests and the desktop app set it at other moments.
var buildVersionMu sync.RWMutex

// currentBuildVersion is the version the console is told about.
func currentBuildVersion() string {
	buildVersionMu.RLock()
	defer buildVersionMu.RUnlock()
	return BuildVersion
}

// handleAppVersion serves the running build's version. It is deliberately not
// admin-only: every console tab polls it, including one that is still on the
// login screen, and a version string is not a secret. It is not cached, so a
// release is visible to open tabs on their next poll.
func (s *Server) handleAppVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"version": currentBuildVersion(),
		"time":    time.Now().Format(time.RFC3339),
	})
}
