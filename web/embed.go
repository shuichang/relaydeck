package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html styles.css app.js
var assets embed.FS

// FS returns the embedded RelayDeck browser assets.
func FS() fs.FS {
	return assets
}
