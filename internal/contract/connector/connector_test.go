package connector

import (
	"context"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	var factory StorageFactory = func(context.Context, StorageParams) (Storage, error) { return nil, nil }
	r.RegisterStorage(Info{Driver: "local", Version: "1", Available: true}, factory)

	if _, err := r.Storage("local"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Storage("s3"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown driver must be not_found, got %v", err)
	}
	list := r.List()
	if len(list) != 1 || list[0].Family != FamilyStorage {
		t.Fatalf("List = %+v", list)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("duplicate driver must panic at wiring time")
		}
	}()
	r.RegisterStorage(Info{Driver: "local"}, factory)
}
