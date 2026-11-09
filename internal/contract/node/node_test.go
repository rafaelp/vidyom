package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

type stub struct{ d Descriptor }

func (s stub) Descriptor() Descriptor { return s.d }
func (s stub) Execute(context.Context, Env, Input) (Result, error) {
	return Result{}, nil
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.RegisterNode(stub{Descriptor{Type: "video.cut", TypeVersion: 1, Pool: PoolMedia, SideEffect: EffectIdempotent}})
	r.RegisterNode(stub{Descriptor{Type: "catalog.get", TypeVersion: 1, Pool: PoolDaemon, SideEffect: EffectNone}})

	if _, ok := r.Lookup("video.cut", 1); !ok {
		t.Fatal("lookup failed")
	}
	if _, ok := r.Lookup("video.cut", 2); ok {
		t.Fatal("wrong version matched")
	}
	list := r.List()
	if len(list) != 2 || list[0].Type != "catalog.get" {
		t.Fatalf("List = %+v", list)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate must panic")
		}
	}()
	r.RegisterNode(stub{Descriptor{Type: "video.cut", TypeVersion: 1}})
}

func TestFailureClassification(t *testing.T) {
	base := errors.New("boom")
	if !IsTransient(Transient(base)) || IsTransient(Permanent(base)) || IsTransient(base) {
		t.Error("IsTransient is wrong")
	}
	if !IsUnknownOutcome(Unknown(base)) || IsUnknownOutcome(Transient(base)) {
		t.Error("IsUnknownOutcome is wrong")
	}
	wrapped := fmt.Errorf("upload: %w", Unknown(base))
	if !IsUnknownOutcome(wrapped) || !errors.Is(wrapped, base) {
		t.Error("classification must survive wrapping")
	}
}

func TestIdempotencyKeyStable(t *testing.T) {
	job := id.New()
	a := IdempotencyKey("acme", job, "publish")
	if a != IdempotencyKey("acme", job, "publish") {
		t.Error("key must be stable across attempts (I36)")
	}
	if a == IdempotencyKey("other", job, "publish") || a == IdempotencyKey("acme", job, "other") || a == IdempotencyKey("acme", id.New(), "publish") {
		t.Error("key must depend on tenant, job and node")
	}
}

func TestBind(t *testing.T) {
	var v struct {
		Key string `json:"key"`
	}
	if err := (Input{Parameters: json.RawMessage(`{"key":"x"}`)}).Bind(&v); err != nil || v.Key != "x" {
		t.Fatalf("Bind: %v, %v", v, err)
	}
	if err := (Input{Parameters: json.RawMessage(`nope`)}).Bind(&v); err == nil {
		t.Fatal("invalid JSON must fail")
	}
}
