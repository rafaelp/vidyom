package config

import (
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

func storages(archive, master, proxy StorageInstance) Storages {
	return Storages{Roles: map[domain.StorageRole]StorageInstance{
		domain.RoleArchive: archive, domain.RoleMaster: master, domain.RoleProxy: proxy,
	}}
}

func gcs(bucket, prefix string) StorageInstance {
	return StorageInstance{Driver: "gcs", Bucket: bucket, Prefix: prefix, CredentialsRef: "gcs-main"}
}

func local(root string) StorageInstance {
	return StorageInstance{Driver: "local", Root: root}
}

func TestSpecExampleIsDisjoint(t *testing.T) {
	// §7.2 example after R28.
	s := storages(gcs("videos-archive", ""), gcs("videos-standard", "masters/"), gcs("videos-standard", "proxies/"))
	if err := ValidateStorages(s); err != nil {
		t.Fatal(err)
	}
}

func TestOverlapRejected(t *testing.T) {
	cases := map[string]Storages{
		// The v1.0 bug R28 fixed: master with no prefix contains proxies/.
		"r28":       storages(gcs("videos-archive", ""), gcs("videos-standard", ""), gcs("videos-standard", "proxies/")),
		"same":      storages(gcs("a", ""), gcs("a", ""), gcs("b", "")),
		"nested":    storages(gcs("a", "x/"), gcs("a", "x/y/"), gcs("b", "")),
		"localroot": storages(local("/data/a"), local("/data/a/master"), local("/data/b")),
	}
	for name, s := range cases {
		if err := ValidateStorages(s); err == nil {
			t.Errorf("%s: expected overlap error", name)
		}
	}
}

func TestPrefixesAreNotStringPrefixesOfEachOther(t *testing.T) {
	// "master" and "master-old" do not overlap: comparison is by path segment.
	s := storages(gcs("a", "archive/"), gcs("a", "master/"), gcs("a", "master-old/"))
	if err := ValidateStorages(s); err != nil {
		t.Fatal(err)
	}
}

func TestMissingRoleRejected(t *testing.T) {
	s := Storages{Roles: map[domain.StorageRole]StorageInstance{domain.RoleMaster: gcs("a", "")}}
	if err := ValidateStorages(s); err == nil {
		t.Fatal("all roles are required")
	}
}

func TestDifferentDriversDoNotOverlap(t *testing.T) {
	s := storages(gcs("a", ""), local("/a/x"), local("/a/y"))
	if err := ValidateStorages(s); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedEndpoints(t *testing.T) {
	endpoints := []LLMEndpoint{
		{Name: "cloud", Class: EndpointExternal},
		{Name: "ollama", Class: EndpointLocal},
		{Name: "unclassified"},
	}
	local := AllowedEndpoints(endpoints, domain.AILocalOnly)
	if len(local) != 1 || local[0].Name != "ollama" {
		t.Fatalf("local_only = %+v", local)
	}
	// An empty policy is the default, local_only (D8), and an unclassified endpoint
	// is never treated as local.
	if got := AllowedEndpoints(endpoints, ""); len(got) != 1 || got[0].Name != "ollama" {
		t.Fatalf("default policy = %+v", got)
	}
	all := AllowedEndpoints(endpoints, domain.AIExternalAllowed)
	if len(all) != 3 || all[0].Name != "ollama" {
		t.Fatalf("external_allowed = %+v", all)
	}
	if got := AllowedEndpoints([]LLMEndpoint{{Name: "cloud", Class: EndpointExternal}}, domain.AILocalOnly); len(got) != 0 {
		t.Fatalf("no local endpoint must leave none: %+v", got)
	}
}
