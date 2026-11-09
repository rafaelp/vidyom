package keyring

import (
	"errors"
	"fmt"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/rafaelp/vidyom/internal/contract/security"
)

// New returns the OS keyring.
func New() security.Keyring { return osKeyring{} }

type osKeyring struct{}

func (osKeyring) Get(service, account string) (string, error) {
	secret, err := gokeyring.Get(service, account)
	if err != nil {
		return "", translate(err)
	}
	return secret, nil
}

func (osKeyring) Set(service, account, secret string) error {
	return translate(gokeyring.Set(service, account, secret))
}

func (osKeyring) Delete(service, account string) error {
	return translate(gokeyring.Delete(service, account))
}

// translate maps the library's errors onto the contract. Any failure that is not
// "this entry does not exist" means the keyring cannot be used. The reason comes
// from the OS and never contains the secret: the library passes it to the OS
// through a pipe or an API call, not in an error.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gokeyring.ErrNotFound):
		return security.ErrKeyringNotFound
	case errors.Is(err, gokeyring.ErrSetDataTooBig):
		return fmt.Errorf("%w: the credential is too large for this keyring", security.ErrKeyringUnavailable)
	default:
		return fmt.Errorf("%w: %v", security.ErrKeyringUnavailable, err)
	}
}
