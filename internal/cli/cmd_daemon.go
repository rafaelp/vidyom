package cli

import (
	"context"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/version"
)

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the client version, and the daemon's when it is reachable",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := struct {
				Client     string               `json:"client"`
				APIVersion string               `json:"api_version"`
				Server     *api.VersionResponse `json:"server,omitempty"`
			}{Client: version.Version, APIVersion: version.APIVersion}
			// The client's own version must not wait on a host that does not answer.
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			if server, err := invoke[api.VersionResponse](ctx, a, apiclient.Call{Route: api.RouteVersion}); err == nil {
				out.Server = &server
			}
			return a.emit(out, func(w io.Writer) {
				pairs := [][2]string{kv("client", out.Client), kv("api", out.APIVersion)}
				if out.Server != nil {
					pairs = append(pairs, kv("server", out.Server.Version), kv("server api", out.Server.APIVersion))
				}
				fields(w, pairs...)
			})
		},
	}
}

// health reads GET /health. An unhealthy daemon answers 503 with the same body, so
// it can say which component is down.
func (a *app) health(ctx context.Context) (api.HealthResponse, error) {
	resp, err := invoke[api.HealthResponse](ctx, a, apiclient.Call{Route: api.RouteHealth, AcceptStatus: []int{http.StatusServiceUnavailable}})
	if err != nil {
		return resp, err
	}
	if resp.Status == "" {
		return resp, errs.New(errs.CodeUnavailable, "the daemon is not available")
	}
	return resp, nil
}

func healthLines(w io.Writer, health api.HealthResponse) {
	if len(health.Components) == 0 {
		return
	}
	names := make([]string, 0, len(health.Components))
	for name := range health.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([][]string, 0, len(names))
	for _, name := range names {
		rows = append(rows, []string{name, health.Components[name].Status, health.Components[name].Detail})
	}
	table(w, []string{"COMPONENT", "STATUS", "DETAIL"}, rows)
}

func (a *app) daemonCommand() *cobra.Command {
	status := &cobra.Command{
		Use:   "status",
		Short: "Show which daemon this client talks to, its version and its state",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, entry, _, err := a.resolveServer()
			if err != nil {
				return err
			}
			versionInfo, err := invoke[api.VersionResponse](cmd.Context(), a, apiclient.Call{Route: api.RouteVersion})
			if err != nil {
				return err
			}
			health, err := a.health(cmd.Context())
			if err != nil {
				return err
			}
			out := struct {
				Server   string `json:"server"`
				Endpoint string `json:"endpoint"`
				api.VersionResponse
				Health api.HealthResponse `json:"health"`
			}{name, entry.endpoint().Display(), versionInfo, health}
			if health.Status != "ok" {
				a.exitCode = exitFailed
			}
			return a.emit(out, func(w io.Writer) {
				fields(w, kv("server", out.Server), kv("endpoint", out.Endpoint), kv("status", health.Status),
					kv("version", versionInfo.Version), kv("api", versionInfo.APIVersion), kv("sealing", health.Sealing))
				healthLines(w, health)
			})
		},
	}
	health := &cobra.Command{
		Use:   "health",
		Short: "Show the daemon's health; exits non-zero unless it is ok",
		Args:  noArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			health, err := a.health(cmd.Context())
			if err != nil {
				return err
			}
			if health.Status != "ok" {
				a.exitCode = exitFailed
			}
			return a.emit(health, func(w io.Writer) {
				fields(w, kv("status", health.Status), kv("version", health.Version), kv("sealing", health.Sealing))
				healthLines(w, health)
			})
		},
	}
	return group("daemon", "Inspect the daemon", status, health)
}
