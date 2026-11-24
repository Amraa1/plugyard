package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Amraa1/plugyard/internal/names"
)

// tokenPrefix starts every token, so that one found in a file or a log can
// be told for what it is.
const tokenPrefix = "pyd_"

// Token is the right to call the API, given to a team or a tool by name.
// The secret itself is shown once, when the token is made; what is kept is
// its hash.
type Token struct {
	Name string `json:"name"`
	// Hash is the SHA-256 of the secret, in hex.
	Hash string `json:"hash"`
	// Site limits the token to the chargers of one site. Empty means
	// every site.
	Site string `json:"site,omitempty"`
	// ReadOnly makes a token that can look and not act.
	ReadOnly bool      `json:"readOnly,omitempty"`
	Created  time.Time `json:"created"`
}

func tokensPath(dataDir string) string { return filepath.Join(dataDir, "api.tokens.json") }

// LoadTokens returns the tokens kept in the data directory, sorted by name.
func LoadTokens(dataDir string) ([]Token, error) {
	data, err := os.ReadFile(tokensPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tokens []Token
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", tokensPath(dataDir), err)
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].Name < tokens[j].Name })
	return tokens, nil
}

func saveTokens(dataDir string, tokens []Token) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	// Written beside and renamed, so that a reader never sees half a file.
	tmp := tokensPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, tokensPath(dataDir))
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken makes a token and returns its secret, which is not kept.
func CreateToken(dataDir, name, site string, readOnly bool) (secret string, err error) {
	if err := names.Validate(name); err != nil {
		return "", err
	}
	tokens, err := LoadTokens(dataDir)
	if err != nil {
		return "", err
	}
	for _, t := range tokens {
		if t.Name == name {
			return "", fmt.Errorf("a token named %q exists already; remove it first, or choose another name", name)
		}
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	secret = tokenPrefix + base64.RawURLEncoding.EncodeToString(random)
	tokens = append(tokens, Token{Name: name, Hash: hashSecret(secret), Site: site, ReadOnly: readOnly, Created: time.Now().UTC().Truncate(time.Second)})
	return secret, saveTokens(dataDir, tokens)
}

// RemoveToken removes the token of that name. It reports whether there was
// one.
func RemoveToken(dataDir, name string) (bool, error) {
	tokens, err := LoadTokens(dataDir)
	if err != nil {
		return false, err
	}
	for i, t := range tokens {
		if t.Name == name {
			return true, saveTokens(dataDir, append(tokens[:i], tokens[i+1:]...))
		}
	}
	return false, nil
}

// findToken returns the token a secret belongs to.
func findToken(tokens []Token, secret string) (Token, bool) {
	hash := []byte(hashSecret(secret))
	var found Token
	ok := false
	// Every token is compared, so that the time taken says nothing.
	for _, t := range tokens {
		if subtle.ConstantTimeCompare(hash, []byte(t.Hash)) == 1 {
			found, ok = t, true
		}
	}
	return found, ok
}
