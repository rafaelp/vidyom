package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

func (a *app) serverCommand() *cobra.Command {
	return group("server", "Configure which daemons this client talks to",
		a.serverAddCommand(), a.serverPairCommand(), a.serverUseCommand(), a.serverListCommand(), a.serverRemoveCommand())
}

type serverView struct {
	Name     string         `json:"name"`
	Kind     apiclient.Kind `json:"kind"`
	Endpoint string         `json:"endpoint"`
	Pinned   bool           `json:"pinned,omitempty"`
	Tenant   string         `json:"tenant,omitempty"`
	DeviceID string         `json:"device_id,omitempty"`
	Email    string         `json:"email,omitempty"`
	Current  bool           `json:"current"`
}

func viewServer(name string, entry *serverEntry, current bool) serverView {
	return serverView{
		Name: name, Kind: entry.Kind, Endpoint: entry.endpoint().Display(), Pinned: entry.Fingerprint != "",
		Tenant: entry.Tenant, DeviceID: entry.DeviceID, Email: entry.DeviceEmail, Current: current,
	}
}

func (a *app) serverAddCommand() *cobra.Command {
	var (
		entry serverEntry
		force bool
	)
	cmd := &cobra.Command{
		Use:   "add NAME (--url URL [--fingerprint SHA256] | --socket PATH | --ssh USER@HOST)",
		Short: "Add a server: HTTPS with pairing, a Unix socket on this host, or an SSH tunnel for host operators",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := validateServerName(name); err != nil {
				return err
			}
			add := entry
			transports := 0
			for _, set := range []string{add.URL, add.Socket, add.SSHTarget} {
				if set != "" {
					transports++
				}
			}
			if transports != 1 {
				return usagef("give exactly one of --url, --socket or --ssh")
			}
			switch {
			case add.URL != "":
				add.Kind = apiclient.KindHTTPS
				if err := apiclient.ValidateHTTPSURL(add.URL); err != nil {
					return err
				}
				add.URL = strings.TrimRight(add.URL, "/")
				if add.Fingerprint != "" {
					normalized, err := apiclient.NormalizeFingerprint(add.Fingerprint)
					if err != nil {
						return err
					}
					add.Fingerprint = normalized
				}
			case add.Socket != "":
				add.Kind = apiclient.KindUnix
			default:
				add.Kind = apiclient.KindSSH
				if add.RemoteSocket == "" {
					add.RemoteSocket = defaultRemoteSocket
				}
			}
			if add.Kind != apiclient.KindHTTPS && (add.Fingerprint != "" || add.CAFile != "" || add.ClientCert != "" || add.ClientKey != "") {
				return usagef("--fingerprint, --ca-file, --client-cert and --client-key only apply to --url")
			}
			if add.Kind != apiclient.KindSSH && add.RemoteSocket != "" {
				return usagef("--remote-socket only applies to --ssh")
			}
			if add.Fingerprint != "" && add.CAFile != "" {
				return usagef("give --fingerprint or --ca-file, not both")
			}
			if (add.ClientCert == "") != (add.ClientKey == "") {
				return usagef("--client-cert and --client-key go together")
			}
			// Later commands run from anywhere, so stored paths must not depend on where
			// this one ran.
			for _, path := range []*string{&add.Socket, &add.CAFile, &add.ClientCert, &add.ClientKey} {
				if *path == "" {
					continue
				}
				abs, err := filepath.Abs(*path)
				if err != nil {
					return errs.Wrap(errs.CodeInvalidArgument, err, "cannot resolve "+*path)
				}
				*path = abs
			}
			// Fails early on a bad target, CA file or client certificate.
			probe, err := a.newClient(name, &add, "")
			if err != nil {
				return err
			}
			_ = probe.Close()

			cfg, err := a.config()
			if err != nil {
				return err
			}
			if existing, ok := cfg.Servers[name]; ok {
				if !force {
					return errs.Newf(errs.CodeConflict, "server %q already exists; pass --force to replace it", name)
				}
				if existing.sameEndpoint(&add) {
					// Same place: keep the pairing and the tenant.
					add.Tenant, add.DeviceID, add.DeviceEmail = existing.Tenant, existing.DeviceID, existing.DeviceEmail
					add.ClientSecrets = existing.ClientSecrets
				} else if existing.DeviceID != "" {
					// A credential never follows an old name to a new endpoint (I163).
					a.forgetCredential(name)
				}
			}
			cfg.Servers[name] = &add
			if cfg.Current == "" {
				cfg.Current = name
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			view := viewServer(name, &add, cfg.Current == name)
			return a.emit(view, func(w io.Writer) {
				fmt.Fprintf(w, "Added server %s (%s).\n", name, view.Endpoint)
				if add.Kind != apiclient.KindUnix && add.DeviceID == "" {
					fmt.Fprintf(w, "Pair it with: vidyom server pair %s --code <one-time-code>\n", name)
				}
			})
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&entry.URL, "url", "", "HTTPS URL of the daemon: https://host[:port]")
	flags.StringVar(&entry.Fingerprint, "fingerprint", "", "SHA-256 of the server's TLS certificate, for a self-signed certificate")
	flags.StringVar(&entry.CAFile, "ca-file", "", "PEM file with the CA that signed the server's certificate")
	flags.StringVar(&entry.ClientCert, "client-cert", "", "client certificate file, when the server requires mTLS")
	flags.StringVar(&entry.ClientKey, "client-key", "", "client key file, when the server requires mTLS")
	flags.StringVar(&entry.Socket, "socket", "", "Unix socket of a daemon on this host")
	flags.StringVar(&entry.SSHTarget, "ssh", "", "user@host, through the system ssh client (for host operators)")
	flags.StringVar(&entry.RemoteSocket, "remote-socket", "", "the daemon's socket on the SSH host (default "+defaultRemoteSocket+")")
	flags.BoolVar(&force, "force", false, "replace an existing server; a changed endpoint also drops its stored credential")
	return cmd
}

func (a *app) serverPairCommand() *cobra.Command {
	var code, deviceName string
	cmd := &cobra.Command{
		Use:   "pair [NAME] [--code CODE]",
		Short: "Exchange a one-time pairing code for this device's credential, stored in the OS keyring",
		Long:  "Exchanges a one-time pairing code for a per-device credential, which is stored in the OS keyring and never printed.\nWithout --code, the code is read through a no-echo prompt or from standard input.",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if err := validateServerName(args[0]); err != nil {
					return err
				}
				a.serverFlag = args[0]
			}
			name, entry, implicit, err := a.resolveServer()
			if err != nil {
				return err
			}
			// Before spending the one-time code (I166).
			if err := a.probeKeyring(); err != nil {
				return err
			}
			if code == "" {
				code, err = a.readSecret(cmd.Context(), "One-time pairing code: ")
				if err != nil {
					return err
				}
			}
			if deviceName == "" {
				deviceName, _ = a.opts.Hostname()
			}
			client, err := a.newClient(name, entry, "")
			if err != nil {
				return err
			}
			defer client.Close()
			issued, err := apiclient.Invoke[api.DeviceCredentialResponse](cmd.Context(), client, apiclient.Call{
				Route:          api.RouteDevicesPair,
				Body:           api.PairDeviceRequest{Code: strings.TrimSpace(code), DeviceName: deviceName},
				IdempotencyKey: a.idempotencyKey,
			})
			if err != nil {
				return err
			}
			if issued.Token == "" {
				return errs.New(errs.CodeInternal, "the server issued no credential")
			}
			if err := a.keyring().Set(keyringService, deviceAccount(name), issued.Token); err != nil {
				return errs.Wrap(errs.CodeFailedPrecondition, err, fmt.Sprintf(
					"device %s was issued, but its credential cannot be stored in the OS keyring and was discarded; an admin should revoke that device, then pair again", issued.Device.ID))
			}
			cfg, err := a.config()
			if err != nil {
				return err
			}
			if implicit {
				cfg.Servers[name] = entry
				if cfg.Current == "" {
					cfg.Current = name
				}
			}
			entry.DeviceID, entry.DeviceEmail = string(issued.Device.ID), issued.Device.Email
			if err := a.saveConfig(); err != nil {
				return err
			}
			out := struct {
				Server string        `json:"server"`
				Device domain.Device `json:"device"`
			}{name, issued.Device}
			return a.emit(out, func(w io.Writer) {
				fmt.Fprintf(w, "Paired with %s as %s", name, clean(string(issued.Device.ID)))
				if issued.Device.Email != "" {
					fmt.Fprintf(w, " (%s)", clean(issued.Device.Email))
				}
				fmt.Fprintln(w, ". The credential is in the OS keyring.")
			})
		},
	}
	cmd.Flags().StringVar(&code, "code", "", "one-time pairing code; omit to be prompted")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "name of this device (default: the host name)")
	return cmd
}

func (a *app) serverUseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "use NAME",
		Short: "Make a server the current one",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			entry, ok := cfg.Servers[args[0]]
			if !ok {
				return errs.Newf(errs.CodeNotFound, "no server named %q; see 'vidyom server list'", args[0])
			}
			cfg.Current = args[0]
			if err := a.saveConfig(); err != nil {
				return err
			}
			view := viewServer(args[0], entry, true)
			return a.emit(view, func(w io.Writer) { fmt.Fprintf(w, "Current server: %s (%s)\n", args[0], view.Endpoint) })
		},
	}
}

func (a *app) serverListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured servers",
		Args:  noArgs(),
		RunE: func(*cobra.Command, []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			items := []serverView{}
			for _, name := range cfg.names() {
				items = append(items, viewServer(name, cfg.Servers[name], cfg.Current == name))
			}
			if len(items) == 0 {
				if entry, err := a.implicitEntry(); err == nil {
					items = append(items, viewServer(implicitServer, entry, true))
				}
			}
			return a.emit(api.List[serverView]{Items: items}, func(w io.Writer) {
				rows := make([][]string, 0, len(items))
				for _, s := range items {
					mark := ""
					if s.Current {
						mark = "*"
					}
					rows = append(rows, []string{mark, s.Name, string(s.Kind), s.Endpoint, s.Tenant, s.DeviceID})
				}
				table(w, []string{"", "NAME", "KIND", "ENDPOINT", "TENANT", "DEVICE"}, rows)
			})
		},
	}
}

func (a *app) serverRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a server and its stored credential; the device stays valid on the server until revoked",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			entry, ok := cfg.Servers[args[0]]
			if !ok {
				return errs.Newf(errs.CodeNotFound, "no server named %q", args[0])
			}
			if entry.DeviceID != "" {
				a.forgetCredential(args[0])
			}
			a.forgetClientSecrets(args[0], entry)
			delete(cfg.Servers, args[0])
			if cfg.Current == args[0] {
				cfg.Current = ""
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			return a.emit(map[string]string{"removed": args[0]}, func(w io.Writer) { fmt.Fprintf(w, "Removed server %s.\n", args[0]) })
		},
	}
}

func (a *app) forgetClientSecrets(server string, entry *serverEntry) {
	for tenant, names := range entry.ClientSecrets {
		for _, name := range names {
			err := a.keyring().Delete(keyringService, secretAccount(server, tenant, name))
			if err != nil && !errors.Is(err, security.ErrKeyringNotFound) {
				a.warn("could not remove client secret %s: %v", name, err)
			}
		}
	}
}

// ---- Devices ----

func (a *app) deviceCommand() *cobra.Command {
	var (
		ttl, userEmail string
		lf             listFlags
		reveal         bool
	)
	pairingCreate := &cobra.Command{
		Use:   "create",
		Short: "Create a one-time pairing code (admin)",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			seconds, err := parseTTLSeconds("ttl", ttl)
			if err != nil {
				return err
			}
			resp, err := invoke[api.PairingCodeResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RoutePairingCodes,
				Body:  api.CreatePairingCodeRequest{UserEmail: userEmail, TTLSeconds: seconds},
			})
			if err != nil {
				return err
			}
			return a.emit(resp, func(w io.Writer) {
				fields(w, kv("code", resp.Code), kv("expires", formatTime(resp.ExpiresAt)), kv("tls fingerprint", resp.TLSFingerprint))
				if resp.TLSFingerprint != "" {
					fmt.Fprintf(w, "Pin it on the client with: vidyom server add <name> --url https://... --fingerprint %s\n", clean(resp.TLSFingerprint))
				}
			})
		},
	}
	pairingCreate.Flags().StringVar(&ttl, "ttl", "", "how long the code is valid, such as 15m")
	pairingCreate.Flags().StringVar(&userEmail, "user", "", "user the device will belong to (default: you)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List paired devices",
		Args:  noArgs(),
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.listDevices(cmd, &lf) },
	}
	lf.add(list)

	revoke := &cobra.Command{
		Use:   "revoke CLIENT-nnn",
		Short: "Revoke a device; its credential stops working at once",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.revokeDevice(cmd, args[0]) },
	}
	rotate := &cobra.Command{
		Use:   "rotate [CLIENT-nnn]",
		Short: "Replace a device's credential (default: this device)",
		Long:  "Replaces a device's credential. For this device, the new credential goes to the OS keyring and is not printed.\nFor another device, it is discarded and that device must pair again, unless --reveal prints it once.",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, entry, _, err := a.resolveServer()
			if err != nil {
				return err
			}
			ref := entry.DeviceID
			if len(args) == 1 {
				ref = args[0]
			}
			if ref == "" {
				return usagef("this client has no device on %s; name the device to rotate", name)
			}
			if err := validateRef("device", ref); err != nil {
				return err
			}
			issued, err := invoke[api.DeviceCredentialResponse](cmd.Context(), a, apiclient.Call{
				Route: api.RouteDevicesRotate, Path: apiclient.PathParam("id", ref), Body: struct{}{},
			})
			if err != nil {
				return err
			}
			out := struct {
				Device domain.Device `json:"device"`
				Stored bool          `json:"stored_in_keyring"`
				Token  string        `json:"token,omitempty"`
			}{Device: issued.Device}
			if string(issued.Device.ID) == entry.DeviceID {
				if err := a.keyring().Set(keyringService, deviceAccount(name), issued.Token); err != nil {
					return errs.Wrap(errs.CodeFailedPrecondition, err, "the credential was rotated on the server but cannot be stored in the OS keyring; pair this client again")
				}
				out.Stored = true
			} else if reveal {
				a.warn("printing a device credential; deliver it securely and clear it from this terminal")
				out.Token = issued.Token
			}
			return a.emit(out, func(w io.Writer) {
				switch {
				case out.Stored:
					fmt.Fprintf(w, "Rotated %s. The new credential is in the OS keyring.\n", clean(string(issued.Device.ID)))
				case out.Token != "":
					fmt.Fprintf(w, "Rotated %s. New credential: %s\n", clean(string(issued.Device.ID)), out.Token)
				default:
					fmt.Fprintf(w, "Rotated %s. Its old credential no longer works; that device must pair again.\n", clean(string(issued.Device.ID)))
				}
			})
		},
	}
	rotate.Flags().BoolVar(&reveal, "reveal", false, "print the new credential of another device once")

	return group("device", "Manage pairing codes and paired devices",
		group("pairing-code", "Manage one-time pairing codes", pairingCreate), list, revoke, rotate)
}

func (a *app) listDevices(cmd *cobra.Command, lf *listFlags) error {
	list, err := fetchList[domain.Device](cmd.Context(), a, apiclient.Call{Route: api.RouteDevicesList, Query: apiclient.Query(lf.page())}, lf)
	if err != nil {
		return err
	}
	return a.emit(list, func(w io.Writer) {
		rows := make([][]string, 0, len(list.Items))
		for _, d := range list.Items {
			rows = append(rows, []string{string(d.ID), d.Email, d.Name, string(d.Status), formatTimePtr(d.LastSeenAt), formatTime(d.CreatedAt)})
		}
		table(w, []string{"ID", "USER", "NAME", "STATUS", "LAST SEEN", "PAIRED"}, rows)
		a.noteNextPage(list.NextCursor)
	})
}

func (a *app) revokeDevice(cmd *cobra.Command, ref string) error {
	if err := validateRef("device", ref); err != nil {
		return err
	}
	device, err := invoke[domain.Device](cmd.Context(), a, apiclient.Call{
		Route: api.RouteDevicesRevoke, Path: apiclient.PathParam("id", ref), Body: struct{}{},
	})
	if err != nil {
		return err
	}
	// Our own credential is dead now; keeping it would only mislead (I167).
	name, entry, _, err := a.resolveServer()
	if err == nil && entry.DeviceID != "" && (ref == entry.DeviceID || string(device.ID) == entry.DeviceID) {
		a.forgetCredential(name)
		entry.DeviceID, entry.DeviceEmail = "", ""
		if err := a.saveConfig(); err != nil {
			return err
		}
	}
	return a.emit(device, func(w io.Writer) { fmt.Fprintf(w, "Revoked %s.\n", clean(string(device.ID))) })
}

// ---- Tenants and users ----

func (a *app) tenantCommand() *cobra.Command {
	var createName string
	list := &cobra.Command{
		Use:   "list",
		Short: "List your tenants and your role in each",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[api.TenantInfo](cmd.Context(), a, apiclient.Call{Route: api.RouteTenantsList}, nil)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				_, entry, _, _ := a.resolveServer()
				rows := make([][]string, 0, len(items.Items))
				for _, t := range items.Items {
					mark := ""
					if entry != nil && entry.Tenant == string(t.ID) {
						mark = "*"
					}
					rows = append(rows, []string{mark, string(t.ID), t.Name, string(t.Role)})
				}
				table(w, []string{"", "ID", "NAME", "ROLE"}, rows)
			})
		},
	}
	use := &cobra.Command{
		Use:   "use TENANT",
		Short: "Run later commands against this tenant on the current server",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant := args[0]
			if err := domain.TenantID(tenant).Validate(); err != nil {
				return usagef("%v", err)
			}
			name, entry, implicit, err := a.resolveServer()
			if err != nil {
				return err
			}
			list, listErr := invoke[api.List[api.TenantInfo]](cmd.Context(), a, apiclient.Call{Route: api.RouteTenantsList})
			switch {
			case listErr == nil:
				member := false
				for _, t := range list.Items {
					member = member || string(t.ID) == tenant
				}
				if !member {
					return errs.Newf(errs.CodeNotFound, "tenant %q was not found among yours on %s; see 'vidyom tenant list'", tenant, name)
				}
			case errs.IsCode(listErr, errs.CodeUnavailable), errs.IsCode(listErr, errs.CodeUnauthenticated):
				// May be chosen before pairing or with the daemon stopped.
				a.warn("could not check the tenant on %s: %v", name, errs.Public(listErr).Message)
			default:
				return listErr
			}
			cfg, err := a.config()
			if err != nil {
				return err
			}
			if implicit {
				cfg.Servers[name] = entry
				if cfg.Current == "" {
					cfg.Current = name
				}
			}
			entry.Tenant = tenant
			if err := a.saveConfig(); err != nil {
				return err
			}
			out := map[string]string{"server": name, "tenant": tenant}
			return a.emit(out, func(w io.Writer) { fmt.Fprintf(w, "Current tenant on %s: %s\n", name, tenant) })
		},
	}
	create := &cobra.Command{
		Use:   "create TENANT",
		Short: "Create a tenant (system admin)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := domain.TenantID(args[0]).Validate(); err != nil {
				return usagef("%v", err)
			}
			tenant, err := invoke[domain.Tenant](cmd.Context(), a, apiclient.Call{
				Route: api.RouteTenantsAdd,
				Body:  api.CreateTenantRequest{ID: domain.TenantID(args[0]), Name: createName},
			})
			if err != nil {
				return err
			}
			return a.emit(tenant, func(w io.Writer) { fmt.Fprintf(w, "Created tenant %s.\n", clean(string(tenant.ID))) })
		},
	}
	create.Flags().StringVar(&createName, "name", "", "display name")
	return group("tenant", "List, select and create tenants", list, use, create)
}

func (a *app) userCommand() *cobra.Command {
	var (
		role       string
		permission string
		revoke     bool
		lf         listFlags
	)
	add := &cobra.Command{
		Use:   "add EMAIL --role ROLE",
		Short: "Add a user to the tenant with a role: viewer, editor, publisher or admin",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !domain.Role(role).Valid() {
				return usagef("invalid --role %q: expected viewer, editor, publisher or admin", role)
			}
			member, err := invoke[domain.Membership](cmd.Context(), a, apiclient.Call{
				Route: api.RouteUsersAdd, Body: api.AddUserRequest{Email: args[0], Role: domain.Role(role)},
			})
			if err != nil {
				return err
			}
			return a.emit(member, func(w io.Writer) {
				fmt.Fprintf(w, "Added %s to %s as %s.\n", clean(args[0]), clean(string(member.TenantID)), clean(string(member.Role)))
			})
		},
	}
	add.Flags().StringVar(&role, "role", "", "viewer, editor, publisher or admin")
	list := &cobra.Command{
		Use:   "list",
		Short: "List the tenant's users",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := fetchList[domain.Membership](cmd.Context(), a, apiclient.Call{Route: api.RouteUsersList, Query: apiclient.Query(lf.page())}, &lf)
			if err != nil {
				return err
			}
			return a.emit(items, func(w io.Writer) {
				rows := make([][]string, 0, len(items.Items))
				for _, m := range items.Items {
					grants := make([]string, len(m.Grants))
					for i, g := range m.Grants {
						grants[i] = string(g)
					}
					rows = append(rows, []string{m.Email, string(m.Role), strings.Join(grants, ",")})
				}
				table(w, []string{"USER", "ROLE", "GRANTS"}, rows)
				a.noteNextPage(items.NextCursor)
			})
		},
	}
	lf.add(list)
	grant := &cobra.Command{
		Use:   "grant EMAIL --permission PERMISSION [--revoke]",
		Short: "Grant or revoke one permission without changing the user's role",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if permission == "" {
				return usagef("--permission is required")
			}
			member, err := invoke[domain.Membership](cmd.Context(), a, apiclient.Call{
				Route: api.RouteUsersGrant, Path: apiclient.PathParam("id", args[0]),
				Body: api.GrantRequest{Permission: domain.Permission(permission), Revoke: revoke},
			})
			if err != nil {
				return err
			}
			return a.emit(member, func(w io.Writer) { fmt.Fprintf(w, "Updated %s.\n", clean(args[0])) })
		},
	}
	grant.Flags().StringVar(&permission, "permission", "", "permission, such as storage.archive.restore")
	grant.Flags().BoolVar(&revoke, "revoke", false, "revoke the permission instead")
	return group("user", "Manage the tenant's users", add, list, grant)
}

// ---- Budget ----

func (a *app) budgetCommand() *cobra.Command {
	var (
		monthly, currency, reason, ttl string
	)
	show := &cobra.Command{
		Use:   "show",
		Short: "Show this month's estimated spending against the tenant's budget",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := invoke[domain.BudgetStatus](cmd.Context(), a, apiclient.Call{Route: api.RouteBudgetGet})
			if err != nil {
				return err
			}
			return a.emit(status, func(w io.Writer) { budgetLines(w, status) })
		},
	}
	set := &cobra.Command{
		Use:   "set --monthly AMOUNT --currency CODE",
		Short: "Set the monthly budget, in major units such as 500 USD (admin)",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if monthly == "" || currency == "" {
				return usagef("--monthly and --currency are required")
			}
			code := strings.ToUpper(currency)
			minor, err := parseMoney(monthly, code)
			if err != nil {
				return err
			}
			status, err := invoke[domain.BudgetStatus](cmd.Context(), a, apiclient.Call{
				Route: api.RouteBudgetPut, Body: api.SetBudgetRequest{MonthlyMinor: minor, Currency: code},
			})
			if err != nil {
				return err
			}
			return a.emit(status, func(w io.Writer) { budgetLines(w, status) })
		},
	}
	set.Flags().StringVar(&monthly, "monthly", "", "monthly amount in major units, such as 500 or 12.50")
	set.Flags().StringVar(&currency, "currency", "", "ISO 4217 code, such as USD")
	override := &cobra.Command{
		Use:   "override --reason TEXT [--ttl DURATION]",
		Short: "Let paid operations run past 100% of the budget (admin; audited)",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if reason == "" {
				return usagef("--reason is required")
			}
			seconds, err := parseTTLSeconds("ttl", ttl)
			if err != nil {
				return err
			}
			status, err := invoke[domain.BudgetStatus](cmd.Context(), a, apiclient.Call{
				Route: api.RouteBudgetOver, Body: api.BudgetOverrideRequest{Reason: reason, TTLSeconds: seconds},
			})
			if err != nil {
				return err
			}
			return a.emit(status, func(w io.Writer) { budgetLines(w, status) })
		},
	}
	override.Flags().StringVar(&reason, "reason", "", "why; recorded in the audit")
	override.Flags().StringVar(&ttl, "ttl", "", "how long the override lasts, such as 24h")
	return group("budget", "Show and change the tenant's monthly budget", show, set, override)
}

func budgetLines(w io.Writer, status domain.BudgetStatus) {
	fields(w,
		kv("budget", formatMoney(status.Budget.MonthlyMinor, status.Budget.Currency)+" per month"),
		kv("period", status.Period),
		kv("spent", formatMoney(status.SpentMinor, status.Budget.Currency)+fmt.Sprintf(" (%.1f%%)", status.Percent)),
		kv("blocked", yesNo(status.Blocked)),
		kv("override until", formatTimePtr(status.OverrideUntil)),
	)
}
