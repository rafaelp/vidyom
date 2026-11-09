//go:build windows

package vault

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiDecrypt calls CryptUnprotectData for the current user or machine. Compile
// checked only in this version (I10, I12).
func dpapiDecrypt(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty DPAPI blob")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	plain := make([]byte, out.Size)
	copy(plain, unsafe.Slice(out.Data, out.Size))
	return plain, nil
}
