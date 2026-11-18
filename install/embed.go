// Package install holds the install scripts every release ships, so a PAI
// server can serve its own: with @VERSION@ its release, @DL@ where that
// release's files are, and @KEY@ the release key, filled in by Script.
package install

import (
	_ "embed"
	"strings"
)

var (
	//go:embed install.sh
	sh string
	//go:embed install.ps1
	ps1 string
)

// Script is the install script name, install.sh or install.ps1, for release
// v at dl with key; ok is false for any other name.
func Script(name, v, dl, key string) (string, bool) {
	s := map[string]string{"install.sh": sh, "install.ps1": ps1}[name]
	if s == "" {
		return "", false
	}
	return strings.NewReplacer("@VERSION@", v, "@DL@", dl, "@KEY@", key).Replace(s), true
}
