package bgem3

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/ai/workerclient"
	"github.com/rafaelp/vidyom/connectors/ai/workerclient/workertest"
	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jsonrpc"
)

func context30(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSplitsBatchesAndKeepsOrder(t *testing.T) {
	worker := workertest.Start(t, workertest.ShortTempDir(t))
	registry := connector.NewRegistry()
	Register(registry)
	factory, err := registry.Embedder(Driver)
	if err != nil {
		t.Fatal(err)
	}
	embedder, err := factory(context30(t), connector.AIParams{
		Daemon: config.AIWorker{Endpoint: worker.Endpoint(), Engine: aiworker.EngineFake},
	})
	if err != nil {
		t.Fatal(err)
	}
	info := embedder.Info()
	if !info.Capabilities["fake"] || !strings.Contains(info.Reason, "FAKE ENGINE") {
		t.Fatalf("the fake must be labeled: %+v", info)
	}

	texts := make([]string, 2*aiworker.MaxEmbedBatch+5)
	for i := range texts {
		texts[i] = fmt.Sprintf("passage number %d", i)
	}
	result, err := embedder.Embed(context30(t), connector.EmbedRequest{Texts: texts, Kind: connector.EmbedPassage})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != aiworker.FakeEmbeddingModel || result.Dimensions != aiworker.FakeEmbeddingDimensions {
		t.Fatalf("model %q dims %d", result.Model, result.Dimensions)
	}
	if len(result.Vectors) != len(texts) {
		t.Fatalf("%d vectors", len(result.Vectors))
	}
	for _, i := range []int{0, aiworker.MaxEmbedBatch - 1, aiworker.MaxEmbedBatch, len(texts) - 1} {
		want := aiworker.FakeHashEmbed(texts[i])
		for j := range want {
			if math.Abs(float64(result.Vectors[i][j]-want[j])) > 1e-6 {
				t.Fatalf("vector %d is out of order", i)
			}
		}
	}
}

func TestRejectsBadRequests(t *testing.T) {
	embedder := &Embedder{}
	if _, err := embedder.Embed(context.Background(), connector.EmbedRequest{Kind: connector.EmbedQuery}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no texts: %v", err)
	}
	if _, err := embedder.Embed(context.Background(), connector.EmbedRequest{Texts: []string{"x"}, Kind: "document"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad kind: %v", err)
	}
}

type stubBehavior func(call int, params aiworker.EmbedParams) aiworker.EmbedResult

func stubClient(t *testing.T, model string, behavior stubBehavior) *workerclient.Client {
	t.Helper()
	socket := filepath.Join(workertest.ShortTempDir(t), "stub.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var calls atomic.Int32
	go func() {
		for {
			stream, err := listener.Accept()
			if err != nil {
				return
			}
			conn := jsonrpc.NewConn(stream, stream, stream, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
				if method == aiworker.MethodInitialize {
					return aiworker.InitializeResult{ProtocolVersion: 1, Engine: aiworker.EngineReal, EmbeddingModel: model, EmbeddingDims: 2}, nil
				}
				var params aiworker.EmbedParams
				_ = json.Unmarshal(raw, &params)
				return behavior(int(calls.Add(1)), params), nil
			})
			go func() { _ = conn.Run(context.Background()) }()
		}
	}()
	dial, err := workerclient.DialerFor("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}
	client := workerclient.New(dial, workerclient.Options{})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func vectors(n, dims int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		out[i] = make([]float32, dims)
	}
	return out
}

func TestModelCheck(t *testing.T) {
	client := stubClient(t, "bge-m3@1", func(int, aiworker.EmbedParams) aiworker.EmbedResult { return aiworker.EmbedResult{} })
	for _, want := range []string{"", "bge-m3", "bge-m3@1"} {
		if _, err := New(context30(t), client, want); err != nil {
			t.Errorf("%q: %v", want, err)
		}
	}
	if _, err := New(context30(t), client, "bge-small"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("other model: %v", err)
	}
	if _, err := New(context30(t), client, "bge-m3@2"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("other version: %v", err)
	}
}

// Vectors of different models must never mix (R18), even inside one request.
func TestRefusesAModelChangeMidRequest(t *testing.T) {
	client := stubClient(t, "bge-m3@1", func(call int, p aiworker.EmbedParams) aiworker.EmbedResult {
		model := "bge-m3@1"
		if call > 1 {
			model = "bge-m3@2"
		}
		return aiworker.EmbedResult{Model: model, Dimensions: 2, Vectors: vectors(len(p.Texts), 2)}
	})
	embedder, err := New(context30(t), client, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = embedder.Embed(context30(t), connector.EmbedRequest{Texts: make([]string, aiworker.MaxEmbedBatch+1), Kind: connector.EmbedPassage})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusesWrongShapes(t *testing.T) {
	missing := stubClient(t, "bge-m3@1", func(_ int, p aiworker.EmbedParams) aiworker.EmbedResult {
		return aiworker.EmbedResult{Model: "bge-m3@1", Dimensions: 2, Vectors: vectors(len(p.Texts)-1, 2)}
	})
	embedder, err := New(context30(t), missing, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context30(t), connector.EmbedRequest{Texts: []string{"a", "b"}, Kind: connector.EmbedPassage}); !errs.IsCode(err, errs.CodeInternal) {
		t.Fatalf("missing vector: %v", err)
	}

	short := stubClient(t, "bge-m3@1", func(_ int, p aiworker.EmbedParams) aiworker.EmbedResult {
		return aiworker.EmbedResult{Model: "bge-m3@1", Dimensions: 2, Vectors: vectors(len(p.Texts), 1)}
	})
	embedder, err = New(context30(t), short, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context30(t), connector.EmbedRequest{Texts: []string{"a"}, Kind: connector.EmbedPassage}); !errs.IsCode(err, errs.CodeInternal) {
		t.Fatalf("short vector: %v", err)
	}
}
