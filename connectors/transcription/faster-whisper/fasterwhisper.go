package fasterwhisper

import (
	"context"

	"github.com/rafaelp/vidyom/connectors/ai/workerclient"
	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is the connector's registered name.
const Driver = "faster-whisper"

const connectorVersion = "1"

// DefaultModel is the transcription model of D9.
const DefaultModel = "large-v3"

// Register adds the connector.
func Register(r connector.Registrar) {
	r.RegisterTranscriber(baseInfo(), Open)
}

func baseInfo() connector.Info {
	return connector.Info{
		Family:  connector.FamilyTranscription,
		Driver:  Driver,
		Version: connectorVersion,
		// Availability of the worker is only known once connected; Info on an opened
		// connector reports it.
		Available: true,
		Capabilities: map[string]bool{
			"word_timestamps": true,
			"external":        false,
		},
		Limits: map[string]any{"model": DefaultModel},
	}
}

// Open is the connector's factory.
func Open(ctx context.Context, p connector.AIParams) (connector.Transcriber, error) {
	client, err := workerclient.Shared(p.Daemon)
	if err != nil {
		return nil, err
	}
	return New(ctx, client, p.AI.TranscriptionModel)
}

// New opens a transcriber over a client. The worker must run wantModel unless it is
// the fake engine, because a transcript must never come from an unexpected model.
func New(ctx context.Context, client *workerclient.Client, wantModel string) (connector.Transcriber, error) {
	described, err := client.Describe(ctx)
	if err != nil {
		return nil, err
	}
	if wantModel == "" {
		wantModel = DefaultModel
	}
	if described.Engine != aiworker.EngineFake && described.TranscriptionModel != Driver+"/"+wantModel {
		return nil, errs.Newf(errs.CodeFailedPrecondition,
			"the AI worker runs transcription model %q, but this tenant is configured for %q", described.TranscriptionModel, wantModel)
	}
	return &Transcriber{client: client, described: described}, nil
}

// Transcriber implements connector.Transcriber.
type Transcriber struct {
	client    *workerclient.Client
	described aiworker.InitializeResult
}

var _ connector.Transcriber = (*Transcriber)(nil)

// Info reports the engine in use, and says so plainly when it is the fake.
func (t *Transcriber) Info() connector.Info {
	info := baseInfo()
	info.Limits = map[string]any{"model": t.described.TranscriptionModel, "device": t.described.Device}
	if t.described.Engine == aiworker.EngineFake {
		info.Capabilities["fake"] = true
		info.Reason = "FAKE ENGINE: the AI worker returns canned text, not a transcription (I27)"
	}
	return info
}

// Transcribe sends the local audio path to the worker. The file must be readable by
// the worker: both mount the shared volume (I5).
func (t *Transcriber) Transcribe(ctx context.Context, req connector.TranscribeRequest) (domain.Transcript, error) {
	if req.AudioPath == "" {
		return domain.Transcript{}, errs.New(errs.CodeInvalidArgument, "no audio path")
	}
	result, err := t.client.Transcribe(ctx, aiworker.TranscribeParams{
		AudioPath:      req.AudioPath,
		Language:       req.Language,
		WordTimestamps: req.WordTimestamps,
	}, req.Progress)
	if err != nil {
		return domain.Transcript{}, err
	}
	out := domain.Transcript{
		Language:        result.Language,
		DurationSeconds: result.DurationSeconds,
		TimestampBasis:  "working_master",
		Engine:          result.Engine,
		Segments:        make([]domain.TranscriptSegment, len(result.Segments)),
	}
	for i, s := range result.Segments {
		segment := domain.TranscriptSegment{Start: s.Start, End: s.End, Text: s.Text, Speaker: s.Speaker}
		if len(s.Words) > 0 {
			segment.Words = make([]domain.Word, len(s.Words))
			for j, w := range s.Words {
				segment.Words[j] = domain.Word{Start: w.Start, End: w.End, Text: w.Text}
			}
		}
		out.Segments[i] = segment
	}
	return out, nil
}
