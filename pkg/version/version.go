// Package version is the release a binary was built from. A release build
// stamps it: -ldflags "-X pai/pkg/version.V=v0.1.0".
// Every binary of one release carries the same version.
package version

import (
	"runtime"
	"runtime/debug"
)

// V is dev for every local build.
var V = "dev"

// Stamped like V, as -X pai/pkg/version.Branch=main, Builder=user@host and
// Built=2026-10-30T12:00:00Z. Empty when the build did not set them.
var Branch, Builder, Built string

// Unknown is what Build reports for a fact the build did not record.
const Unknown = "unknown"

// Build is where a binary came from, for a person to read. Commit and Dirty
// are the ones the go tool stamped from the git checkout.
type Build struct {
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Dirty    string `json:"dirty"` // yes, no, or unknown
	Builder  string `json:"builder"`
	Built    string `json:"built"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

// Info is this binary's Build; every fact the build did not record is Unknown.
func Info() Build {
	b := Build{Branch: Branch, Builder: Builder, Built: Built, Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Dirty: Unknown}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Commit = s.Value[:min(7, len(s.Value))]
			case "vcs.modified":
				b.Dirty = map[string]string{"true": "yes", "false": "no"}[s.Value]
			}
		}
	}
	for _, f := range []*string{&b.Branch, &b.Commit, &b.Builder, &b.Built, &b.Dirty} {
		if *f == "" {
			*f = Unknown
		}
	}
	return b
}

// Wordmark heads what an install prints when it is done: quadrant blocks,
// which terminals draw as exact cell fills.
const Wordmark = `▗▄▄▄▖  ▗▄▄▖  ▗▄▄▄▖
▐▌ ▐▌ ▐▌  ▐▌   █
▐▛▀▀▘ ▐▛▀▀▜▌   █
▐▌    ▐▌  ▐▌ ▗▄█▄▖
`

// Installed is the first thing a finished install prints.
func Installed() string { return Wordmark + "PAI " + V + " installed\n" }

// Match says whether a peer at version v may talk to this binary: both from
// one release, or either one a local build.
func Match(v string) bool { return v == V || v == "dev" || V == "dev" }
