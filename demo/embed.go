// Package demo embeds the built-in demo assets so the CLI can initialize a
// fully working studio without any network access.
package demo

import (
	"embed"
	"fmt"
)

// FS carries the standalone runtime example: mock data source responses and
// one ready-made self-contained package.
//
//go:embed mock packages
var FS embed.FS

// MustRead returns one embedded asset or panics with a clear message.
func MustRead(path string) []byte {
	raw, err := FS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("demo: embedded asset missing: %s", path))
	}
	return raw
}
