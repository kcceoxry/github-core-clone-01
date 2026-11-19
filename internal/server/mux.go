// Package server composes the forge's HTTP surfaces into one handler:
// the git smart-HTTP protocol, the REST v3 API, the GraphQL API and the
// MCP endpoint. The gitproto package owns the git protocol and the domain
// store; rest, graphql and mcp are adapters over the same forge, so every
// surface reads and writes the same objects.
//
// Routing is by path prefix, with the git protocol as the fallback: the
// git routes are the ones gitproto already served, and the adapters own
// /api/v3/*, /api/graphql and /mcp.
package server

import (
	"net/http"

	"forge/internal/gitproto"
	"forge/internal/graphql"
	"forge/internal/mcp"
	"forge/internal/rest"
)

// New returns the composed handler for the forge.
func New(forge *gitproto.Forge) http.Handler {
	mux := http.NewServeMux()
	restHandler := rest.NewHandler(forge)
	mux.Handle("/api/v3/", restHandler)
	mux.Handle("/api/v3", restHandler)
	mux.Handle("/api/graphql", graphql.NewHandler(forge))
	mux.Handle("/mcp", mcp.NewHandler(forge))
	// Everything else is the git smart-HTTP surface (and the health
	// probe): gitproto already serves those paths.
	mux.Handle("/", forge)
	return mux
}
