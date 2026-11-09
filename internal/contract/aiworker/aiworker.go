// Package aiworker is the JSON-RPC method set of the Python AI worker (D31, I6).
// schemas/ai-worker-rpc.v1.json defines the same methods for the Python side.
// Files travel by local path on a shared volume, never as bytes on the wire.
package aiworker

import (
	"math"
	"strings"
	"unicode"
)

// ProtocolVersion is this contract's version.
const ProtocolVersion = 1

// Methods. `initialize` must come first on a connection.
const (
	MethodInitialize = "initialize"
	MethodShutdown   = "shutdown"
	MethodHealth     = "health"
	MethodTranscribe = "transcribe"
	MethodEmbed      = "embed"
)

// MaxEmbedBatch is the most texts one embed call may carry; clients split batches.
const MaxEmbedBatch = 64

// Engine names (I27). There is no fallback between them.
const (
	EngineReal = "real"
	EngineFake = "fake"
)

// FakeEmbeddingModel is the model name of the fake engine (I27).
const FakeEmbeddingModel = "fake-hash-embed@1"

// FakeEmbeddingDimensions matches BGE-M3's dense vectors.
const FakeEmbeddingDimensions = 1024

// InitializeParams starts a session. Token is required on the tcp transport.
type InitializeParams struct {
	ProtocolVersion int    `json:"protocol_version"`
	Client          string `json:"client"`
	Token           string `json:"token,omitempty"`
}

// InitializeResult describes the worker.
type InitializeResult struct {
	ProtocolVersion    int      `json:"protocol_version"`
	Engine             string   `json:"engine"`
	TranscriptionModel string   `json:"transcription_model"`
	EmbeddingModel     string   `json:"embedding_model"`
	EmbeddingDims      int      `json:"embedding_dimensions"`
	Device             string   `json:"device"` // "cpu", "cuda", "metal"
	Capabilities       []string `json:"capabilities"`
}

// HealthResult reports readiness.
type HealthResult struct {
	Status       string   `json:"status"` // "ok" or "degraded"
	LoadedModels []string `json:"loaded_models,omitempty"`
	Detail       string   `json:"detail,omitempty"`
}

// TranscribeParams asks for a transcript. ProgressToken, when set, makes the
// server send $/progress notifications carrying it.
type TranscribeParams struct {
	AudioPath      string `json:"audio_path"`
	Language       string `json:"language,omitempty"`
	WordTimestamps bool   `json:"word_timestamps"`
	ProgressToken  string `json:"progress_token,omitempty"`
}

// Word is a timed word.
type Word struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Segment is a timed span of speech.
type Segment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Words   []Word  `json:"words,omitempty"`
	Speaker string  `json:"speaker,omitempty"`
}

// TranscribeResult is the transcript. Timestamps are on the audio's timeline,
// which is the working master's (D21, D56).
type TranscribeResult struct {
	Language        string    `json:"language"`
	DurationSeconds float64   `json:"duration_seconds"`
	Engine          string    `json:"engine"` // "faster-whisper/large-v3"
	Segments        []Segment `json:"segments"`
}

// EmbedParams asks for vectors. Kind is "passage" or "query".
type EmbedParams struct {
	Texts []string `json:"texts"`
	Kind  string   `json:"kind"`
}

// EmbedResult carries one vector per text. Model is "<name>@<version>" (R18).
type EmbedResult struct {
	Model      string      `json:"model"`
	Dimensions int         `json:"dimensions"`
	Vectors    [][]float32 `json:"vectors"`
}

// FakeHashEmbed is the fake engine's embedding (I27), the Go copy of the algorithm
// in schemas/ai-worker-rpc.v1.json, for tests and mocks. Lowercased Unicode word
// tokens are feature-hashed with 32-bit FNV-1a into FakeEmbeddingDimensions
// buckets; bit 31 of the hash gives the sign; the result is L2 normalized. It only
// finds shared words, so it tests the search path, not semantic quality.
func FakeHashEmbed(text string) []float32 {
	sums := make([]float64, FakeEmbeddingDimensions)
	token := make([]rune, 0, 32)
	flush := func() {
		if len(token) == 0 {
			return
		}
		hash := uint32(2166136261)
		for _, b := range []byte(string(token)) {
			hash ^= uint32(b)
			hash *= 16777619
		}
		sign := 1.0
		if hash&(1<<31) != 0 {
			sign = -1.0
		}
		sums[hash%FakeEmbeddingDimensions] += sign
		token = token[:0]
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			token = append(token, r)
			continue
		}
		flush()
	}
	flush()

	var norm float64
	for _, v := range sums {
		norm += v * v
	}
	out := make([]float32, FakeEmbeddingDimensions)
	if norm == 0 {
		return out
	}
	norm = math.Sqrt(norm)
	for i, v := range sums {
		out[i] = float32(v / norm)
	}
	return out
}
