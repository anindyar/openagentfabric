// Package keys issues and rotates the gateway API keys that apps use.
//
// An app installed next to OAF (vclocal, for example) gets its own named key
// in cluster.yaml's routing.keys, with the key material in the secrets store
// like every other generated secret. The app can later rotate that key
// itself: it presents its current key to the key service and receives a new
// one, after which the old key no longer works. cluster.yaml stays the source
// of truth; only the secret value changes on rotation.
package keys

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"openagentfabric.dev/oaf/internal/config"
	"openagentfabric.dev/oaf/internal/secrets"
)

// SecretPrefix names an app key's secret: gateway-key-<name>.
const SecretPrefix = "gateway-key-"

// MinRotateInterval stops one key from being rotated in a tight loop. Each
// rotation recreates the gateway, which drops requests in flight.
const MinRotateInterval = 30 * time.Second

// ErrUnknownKey means a presented key matches no routing key.
var ErrUnknownKey = errors.New("unknown key")

// ErrTooSoon means the key was rotated less than MinRotateInterval ago.
var ErrTooSoon = errors.New("this key was rotated moments ago; try again shortly")

// Service issues and rotates keys. Its methods are safe for concurrent use;
// they serialize, because each one rewrites the gateway's configuration.
type Service struct {
	ClusterPath string
	Secrets     secrets.Store
	// ApplyGateway re-renders cluster.yaml and applies the gateway project,
	// so the gateway accepts exactly the keys in the secrets store.
	ApplyGateway func(ctx context.Context) error
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	rotated map[string]time.Time
}

// Credentials are an app's key, by its routing key name.
type Credentials struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Issue makes sure cluster.yaml has a key called name, generates its secret
// if needed, and applies the gateway. Issuing an existing key returns it
// unchanged, so installers can call Issue on every run.
func (s *Service) Issue(ctx context.Context, name string, models []string) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.ClusterPath)
	if err != nil {
		return Credentials{}, err
	}
	key := config.APIKey{Name: name, SecretRef: SecretPrefix + name, Models: models}
	out, changed, err := config.AddAPIKey(data, key)
	if err != nil {
		return Credentials{}, err
	}
	c, err := config.ParseCluster(out)
	if err != nil {
		return Credentials{}, err
	}
	ref := secretRefFor(c, name)
	value, err := s.Secrets.Value(ref)
	if err != nil {
		return Credentials{}, err
	}
	if changed {
		if err := config.WriteFileAtomic(s.ClusterPath, out); err != nil {
			return Credentials{}, err
		}
	}
	if err := s.ApplyGateway(ctx); err != nil {
		return Credentials{}, fmt.Errorf("applying the gateway: %w", err)
	}
	return Credentials{Name: name, Key: value}, nil
}

// Rotate replaces the key called name and applies the gateway. If the apply
// fails the old key is restored, so the app never holds a key the gateway
// does not accept.
func (s *Service) Rotate(ctx context.Context, name string) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := config.LoadCluster(s.ClusterPath)
	if err != nil {
		return Credentials{}, err
	}
	ref := secretRefFor(c, name)
	if ref == "" {
		return Credentials{}, fmt.Errorf("cluster.yaml has no key %q", name)
	}
	if last, ok := s.rotated[name]; ok && s.now().Sub(last) < MinRotateInterval {
		return Credentials{}, ErrTooSoon
	}
	newValue, oldValue, err := s.Secrets.Replace(ref)
	if err != nil {
		return Credentials{}, err
	}
	if err := s.ApplyGateway(ctx); err != nil {
		if oldValue != "" {
			if rerr := s.Secrets.Set(ref, oldValue); rerr != nil {
				return Credentials{}, fmt.Errorf("applying the gateway: %w; restoring the old key also failed: %w", err, rerr)
			}
			// Best effort: the gateway may already have taken the new key.
			_ = s.ApplyGateway(context.WithoutCancel(ctx))
		}
		return Credentials{}, fmt.Errorf("applying the gateway (the old key still works): %w", err)
	}
	if s.rotated == nil {
		s.rotated = map[string]time.Time{}
	}
	s.rotated[name] = s.now()
	return Credentials{Name: name, Key: newValue}, nil
}

// Owner returns the name of the routing key whose value is presented. It
// never generates a secret, and compares digests in constant time.
func (s *Service) Owner(presented string) (string, error) {
	if presented == "" {
		return "", ErrUnknownKey
	}
	c, err := config.LoadCluster(s.ClusterPath)
	if err != nil {
		return "", err
	}
	want := sha256.Sum256([]byte(presented))
	found := ""
	preview := secrets.Preview{Store: s.Secrets}
	for _, k := range c.Spec.Routing.Keys {
		v, err := preview.Value(k.SecretRef)
		if err != nil || v == secrets.Placeholder {
			continue
		}
		got := sha256.Sum256([]byte(v))
		if subtle.ConstantTimeCompare(want[:], got[:]) == 1 && found == "" {
			found = k.Name
		}
	}
	if found == "" {
		return "", ErrUnknownKey
	}
	return found, nil
}

// List returns the routing key names in cluster.yaml order.
func (s *Service) List() ([]config.APIKey, error) {
	c, err := config.LoadCluster(s.ClusterPath)
	if err != nil {
		return nil, err
	}
	return c.Spec.Routing.Keys, nil
}

func secretRefFor(c *config.Cluster, name string) string {
	for _, k := range c.Spec.Routing.Keys {
		if k.Name == name {
			return k.SecretRef
		}
	}
	return ""
}
