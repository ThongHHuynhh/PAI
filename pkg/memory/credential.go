package memory

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Credentials are the hidden section of memory: addresses, logins, keys and
// passwords the person wants agents to use. A value is sealed with a key
// beside the database, so a copy of memory.db alone does not hold it. It is
// never recalled into a context: recall shows a credential's name and
// description, and an agent uses it as {{cred:name}}, which the harness
// fills in on the way to the worker. Only the credential tool's get, and the
// person in the web app, see a value.
const credentialSchema = `
CREATE TABLE IF NOT EXISTS credentials (
	name TEXT PRIMARY KEY, description TEXT NOT NULL, scope TEXT NOT NULL,
	sealed BLOB, created TEXT NOT NULL, updated TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '',
	forgotten TEXT NOT NULL DEFAULT ''
);`

// credentialKey is the file of the sealing key in the memory directory.
const credentialKey = "credentials.key"

// Credential is one entry, its value only when asked for.
type Credential struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Scope       string    `json:"scope"`
	Value       string    `json:"value,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	Source      string    `json:"source,omitempty"` // a person, the credential tool, MCP
	Session     string    `json:"session,omitempty"`
}

var credentialName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Placeholder is how an agent names a credential's value in a tool call.
func Placeholder(name string) string { return "{{cred:" + name + "}}" }

// PlaceholderPattern finds placeholders; group 1 is the name.
var PlaceholderPattern = regexp.MustCompile(`\{\{cred:([a-z0-9][a-z0-9._-]{0,63})\}\}`)

func (s *Store) sealer() (cipher.AEAD, error) {
	p := filepath.Join(s.dir, credentialKey)
	key, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		f, ferr := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if ferr != nil {
			return nil, ferr
		}
		if _, ferr = f.Write(key); ferr != nil {
			_ = f.Close()
			return nil, ferr
		}
		err = f.Close()
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("memory: %s is %d bytes, not 32", p, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SaveCredential stores c, or replaces the value and description of the one
// of its name.
func (s *Store) SaveCredential(ctx context.Context, c Credential) error {
	c.Name = strings.TrimSpace(c.Name)
	switch {
	case !credentialName.MatchString(c.Name):
		return fmt.Errorf("credential name %q: lower case letters, digits, dot, dash and underscore, up to 64", c.Name)
	case c.Value == "":
		return errors.New("credential value is empty")
	case strings.TrimSpace(c.Description) == "":
		return errors.New("credential needs a description: what it is for, such as which machine and user")
	case secret.MatchString(c.Description):
		return errors.New("credential description looks like it holds the secret; put that in the value")
	}
	if c.Scope == "" {
		c.Scope = Global
	}
	aead, err := s.sealer()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	sealed := aead.Seal(nonce, nonce, []byte(c.Value), []byte(c.Name))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, `INSERT INTO credentials(name,description,scope,sealed,created,updated,source,session) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET description=excluded.description, scope=excluded.scope, sealed=excluded.sealed, updated=excluded.updated, source=excluded.source, session=excluded.session, forgotten=''`,
		c.Name, c.Description, c.Scope, sealed, now, now, c.Source, c.Session)
	if err == nil {
		s.emit(Event{Type: "credential", Detail: map[string]any{"name": c.Name, "op": "saved"}})
	}
	return err
}

// Credentials lists the live credentials without values, by name.
func (s *Store) Credentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,description,scope,created,updated,source,session FROM credentials WHERE forgotten='' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Credential
	for rows.Next() {
		var c Credential
		var created, updated string
		if err := rows.Scan(&c.Name, &c.Description, &c.Scope, &created, &updated, &c.Source, &c.Session); err != nil {
			return nil, err
		}
		c.Created, _ = time.Parse(time.RFC3339Nano, created)
		c.Updated, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CredentialValue is the value of the live credential name.
func (s *Store) CredentialValue(ctx context.Context, name string) (string, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT sealed FROM credentials WHERE name=? AND forgotten=''`, name).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no credential named %q", name)
	}
	if err != nil {
		return "", err
	}
	aead, err := s.sealer()
	if err != nil {
		return "", err
	}
	n := aead.NonceSize()
	if len(sealed) < n {
		return "", fmt.Errorf("credential %q is damaged", name)
	}
	b, err := aead.Open(nil, sealed[:n], sealed[n:], []byte(name))
	if err != nil {
		return "", fmt.Errorf("credential %q cannot be opened with %s: %w", name, credentialKey, err)
	}
	return string(b), nil
}

// ForgetCredential retires name with why. Unlike an item, its value is
// erased: a secret kept after it was retired is a leak waiting, while its
// name, description and reason stay for the record.
func (s *Store) ForgetCredential(ctx context.Context, name, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("forgetting a credential needs a reason")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE credentials SET sealed=NULL, forgotten=?, updated=? WHERE name=? AND forgotten=''`, reason, time.Now().UTC().Format(time.RFC3339Nano), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no credential named %q", name)
	}
	s.emit(Event{Type: "credential", Detail: map[string]any{"name": name, "op": "forgotten"}})
	return nil
}

// Mask replaces every live credential's value in b with its placeholder, so
// a value a command printed does not land in a context or a session file.
// Values under minMask bytes are left: masking "22" would garble output.
func (s *Store) Mask(ctx context.Context, b []byte) []byte {
	cs, err := s.Credentials(ctx)
	if err != nil || len(cs) == 0 {
		return b
	}
	out := string(b)
	for _, c := range cs {
		v, err := s.CredentialValue(ctx, c.Name)
		if err == nil && len(v) >= minMask {
			out = strings.ReplaceAll(out, v, Placeholder(c.Name))
		}
	}
	return []byte(out)
}

// minMask is the shortest value Mask hides.
const minMask = 6

// CredentialStubs are the credentials whose name or description shares a
// word with query, as lines for a context: never a value.
func (s *Store) CredentialStubs(ctx context.Context, query string) ([]string, error) {
	cs, err := s.Credentials(ctx)
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(query), splitWord) {
		if len(w) >= 3 {
			words[w] = true
		}
	}
	var out []string
	for _, c := range cs {
		for _, w := range strings.FieldsFunc(strings.ToLower(c.Name+" "+c.Description), splitWord) {
			if words[w] {
				out = append(out, fmt.Sprintf("- [credential %s] %s. Use %s in run, write or edit; the harness fills it in. Never print it.\n", c.Name, c.Description, Placeholder(c.Name)))
				break
			}
		}
	}
	return out, nil
}

func splitWord(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
}
