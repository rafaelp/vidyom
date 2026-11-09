package connector

import (
	"context"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// TranscribeRequest asks for a transcript of a local audio file.
type TranscribeRequest struct {
	AudioPath      string
	Language       string // empty detects
	WordTimestamps bool
	Progress       func(percent float64, message string)
}

// Transcriber is a transcription connector: faster-whisper large-v3 first (D9).
type Transcriber interface {
	Info() Info
	Transcribe(ctx context.Context, req TranscribeRequest) (domain.Transcript, error)
}

// EmbedKind picks the model's prompt side.
type EmbedKind string

const (
	EmbedPassage EmbedKind = "passage"
	EmbedQuery   EmbedKind = "query"
)

// EmbedRequest asks for vectors.
type EmbedRequest struct {
	Texts []string
	Kind  EmbedKind
}

// EmbedResult carries one vector per text with the model that produced them.
// Model is "<name>@<version>", stored per vector (R18).
type EmbedResult struct {
	Model      string
	Dimensions int
	Vectors    [][]float32
}

// Embedder is an embedding connector: BGE-M3 first (D9).
type Embedder interface {
	Info() Info
	Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error)
}

// ChatMessage is one OpenAI-compatible message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is one completion request. JSONMode asks for a JSON object.
type ChatRequest struct {
	Messages    []ChatMessage
	Temperature float64
	MaxTokens   int
	JSONMode    bool
}

// ChatResponse is the completion. Tokens feed the usage record (D19).
type ChatResponse struct {
	Content      string
	Model        string
	InputTokens  int
	OutputTokens int
}

// LLM is one OpenAI-compatible endpoint (D9), classified for the external AI
// policy (D8). Callers must check Class against the asset's ai_processing before
// sending any content: local_only content may only use a local endpoint.
type LLM interface {
	Info() Info
	Name() string
	Class() config.EndpointClass
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// AIParams is what AI connector factories receive.
type AIParams struct {
	Tenant  domain.TenantID
	AI      config.AI
	Daemon  config.AIWorker
	Secrets SecretResolver
}

// TranscriberFactory opens a transcription connector.
type TranscriberFactory func(ctx context.Context, p AIParams) (Transcriber, error)

// EmbedderFactory opens an embedding connector.
type EmbedderFactory func(ctx context.Context, p AIParams) (Embedder, error)

// LLMFactory opens a connector for one endpoint.
type LLMFactory func(ctx context.Context, endpoint config.LLMEndpoint, secrets SecretResolver) (LLM, error)
