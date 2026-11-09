package plugins

import (
	"strings"
	"testing"
)

func TestSeatbeltProfileAllowsOnlyThePolicy(t *testing.T) {
	profile := seatbeltProfile(policy{
		entry:      "/store/p/bin/plugin",
		pluginDir:  "/store/p",
		readPaths:  []string{`/odd "path"\here`},
		writePaths: []string{"/run/p/tmp"},
		execPaths:  []string{"/opt/python"},
		proxyPort:  4242,
	})
	for _, want := range []string{
		"(deny default)",
		`(literal "/store/p/bin/plugin")`,
		`(subpath "/opt/python")`,
		`(subpath "/odd \"path\"\\here")`,
		`file-write* (subpath "/run/p/tmp")`,
		`(remote ip "localhost:4242")`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %s:\n%s", want, profile)
		}
	}
	none := seatbeltProfile(policy{entry: "/store/p/bin/plugin", pluginDir: "/store/p"})
	if strings.Contains(none, "network") {
		t.Errorf("no grant, no network rule:\n%s", none)
	}
	if strings.Contains(none, "file-write*") {
		t.Errorf("no grant, nothing writable:\n%s", none)
	}
}
