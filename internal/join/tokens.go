// Package join adds a box to an OAF cluster: one-time join tokens, the
// join.sh script an admin runs on the new box, enrolment into the cluster
// PKI, and the hardware report the topology recommendation works from.
// Nothing is deployed on join; the admin chooses a topology afterwards.
package join

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultTokenTTL is how long a join token is valid.
const DefaultTokenTTL = time.Hour

// TokenPrefix marks join tokens, so a leaked one is recognisable.
const TokenPrefix = "oafj_"

// Errors a token check returns.
var (
	ErrTokenUnknown = errors.New("unknown join token")
	ErrTokenExpired = errors.New("join token expired")
	ErrTokenUsed    = errors.New("join token already used")
)

// Token is a stored join token. Only its hash is kept.
type Token struct {
	ID        string    `json:"id"` // first 8 hex of the hash, for listing
	Hash      string    `json:"hash"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// MaxUses is how many boxes may enrol with it (one per box joining in
	// this round); Uses counts them.
	MaxUses int       `json:"max_uses"`
	Uses    int       `json:"uses"`
	UsedAt  time.Time `json:"used_at,omitzero"`  // when it was used up
	UsedBy  []string  `json:"used_by,omitempty"` // the node names it enrolled
}

// TokenStore keeps join tokens hashed in a file under the state dir.
type TokenStore struct {
	Path string
	Now  func() time.Time
	mu   sync.Mutex
}

func (s *TokenStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *TokenStore) load() ([]Token, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ts []Token
	return ts, json.Unmarshal(b, &ts)
}

func (s *TokenStore) save(ts []Token) error {
	// Drop tokens that expired over a day ago: the file stays small.
	cut := s.now().Add(-24 * time.Hour)
	keep := ts[:0]
	for _, t := range ts {
		if t.ExpiresAt.After(cut) {
			keep = append(keep, t)
		}
	}
	b, err := json.MarshalIndent(keep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Create makes a token valid for ttl (DefaultTokenTTL when zero) and for
// uses enrolments (1 when zero), and returns it in clear: the only time it
// exists in clear.
func (s *TokenStore) Create(label string, ttl time.Duration, uses int) (string, Token, error) {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	if uses <= 0 {
		uses = 1
	}
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", Token{}, err
	}
	clear := TokenPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	h := hashToken(clear)
	now := s.now()
	t := Token{ID: h[:8], Hash: h, Label: label, CreatedAt: now, ExpiresAt: now.Add(ttl), MaxUses: uses}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return "", Token{}, err
	}
	if err := s.save(append(ts, t)); err != nil {
		return "", Token{}, err
	}
	return clear, t, nil
}

func (s *TokenStore) find(ts []Token, clear string) int {
	h := hashToken(strings.TrimSpace(clear))
	for i, t := range ts {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			return i
		}
	}
	return -1
}

func (s *TokenStore) check(t Token) error {
	switch {
	case t.Uses >= max(t.MaxUses, 1):
		return ErrTokenUsed
	case !s.now().Before(t.ExpiresAt):
		return ErrTokenExpired
	}
	return nil
}

// Check reports whether a token may still be used (downloads before
// enrolment). It does not use it up.
func (s *TokenStore) Check(clear string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return Token{}, err
	}
	i := s.find(ts, clear)
	if i < 0 {
		return Token{}, ErrTokenUnknown
	}
	return ts[i], s.check(ts[i])
}

// Consume counts one enrolment; after MaxUses the token is used up.
func (s *TokenStore) Consume(clear, by string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return Token{}, err
	}
	i := s.find(ts, clear)
	if i < 0 {
		return Token{}, ErrTokenUnknown
	}
	if err := s.check(ts[i]); err != nil {
		return ts[i], err
	}
	ts[i].Uses++
	ts[i].UsedBy = append(ts[i].UsedBy, by)
	if ts[i].Uses >= max(ts[i].MaxUses, 1) {
		ts[i].UsedAt = s.now()
	}
	return ts[i], s.save(ts)
}

// List returns the stored tokens (hashes only), newest first.
func (s *TokenStore) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	for i, j := 0, len(ts)-1; i < j; i, j = i+1, j-1 {
		ts[i], ts[j] = ts[j], ts[i]
	}
	return ts, err
}
