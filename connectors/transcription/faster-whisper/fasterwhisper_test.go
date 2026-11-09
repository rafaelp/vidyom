package fasterwhisper

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
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

func TestFakeWorkerIsLabeledAndMapped(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir)
	audio := filepath.Join(dir, "a.wav")
	workertest.WriteWAV(t, audio, 25)

	registry := connector.NewRegistry()
	Register(registry)
	factory, err := registry.Transcriber(Driver)
	if err != nil {
		t.Fatal(err)
	}
	transcriber, err := factory(context30(t), connector.AIParams{
		Daemon: config.AIWorker{Endpoint: worker.Endpoint(), Engine: aiworker.EngineFake},
		AI:     config.AI{TranscriptionModel: "large-v3"},
	})
	if err != nil {
		t.Fatal(err)
	}

	info := transcriber.Info()
	if !info.Capabilities["fake"] || !strings.Contains(info.Reason, "FAKE ENGINE") {
		t.Fatalf("the fake must be labeled: %+v", info)
	}
	if info.Capabilities["external"] {
		t.Fatal("a worker on this host is not external")
	}

	progress := 0
	transcript, err := transcriber.Transcribe(context30(t), connector.TranscribeRequest{
		AudioPath:      audio,
		WordTimestamps: true,
		Progress:       func(float64, string) { progress++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if transcript.TimestampBasis != "working_master" {
		t.Fatalf("basis = %q", transcript.TimestampBasis)
	}
	if !strings.HasPrefix(transcript.Engine, "fake/") {
		t.Fatalf("engine = %q", transcript.Engine)
	}
	if transcript.DurationSeconds != 25 || len(transcript.Segments) == 0 || len(transcript.Segments[0].Words) == 0 {
		t.Fatalf("transcript = %+v", transcript.Segments[:1])
	}
}

func TestEmptyAudioPath(t *testing.T) {
	transcriber := &Transcriber{}
	if _, err := transcriber.Transcribe(context.Background(), connector.TranscribeRequest{}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterDeclaresALocalDriver(t *testing.T) {
	registry := connector.NewRegistry()
	Register(registry)
	found := false
	for _, info := range registry.List() {
		if info.Driver == Driver && info.Family == connector.FamilyTranscription {
			found = true
			if info.Capabilities["external"] {
				t.Fatal("must not declare itself external")
			}
		}
	}
	if !found {
		t.Fatal("not registered")
	}
}

// A real-engine worker running another model than the tenant's is refused.
func TestRefusesAnUnexpectedModel(t *testing.T) {
	socket := filepath.Join(workertest.ShortTempDir(t), "stub.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			stream, err := listener.Accept()
			if err != nil {
				return
			}
			conn := jsonrpc.NewConn(stream, stream, stream, func(context.Context, string, json.RawMessage) (any, error) {
				return aiworker.InitializeResult{ProtocolVersion: 1, Engine: aiworker.EngineReal, TranscriptionModel: "faster-whisper/tiny"}, nil
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
	if _, err := New(context30(t), client, ""); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("default model: err = %v", err)
	}
	if _, err := New(context30(t), client, "tiny"); err != nil {
		t.Fatalf("configured model: err = %v", err)
	}
}
