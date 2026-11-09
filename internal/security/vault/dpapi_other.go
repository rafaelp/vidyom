//go:build !windows

package vault

import "errors"

func dpapiDecrypt([]byte) ([]byte, error) {
	return nil, errors.New("DPAPI exists only on Windows")
}
