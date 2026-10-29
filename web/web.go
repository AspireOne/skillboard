// Package web embeds the single-page frontend.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

func Static() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
