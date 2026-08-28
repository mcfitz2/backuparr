//go:build tools

// Package tools records build-time tool dependencies in go.mod/go.sum so
// versions used by `go generate` directives are pinned and reproducible.
// See https://github.com/golang/go/wiki/Modules#how-can-i-track-tool-dependencies-for-a-module
package tools

import (
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)
