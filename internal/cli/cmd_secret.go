package cli

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// secretFlagError never repeats what was typed: on a secret command, that could be
// the secret (R36).
func secretFlagError(cmd *cobra.Command, _ error) error {
	return usagef("invalid flags for '%s': a secret's value is never a flag or an argument; it is read from a no-echo prompt or from standard input", cmd.CommandPath())
}

// clientSecretContext is where client-location secrets of this command live.
func (a *app) clientSecretContext() (server, tenant string, entry *serverEntry, err error) {
	server, entry, _, err = a.resolveServer()
	if err != nil {
		return "", "", nil, err
	}
	tenant, err = a.currentTenant(entry)
	if err != nil {
		return "", "", nil, err
	}
	if tenant == "" {
		return "", "", nil, errs.New(errs.CodeTenantRequired, "no tenant selected")
	}
	return server, tenant, entry, nil
}

// isLocalSecret decides where a secret command acts (I177).
func isLocalSecret(entry *serverEntry, tenant, name, location string) bool {
	switch location {
	case string(domain.SecretClient):
		return true
	case "":
		return slices.Contains(entry.ClientSecrets[tenant], name)
	default:
		return false
	}
}

func (a *app) secretCommand() *cobra.Command {
	var (
		addLocation, addTTL string
		rotateLocation      string
		testLocation        string
		deleteLocation      string
		lf                  listFlags
	)
	add := &cobra.Command{
		Use:   "add NAME --location (server|delegated|client) [--ttl DURATION]",
		Short: "Store a secret; the value comes from a no-echo prompt or stdin, never an argument",
		Long: "Stores a secret. The value is read through a no-echo prompt on a terminal, or from standard input; it is never an argument or a flag.\n" +
			"server: the tenant's encrypted vault, preferred for autonomous jobs. delegated: same, temporary, needs --ttl. client: this machine's OS keyring, and it never leaves this machine.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !secretNamePattern.MatchString(name) {
				return usagef("invalid secret name: use letters, digits, '.', '_' and '-'")
			}
			location := domain.SecretLocation(addLocation)
			switch location {
			case domain.SecretServer, domain.SecretDelegated, domain.SecretClient:
			case "":
				return usagef("--location is required: server, delegated or client")
			default:
				return usagef("invalid --location: expected server, delegated or client")
			}
			seconds, err := parseTTLSeconds("ttl", addTTL)
			if err != nil {
				return err
			}
			if location == domain.SecretClient && seconds > 0 {
				return usagef("--ttl does not apply to --location client")
			}
			if location == domain.SecretDelegated && seconds == 0 {
				return usagef("--location delegated needs --ttl: a delegated credential has a limited duration")
			}
			var local struct {
				server, tenant string
				entry          *serverEntry
			}
			if location == domain.SecretClient {
				local.server, local.tenant, local.entry, err = a.clientSecretContext()
				if err != nil {
					return err
				}
				if err := a.probeKeyring(); err != nil {
					return err
				}
			} else if _, err := a.api(); err != nil {
				return err
			}
			value, err := a.readSecret(fmt.Sprintf("Value of %s (not echoed): ", name))
			if err != nil {
				return err
			}
			if location == domain.SecretClient {
				if err := a.storeClientSecret(local.server, local.tenant, local.entry, name, value); err != nil {
					return err
				}
				meta := domain.SecretMeta{Name: name, TenantID: domain.TenantID(local.tenant), Location: domain.SecretClient}
				return a.emit(meta, func(w io.Writer) { fmt.Fprintf(w, "Stored %s in this machine's OS keyring.\n", name) })
			}
			meta, err := invoke[domain.SecretMeta](cmd.Context(), a, apiclient.Call{
				Route: api.RouteSecretsPut,
				Body:  api.PutSecretRequest{Name: name, Location: location, Value: value, TTLSeconds: seconds},
			})
			if err != nil {
				return err
			}
			return a.emit(meta, func(w io.Writer) {
				fmt.Fprintf(w, "Stored %s in the tenant's vault (%s).\n", clean(meta.Name), clean(string(meta.Location)))
			})
		},
	}
	add.SetFlagErrorFunc(secretFlagError)
	add.Flags().StringVar(&addLocation, "location", "", "server, delegated or client")
	add.Flags().StringVar(&addTTL, "ttl", "", "lifetime of a delegated secret, such as 2h")

	list := &cobra.Command{
		Use:   "list",
		Short: "List secrets: metadata only, values never leave",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[domain.SecretMeta](cmd.Context(), a, apiclient.Call{Route: api.RouteSecretsList, Query: apiclient.Query(lf.page())}, &lf)
			if err != nil {
				return err
			}
			if _, tenant, entry, err := a.clientSecretContext(); err == nil {
				names := append([]string(nil), entry.ClientSecrets[tenant]...)
				sort.Strings(names)
				for _, name := range names {
					items.Items = append(items.Items, domain.SecretMeta{Name: name, TenantID: domain.TenantID(tenant), Location: domain.SecretClient})
				}
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, s := range items.Items {
					rows = append(rows, []string{s.Name, string(s.Location), formatTime(s.CreatedAt), formatTimePtr(s.RotatedAt), formatTimePtr(s.ExpiresAt), formatTimePtr(s.LastUsedAt)})
				}
				table(w, []string{"NAME", "LOCATION", "CREATED", "ROTATED", "EXPIRES", "LAST USED"}, rows)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(list)

	test := &cobra.Command{
		Use:   "test NAME",
		Short: "Check that a secret can be used; exits non-zero when it cannot",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if server, tenant, entry, err := a.clientSecretContext(); err == nil && isLocalSecret(entry, tenant, name, testLocation) {
				resp := api.SecretTestResponse{OK: true, Detail: "present in this machine's OS keyring"}
				if _, err := a.keyring().Get(keyringService, secretAccount(server, tenant, name)); err != nil {
					resp = api.SecretTestResponse{OK: false, Detail: "not readable in this machine's OS keyring"}
					a.exitCode = exitFailed
				}
				return a.emit(resp, func(w io.Writer) { fields(w, kv("ok", yesNo(resp.OK)), kv("detail", resp.Detail)) })
			}
			resp, err := invoke[api.SecretTestResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RouteSecretsTest, Path: apiclient.PathParam("name", name), Body: struct{}{},
			})
			if err != nil {
				return err
			}
			if !resp.OK {
				a.exitCode = exitFailed
			}
			return a.emit(resp, func(w io.Writer) { fields(w, kv("ok", yesNo(resp.OK)), kv("detail", resp.Detail)) })
		},
	}
	test.Flags().StringVar(&testLocation, "location", "", "client, to test the local one when a name exists in both places")

	rotate := &cobra.Command{
		Use:   "rotate NAME",
		Short: "Replace a secret's value; the new one comes from a no-echo prompt or stdin",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			server, tenant, entry, localErr := a.clientSecretContext()
			local := localErr == nil && isLocalSecret(entry, tenant, name, rotateLocation)
			if !local {
				if _, err := a.api(); err != nil {
					return err
				}
			}
			value, err := a.readSecret(fmt.Sprintf("New value of %s (not echoed): ", name))
			if err != nil {
				return err
			}
			if local {
				if err := a.storeClientSecret(server, tenant, entry, name, value); err != nil {
					return err
				}
				meta := domain.SecretMeta{Name: name, TenantID: domain.TenantID(tenant), Location: domain.SecretClient}
				return a.emit(meta, func(w io.Writer) { fmt.Fprintf(w, "Rotated %s in this machine's OS keyring.\n", name) })
			}
			meta, err := invoke[domain.SecretMeta](cmd.Context(), a, apiclient.Call{
				Route: api.RouteSecretsRotate, Path: apiclient.PathParam("name", name), Body: api.RotateSecretRequest{Value: value},
			})
			if err != nil {
				return err
			}
			return a.emit(meta, func(w io.Writer) { fmt.Fprintf(w, "Rotated %s.\n", clean(meta.Name)) })
		},
	}
	rotate.SetFlagErrorFunc(secretFlagError)
	rotate.Flags().StringVar(&rotateLocation, "location", "", "client, to rotate the local one when a name exists in both places")

	deleteCmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a secret",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if server, tenant, entry, err := a.clientSecretContext(); err == nil && isLocalSecret(entry, tenant, name, deleteLocation) {
				err := a.keyring().Delete(keyringService, secretAccount(server, tenant, name))
				if err != nil && !errors.Is(err, security.ErrKeyringNotFound) {
					return keyringError(err)
				}
				entry.ClientSecrets[tenant] = slices.DeleteFunc(entry.ClientSecrets[tenant], func(s string) bool { return s == name })
				if err := a.saveConfig(); err != nil {
					return err
				}
				return a.emit(map[string]string{"deleted": name, "location": string(domain.SecretClient)}, func(w io.Writer) {
					fmt.Fprintf(w, "Deleted %s from this machine's OS keyring.\n", name)
				})
			}
			if _, err := invoke[struct{}](cmd.Context(), a, apiclient.Call{Route: api.RouteSecretsDelete, Path: apiclient.PathParam("name", name)}); err != nil {
				return err
			}
			return a.emit(map[string]string{"deleted": name}, func(w io.Writer) { fmt.Fprintf(w, "Deleted %s.\n", clean(name)) })
		},
	}
	deleteCmd.Flags().StringVar(&deleteLocation, "location", "", "client, to delete the local one when a name exists in both places")

	return group("secret", "Manage credentials: values are never shown", add, list, test, rotate, deleteCmd)
}

func (a *app) storeClientSecret(server, tenant string, entry *serverEntry, name, value string) error {
	if err := a.keyring().Set(keyringService, secretAccount(server, tenant, name), value); err != nil {
		return keyringError(err)
	}
	if entry.ClientSecrets == nil {
		entry.ClientSecrets = map[string][]string{}
	}
	if !slices.Contains(entry.ClientSecrets[tenant], name) {
		entry.ClientSecrets[tenant] = append(entry.ClientSecrets[tenant], name)
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if _, known := cfg.Servers[server]; !known {
		cfg.Servers[server] = entry
		if cfg.Current == "" {
			cfg.Current = server
		}
	}
	return a.saveConfig()
}
