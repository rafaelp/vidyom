//go:build vidyom_test_keyring

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/security"
)

// defaultKeyring, in this test-only build, is a plaintext JSON file (I168). Default
// builds do not contain this code. No variable means no keyring at all, so E2E can
// also exercise the fail-closed path.
func defaultKeyring(getenv func(string) string, warn func(string)) security.Keyring {
	path := getenv("VIDYOM_TEST_KEYRING_FILE")
	if path == "" {
		return unavailableKeyring{}
	}
	warn("test build: credentials are stored in plaintext in " + path)
	return &fileKeyring{path: path}
}

type unavailableKeyring struct{}

func (unavailableKeyring) Get(string, string) (string, error) {
	return "", security.ErrKeyringUnavailable
}
func (unavailableKeyring) Set(string, string, string) error { return security.ErrKeyringUnavailable }
func (unavailableKeyring) Delete(string, string) error      { return security.ErrKeyringUnavailable }

type fileKeyring struct {
	mu   sync.Mutex
	path string
}

func (f *fileKeyring) load() (map[string]string, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := map[string]string{}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (f *fileKeyring) store(entries map[string]string) error {
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

func (f *fileKeyring) Get(service, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.load()
	if err != nil {
		return "", errors.Join(security.ErrKeyringUnavailable, err)
	}
	secret, ok := entries[service+"\x00"+account]
	if !ok {
		return "", security.ErrKeyringNotFound
	}
	return secret, nil
}

func (f *fileKeyring) Set(service, account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.load()
	if err != nil {
		return errors.Join(security.ErrKeyringUnavailable, err)
	}
	entries[service+"\x00"+account] = secret
	if err := f.store(entries); err != nil {
		return errors.Join(security.ErrKeyringUnavailable, err)
	}
	return nil
}

func (f *fileKeyring) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.load()
	if err != nil {
		return errors.Join(security.ErrKeyringUnavailable, err)
	}
	if _, ok := entries[service+"\x00"+account]; !ok {
		return security.ErrKeyringNotFound
	}
	delete(entries, service+"\x00"+account)
	if err := f.store(entries); err != nil {
		return errors.Join(security.ErrKeyringUnavailable, err)
	}
	return nil
}
