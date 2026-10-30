package server

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"pai/pkg/auth"
	"pai/pkg/identity"
	"pai/pkg/worker"
)

// Network is what a person types to make a machine a worker of the server:
// its URL, then its device code.
type Network struct {
	URL  string `json:"-"` // the config's, read with the network
	Code string `json:"code"`
}

func (n Network) String() string { return n.URL + " " + n.Code }

func networkPath(dir string) string { return filepath.Join(dir, "network.json") }

// LoadNetwork is the network of the harness in dir.
func LoadNetwork(dir string) (Network, error) {
	var n Network
	b, err := os.ReadFile(networkPath(dir))
	if err != nil {
		return n, err
	}
	if err := json.Unmarshal(b, &n); err != nil {
		return n, err
	}
	// an empty code would let anyone in who runs the exchange with one
	if n.Code == "" {
		return n, fmt.Errorf("%s has no code", networkPath(dir))
	}
	c, err := Load(filepath.Join(dir, identity.Config))
	n.URL = c.Public()
	return n, err
}

// save writes the network in dir. join cycle and rename write it as root, in
// the operator's dir, so a link there leads nowhere outside it.
func (n Network) save(dir string) error {
	b, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return r.WriteFile(filepath.Base(networkPath(dir)), b, 0o600)
}

// adminSecret is admin's join secret, which no device code is: it changes
// with the code, as admin's own secret did with each install.
func adminSecret(k ed25519.PrivateKey, code string) string {
	return auth.Secret(k, worker.Admin+" "+code)
}

// network gives the server in dir a device code, unless it has one.
func network(dir string) error {
	_, err := LoadNetwork(dir)
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return Network{Code: auth.NewCode()}.save(dir)
}

// Cycle gives the installed harness a new device code. Its workers stay; a
// machine joins with the new code only.
func Cycle() (Network, error) {
	n, err := LoadNetwork(Dir)
	if err != nil {
		return n, err
	}
	n.Code = auth.NewCode()
	return n, n.save(Dir)
}
