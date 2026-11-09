package approvalnodes

import (
	"encoding/json"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/schemas"
)

func TestDescriptorMatchesTheCatalog(t *testing.T) {
	r := node.NewRegistry()
	Register(r)
	impl, ok := r.Lookup(wfdef.TypeApprovalRequest, 1)
	if !ok {
		t.Fatal("not registered")
	}
	if err := schemas.CheckDescriptor(impl.Descriptor()); err != nil {
		t.Fatal(err)
	}
	if _, ok := impl.(node.Resumer); !ok {
		t.Fatal("a gate must resume")
	}
}

func TestEvidenceHelpers(t *testing.T) {
	evidence := json.RawMessage(`{"segments":[{"start_seconds":1,"end_seconds":2}],"confidence":0.5}`)
	if got := evidenceSegments(evidence); len(got) != 1 || got[0].EndSeconds != 2 {
		t.Fatalf("segments %+v", got)
	}
	if got := evidenceSegments(json.RawMessage(`"not an object"`)); got != nil {
		t.Fatalf("segments %+v", got)
	}
	if got := evidenceSegments(nil); got != nil {
		t.Fatalf("segments %+v", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(withSummary(evidence, "pick"), &doc); err != nil || doc["summary"] != "pick" || doc["confidence"] != 0.5 {
		t.Fatalf("doc %v %v", doc, err)
	}
	if err := json.Unmarshal(withSummary(json.RawMessage(`"text"`), "pick"), &doc); err != nil || doc["value"] != "text" {
		t.Fatalf("doc %v %v", doc, err)
	}
	if string(withSummary(evidence, "")) != string(evidence) {
		t.Fatal("no summary, no change")
	}
}
