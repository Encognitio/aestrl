package web

import (
	"embed"
	"net/http"
)

//go:embed index.html style.css app.js
var content embed.FS

// FS returns an http.FileSystem serving the embedded web UI assets.
func FS() http.FileSystem {
	return http.FS(content)
}
