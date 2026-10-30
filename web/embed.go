// Package web is PAI's web app: src/ is its source, dist/ the build every
// server binary carries. Build it with npm run build in this directory.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
