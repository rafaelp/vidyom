package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

const (
	keyringService = "vidyom"
	maxSecretBytes = 1 << 20
)

func deviceAccount(server string) string { return "device/" + server }

func secretAccount(server, tenant, name string) string {
	return "secret/" + server + "/" + tenant + "/" + name
}

func (a *app) keyring() security.Keyring {
	a.krOnce.Do(func() {
		if a.opts.Keyring != nil {
			a.kr = a.opts.Keyring
			return
		}
		a.kr = defaultKeyring(a.opts.Getenv, func(message string) { a.warn("%s", message) })
	})
	return a.kr
}

// tokenFunc supplies the device credential of a server (I161). A Unix server that
// never paired has no credential to look for, so a person on the host is not asked
// to unlock a keyring the transport does not need.
func (a *app) tokenFunc(name string, entry *serverEntry) apiclient.TokenFunc {
	if entry.Kind == apiclient.KindUnix && entry.DeviceID == "" {
		return nil
	}
	var (
		cached string
		err    error
		read   bool
	)
	return func() (string, error) {
		if !read {
			cached, err = a.keyring().Get(keyringService, deviceAccount(name))
			read = true
		}
		return cached, err
	}
}

// probeKeyring proves the keyring works before something irreversible depends on it
// (I166). It writes and removes a throwaway entry.
func (a *app) probeKeyring() error {
	account := "probe/" + strings.ReplaceAll(a.opts.Now().UTC().Format("20060102T150405.000000000"), ".", "")
	kr := a.keyring()
	if err := kr.Set(keyringService, account, "probe"); err != nil {
		return keyringError(err)
	}
	if err := kr.Delete(keyringService, account); err != nil {
		return keyringError(err)
	}
	return nil
}

// keyringError keeps the clear message of the contract (§15.6).
func keyringError(err error) error {
	if errors.Is(err, security.ErrKeyringUnavailable) {
		return errs.Wrap(errs.CodeFailedPrecondition, err, security.ErrKeyringUnavailable.Error())
	}
	return errs.Wrap(errs.CodeInternal, err, "keyring operation failed")
}

// forgetCredential removes a server's credential. An absent entry or a keyring that
// cannot be reached is not an error here: there is nothing more to protect locally.
func (a *app) forgetCredential(server string) {
	if err := a.keyring().Delete(keyringService, deviceAccount(server)); err != nil && !errors.Is(err, security.ErrKeyringNotFound) {
		a.warn("could not remove the stored credential of %s: %v", server, err)
	}
}

// readSecret takes a secret value through a no-echo prompt on a terminal, otherwise
// from stdin, and never from an argument (R36).
func (a *app) readSecret(ctx context.Context, prompt string) (string, error) {
	var (
		raw []byte
		err error
	)
	if a.opts.StdinIsTerminal() {
		fmt.Fprint(a.stderr(), prompt)
		raw, err = a.opts.ReadPassword(ctx)
		fmt.Fprintln(a.stderr())
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "", errs.Wrap(errs.CodeInvalidArgument, err, "cannot read from the terminal")
		}
	} else {
		raw, err = io.ReadAll(io.LimitReader(a.opts.Stdin, maxSecretBytes+1))
		if err != nil {
			return "", errs.Wrap(errs.CodeInvalidArgument, err, "cannot read standard input")
		}
		if len(raw) > maxSecretBytes {
			return "", errs.New(errs.CodeInvalidArgument, "the value is larger than 1 MiB")
		}
		// One trailing line break is how echo and files end; anything else is content.
		value := strings.TrimSuffix(string(raw), "\n")
		value = strings.TrimSuffix(value, "\r")
		raw = []byte(value)
	}
	if len(raw) == 0 {
		return "", errs.New(errs.CodeInvalidArgument, "the value is empty")
	}
	return string(raw), nil
}

// confirm asks a yes-or-no question on the terminal. With no terminal and no --yes it
// refuses, because a script must decide explicitly (I178).
func (a *app) confirm(ctx context.Context, question string, yes bool) error {
	if yes {
		return nil
	}
	if !a.opts.StdinIsTerminal() {
		return errs.Newf(errs.CodeFailedPrecondition, "%s Refusing without a terminal; pass --yes to confirm", question)
	}
	fmt.Fprintf(a.stderr(), "%s [y/N] ", question)
	type answer struct {
		line string
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		line, err := readLine(a.opts.Stdin)
		answered <- answer{line, err}
	}()
	var got answer
	select {
	case got = <-answered:
	case <-ctx.Done():
		fmt.Fprintln(a.stderr())
		return ctx.Err()
	}
	if got.err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, got.err, "cannot read the answer")
	}
	switch strings.ToLower(strings.TrimSpace(got.line)) {
	case "y", "yes":
		return nil
	default:
		return errs.New(errs.CodeFailedPrecondition, "not confirmed")
	}
}

// readLine reads up to a line break, one byte at a time, so nothing past it is taken
// from stdin.
func readLine(r io.Reader) (string, error) {
	var (
		line []byte
		one  [1]byte
	)
	for len(line) < 4096 {
		n, err := r.Read(one[:])
		if n == 1 {
			if one[0] == '\n' {
				return string(line), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return string(line), nil
			}
			return "", err
		}
	}
	return string(line), nil
}
