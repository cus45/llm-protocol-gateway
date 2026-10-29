//go:build !desktop_ui

package main

// materializeEmbeddedUI is a no-op unless the binary is built with the
// desktop_ui tag (release packaging runs scripts/bundle-desktop-assets.sh).
func materializeEmbeddedUI() string {
	return ""
}
