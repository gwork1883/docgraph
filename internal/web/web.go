package web

import "embed"

// Static contains the built Web UI served by docgraph.
//
//go:embed static/*
var Static embed.FS
