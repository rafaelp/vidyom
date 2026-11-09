package bgem3

import (
	"context"
	"strings"

	"github.com/rafaelp/vidyom/connectors/ai/workerclient"
	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is the connector's registered name.
const Driver = "bge-m3"

const connectorVersion = "1"

// DefaultModel is the embedding model family of D9.
const DefaultModel = "bge-m3"

// Register adds the connector.
func Register(r connector.Registrar) {
	r.RegisterEmbedder(baseInfo(), Open)
}

func baseInfo() connector.Info {
	return connector.Info{
		Family:  connector.FamilyEmbedding,
		Driver:  Driver,
		Version: connectorVersion,
		// Availability of the worker is only known once connected; Info on an opened
		// connector reports it.
		Available:    true,
		Capabilities: map[string]bool{"external": false},
		Limits:       map[string]any{"model": DefaultModel, "batch": aiworker.MaxEmbedBatch},
	}
}

// Open is the connector's factory.
func Open(ctx context.Context, p connector.AIParams) (connector.Embedder, error) {
	client, err := workerclient.Shared(p.Daemon)
	if err != nil {
		return nil, err
	}
	return New(ctx, client, p.AI.EmbeddingModel)
}

// New opens an embedder over a client. The worker must run wantModel unless it is
// the fake engine. wantModel may omit the version: "bge-m3" accepts "bge-m3@1".
func New(ctx context.Context, client *workerclient.Client, wantModel string) (connector.Embedder, error) {
	described, err := client.Describe(ctx)
	if err != nil {
		return nil, err
	}
	if wantModel == "" {
		wantModel = DefaultModel
	}
	if described.Engine != aiworker.EngineFake &&
		described.EmbeddingModel != wantModel && !strings.HasPrefix(described.EmbeddingModel, wantModel+"@") {
		return nil, errs.Newf(errs.CodeFailedPrecondition,
			"the AI worker runs embedding model %q, but this tenant is configured for %q", described.EmbeddingModel, wantModel)
	}
	return &Embedder{client: client, described: described}, nil
}

// Embedder implements connector.Embedder.
type Embedder struct {
	client    *workerclient.Client
	described aiworker.InitializeResult
}

var _ connector.Embedder = (*Embedder)(nil)

// Info reports the model in use, and says so plainly when it is the fake.
func (e *Embedder) Info() connector.Info {
	info := baseInfo()
	info.Limits = map[string]any{
		"model":      e.described.EmbeddingModel,
		"dimensions": e.described.EmbeddingDims,
		"batch":      aiworker.MaxEmbedBatch,
		"device":     e.described.Device,
	}
	if e.described.Engine == aiworker.EngineFake {
		info.Capabilities["fake"] = true
		info.Reason = "FAKE ENGINE: embeddings are feature hashes that only match shared words (I27)"
	}
	return info
}

// Embed splits texts into batches of aiworker.MaxEmbedBatch and checks that every
// batch came from one model with the expected shape.
func (e *Embedder) Embed(ctx context.Context, req connector.EmbedRequest) (connector.EmbedResult, error) {
	if len(req.Texts) == 0 {
		return connector.EmbedResult{}, errs.New(errs.CodeInvalidArgument, "no texts to embed")
	}
	if req.Kind != connector.EmbedPassage && req.Kind != connector.EmbedQuery {
		return connector.EmbedResult{}, errs.Newf(errs.CodeInvalidArgument, "unknown embedding kind %q", req.Kind)
	}
	out := connector.EmbedResult{Vectors: make([][]float32, 0, len(req.Texts))}
	for start := 0; start < len(req.Texts); start += aiworker.MaxEmbedBatch {
		end := min(start+aiworker.MaxEmbedBatch, len(req.Texts))
		batch := req.Texts[start:end]
		result, err := e.client.Embed(ctx, aiworker.EmbedParams{Texts: batch, Kind: string(req.Kind)})
		if err != nil {
			return connector.EmbedResult{}, err
		}
		if len(result.Vectors) != len(batch) {
			return connector.EmbedResult{}, errs.Newf(errs.CodeInternal, "the AI worker returned %d vectors for %d texts", len(result.Vectors), len(batch))
		}
		if out.Model == "" {
			out.Model, out.Dimensions = result.Model, result.Dimensions
		} else if out.Model != result.Model || out.Dimensions != result.Dimensions {
			return connector.EmbedResult{}, errs.Newf(errs.CodeFailedPrecondition, "the AI worker changed embedding model mid-request: %q then %q", out.Model, result.Model)
		}
		for _, vector := range result.Vectors {
			if len(vector) != result.Dimensions {
				return connector.EmbedResult{}, errs.Newf(errs.CodeInternal, "the AI worker returned a vector of %d dimensions, expected %d", len(vector), result.Dimensions)
			}
			out.Vectors = append(out.Vectors, vector)
		}
	}
	return out, nil
}
