// Package webui embeds the built web UI (web/dist, produced by
// `npm run build`). Without a build only a placeholder file is embedded
// and voipd serves a hint instead of the UI.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built UI, or nil if it was not built into this binary.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
