package aiworker

import (
	"math"
	"testing"
)

func TestFakeHashEmbed(t *testing.T) {
	v := FakeHashEmbed("Conscious breathing")
	if len(v) != FakeEmbeddingDimensions {
		t.Fatalf("dimensions = %d", len(v))
	}
	var norm float64
	nonzero := 0
	for _, x := range v {
		norm += float64(x) * float64(x)
		if x != 0 {
			nonzero++
		}
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatalf("not normalized: %f", norm)
	}
	if nonzero != 2 {
		t.Fatalf("two tokens must fill two buckets, got %d", nonzero)
	}

	same := FakeHashEmbed("conscious, BREATHING!")
	for i := range v {
		if v[i] != same[i] {
			t.Fatal("case and punctuation must not matter")
		}
	}
	for _, x := range FakeHashEmbed("!!!") {
		if x != 0 {
			t.Fatal("no tokens must give a zero vector")
		}
	}

	dot := func(a, b []float32) (out float64) {
		for i := range a {
			out += float64(a[i]) * float64(b[i])
		}
		return out
	}
	doc := FakeHashEmbed("the story of conscious breathing in yoga")
	if dot(v, doc) <= dot(v, FakeHashEmbed("publishing to a destination")) {
		t.Fatal("shared words must score higher")
	}
}
