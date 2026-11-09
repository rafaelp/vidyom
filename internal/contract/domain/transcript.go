package domain

import (
	"time"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

// Word is a timed word.
type Word struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// TranscriptSegment is a timed span of speech on the working master's timeline (D21).
type TranscriptSegment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Words   []Word  `json:"words,omitempty"`
	Speaker string  `json:"speaker,omitempty"`
}

// Transcript is the structured transcript of an asset version. Private by
// default (D30).
type Transcript struct {
	AssetID         id.Friendly         `json:"asset_id,omitempty"`
	Version         string              `json:"version,omitempty"`
	Language        string              `json:"language"`
	DurationSeconds float64             `json:"duration_seconds"`
	TimestampBasis  string              `json:"timestamp_basis"` // "working_master"
	Engine          string              `json:"engine"`          // "faster-whisper/large-v3"
	Segments        []TranscriptSegment `json:"segments"`
	CreatedAt       time.Time           `json:"created_at,omitzero"`
}

// Chapter is a titled interval with a summary.
type Chapter struct {
	Title        string  `json:"title"`
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	Summary      string  `json:"summary,omitempty"`
}

// Story is a candidate complete narrative found during analysis (§4.4).
type Story struct {
	Title        string  `json:"title"`
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	Summary      string  `json:"summary,omitempty"`
	Confidence   float64 `json:"confidence"`
}

// Analysis is the LLM output stored for an asset version. Available is false
// when no endpoint was allowed for the asset (D8, I28).
type Analysis struct {
	AssetID   id.Friendly `json:"asset_id,omitempty"`
	Version   string      `json:"version,omitempty"`
	Available bool        `json:"available"`
	Summary   string      `json:"summary,omitempty"`
	Chapters  []Chapter   `json:"chapters,omitempty"`
	Tags      []string    `json:"tags,omitempty"`
	Stories   []Story     `json:"stories,omitempty"`
	Model     string      `json:"model,omitempty"`
	Endpoint  string      `json:"endpoint_class,omitempty"` // "local" or "external"
}

// SensitiveSegment marks an interval excluded from public.db, public captions
// and external AI, and redacted in exports (D30).
type SensitiveSegment struct {
	AssetID      id.Friendly `json:"asset_id"`
	Version      string      `json:"version"`
	StartSeconds float64     `json:"start_seconds"`
	EndSeconds   float64     `json:"end_seconds"`
	Reason       string      `json:"reason,omitempty"`
	MarkedBy     string      `json:"marked_by,omitempty"`
	MarkedAt     time.Time   `json:"marked_at,omitzero"`
}

// ChunkLevel distinguishes speech windows from chapter summaries (D15).
type ChunkLevel string

const (
	ChunkWindow  ChunkLevel = "window"
	ChunkChapter ChunkLevel = "chapter"
)

// SearchChunk is an indexable span: 30-60 s windows at sentence boundaries with
// about 15% overlap, plus chapter summaries as a second level (D15).
type SearchChunk struct {
	Level        ChunkLevel `json:"level"`
	StartSeconds float64    `json:"start_seconds"`
	EndSeconds   float64    `json:"end_seconds"`
	Text         string     `json:"text"`
}

// EmbeddedChunk is a chunk with its vector. Every vector stores its model and
// segmentation strategy (R18); vectors of different models never mix.
type EmbeddedChunk struct {
	SearchChunk
	Vector []float32 `json:"vector"`
}

// SearchHit is one retrieval result.
type SearchHit struct {
	AssetID      id.Friendly `json:"asset_id"`
	Version      string      `json:"version"`
	Level        ChunkLevel  `json:"level"`
	StartSeconds float64     `json:"start_seconds"`
	EndSeconds   float64     `json:"end_seconds"`
	Text         string      `json:"text"`
	Score        float64     `json:"score"`
	Source       string      `json:"source"` // "lexical", "vector" or "hybrid"
}

// CandidateSegment is a proposed clip with its review evidence (D51).
type CandidateSegment struct {
	Segments   []Segment `json:"segments"`
	Excerpt    string    `json:"excerpt"`
	Confidence float64   `json:"confidence"`
	Rationale  string    `json:"rationale,omitempty"`
}
