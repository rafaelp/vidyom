package ainodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/retrieval"
)

type exportNode struct{ base }

type exportParams struct {
	Transcript       domain.Transcript         `json:"transcript"`
	Formats          []string                  `json:"formats"`
	Sensitive        []domain.SensitiveSegment `json:"sensitive"`
	ExcludeSensitive bool                      `json:"excludeSensitive"`
}

type exportedFile struct {
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Execute writes open formats to scratch (§4.1). Marked speech is redacted, keeping its
// timing, or dropped with excludeSensitive for anything public (D30, I210). It takes
// its marks as a parameter, so a workflow exporting for the public must pass them.
func (n exportNode) Execute(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p exportParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	formats := p.Formats
	if len(formats) == 0 {
		formats = retrieval.ExportFormats
	}
	transcript := p.Transcript
	if p.ExcludeSensitive {
		transcript = retrieval.RemoveSensitive(transcript, p.Sensitive)
	} else {
		transcript = retrieval.RedactSensitive(transcript, p.Sensitive)
	}
	files := make(map[string]exportedFile, len(formats))
	for _, format := range formats {
		data, err := retrieval.RenderTranscript(transcript, format)
		if err != nil {
			return node.Result{}, node.Permanent(err)
		}
		name := "transcript." + format
		path, err := env.Scratch().Path(name)
		if err != nil {
			return node.Result{}, node.Permanent(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return node.Result{}, node.Transient(errs.Wrap(errs.CodeUnavailable, err, "cannot write to scratch"))
		}
		sum := sha256.Sum256(data)
		files[format] = exportedFile{File: env.Scratch().URI(name), SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}
	}
	result, err := node.JSONResult(map[string]any{"files": files})
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	return result, nil
}
