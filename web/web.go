// Package web holds the page: static files, served as they are. There is no build step —
// what is in this directory is what the browser gets.
package web

import "embed"

// Files is the page, index.html at the root.
//
//go:embed index.html app.css app.js js fonts icons favicon.ico manifest.webmanifest
var Files embed.FS
