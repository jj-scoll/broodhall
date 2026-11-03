package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFS holds the UI: one page, one script, one stylesheet. Nothing is loaded from
// outside this binary.
//
//go:embed ui/*
var uiFS embed.FS

// uiAssets serves the embedded UI at /.
func uiAssets() http.Handler {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic(err) // the embed directive guarantees the directory
	}
	return http.FileServerFS(sub)
}

// mountUIAssets serves the UI for every GET path the API does not claim.
func mountUIAssets(mux *http.ServeMux) {
	mux.Handle("GET /", uiAssets())
}
