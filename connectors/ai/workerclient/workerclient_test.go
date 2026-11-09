package workerclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/ai/workerclient/workertest"
	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jsonrpc"
	"github.com/rafaelp/vidyom/schemas"
)

func fakeWorkerClient(t *testing.T, extraEnv ...string) (*Client, *workertest.Worker, string) {
	t.Helper()
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir, extraEnv...)
	client, err := FromConfig(config.AIWorker{Endpoint: worker.Endpoint(), Engine: aiworker.EngineFake})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	audio := filepath.Join(dir, "audio.wav")
	workertest.WriteWAV(t, audio, 12)
	return client, worker, audio
}

func withTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestDescribeHealthTranscribeEmbedOverUnixSocket(t *testing.T) {
	client, _, audio := fakeWorkerClient(t)
	ctx := withTimeout(t, 30*time.Second)

	described, err := client.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if described.Engine != aiworker.EngineFake || described.EmbeddingModel != aiworker.FakeEmbeddingModel || described.EmbeddingDims != aiworker.FakeEmbeddingDimensions {
		t.Fatalf("described = %+v", described)
	}

	health, err := client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || !strings.Contains(health.Detail, "FAKE ENGINE") {
		t.Fatalf("health = %+v", health)
	}

	var percents atomic.Int32
	transcript, err := client.Transcribe(ctx, aiworker.TranscribeParams{AudioPath: audio, WordTimestamps: true}, func(float64, string) { percents.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(transcript.Engine, "fake/") {
		t.Fatalf("engine = %q, the fake must be labeled", transcript.Engine)
	}
	if transcript.DurationSeconds != 12 || len(transcript.Segments) == 0 || len(transcript.Segments[0].Words) == 0 {
		t.Fatalf("transcript = %+v", transcript)
	}
	deadline := time.Now().Add(2 * time.Second)
	for percents.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if percents.Load() == 0 {
		t.Fatal("no progress received")
	}

	vectors, err := client.Embed(ctx, aiworker.EmbedParams{Texts: []string{"conscious breathing"}, Kind: "query"})
	if err != nil {
		t.Fatal(err)
	}
	if vectors.Model != aiworker.FakeEmbeddingModel || len(vectors.Vectors) != 1 || len(vectors.Vectors[0]) != vectors.Dimensions {
		t.Fatalf("vectors: model %q, %d", vectors.Model, len(vectors.Vectors))
	}
}

// The results the Python side really sends must validate against the schema.
func TestPythonResultsMatchTheSchema(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir)
	stream, err := net.Dial("unix", worker.Socket)
	if err != nil {
		t.Fatal(err)
	}
	conn := jsonrpc.NewConn(stream, stream, stream, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })
	go func() { _ = conn.Run(context.Background()) }()
	t.Cleanup(func() { _ = conn.Close() })
	ctx := withTimeout(t, 30*time.Second)

	audio := filepath.Join(dir, "audio.wav")
	workertest.WriteWAV(t, audio, 3)

	calls := []struct {
		method, def string
		params      any
	}{
		{aiworker.MethodInitialize, "initializeResult", aiworker.InitializeParams{ProtocolVersion: 1, Client: "test"}},
		{aiworker.MethodHealth, "healthResult", struct{}{}},
		{aiworker.MethodTranscribe, "transcribeResult", aiworker.TranscribeParams{AudioPath: audio, WordTimestamps: true}},
		{aiworker.MethodEmbed, "embedResult", aiworker.EmbedParams{Texts: []string{"hello"}, Kind: "passage"}},
	}
	for _, call := range calls {
		var raw json.RawMessage
		if err := conn.Call(ctx, call.method, call.params, &raw); err != nil {
			t.Fatalf("%s: %v", call.method, err)
		}
		if err := schemas.Validate(schemas.AIWorkerRPC+"#/$defs/"+call.def, raw); err != nil {
			t.Errorf("%s: result does not match the schema: %v\n%s", call.method, err, firstBytes(raw))
		}
	}
}

func firstBytes(b []byte) string {
	if len(b) > 400 {
		return string(b[:400]) + "..."
	}
	return string(b)
}

// Python's fake-hash-embed@1 must agree with Go's over Unicode, because Go, Python and
// the library's TypeScript embed queries and passages interchangeably (I27, I203).
func TestFakeEmbedAgreesWithGo(t *testing.T) {
	client, _, _ := fakeWorkerClient(t)
	ctx := withTimeout(t, 30*time.Second)
	corpus := []string{
		"Conscious breathing",
		"Respiração consciente, breathing breathing!",
		"İstanbul İİ ı I",
		"ΣΑΣ σας ΟΔΟΣ",
		"½ ٣ 呼吸 Ⅷ ² ०१२",
		"Straße ẞ ß",
		"Ǆ ǅ ǆ ﬁ ﬀ",
		"é é combininǵmarks",
		"👍🏽 emoji-text 100%",
		"",
		"!!!",
		"a\tb\nc d",
	}
	result, err := client.Embed(ctx, aiworker.EmbedParams{Texts: corpus, Kind: "passage"})
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range corpus {
		want := aiworker.FakeHashEmbed(text)
		got := result.Vectors[i]
		if len(got) != len(want) {
			t.Fatalf("%q: %d dimensions", text, len(got))
		}
		for j := range want {
			if math.Abs(float64(got[j]-want[j])) > 1e-6 {
				t.Errorf("%q: bucket %d: python %v, go %v", text, j, got[j], want[j])
				break
			}
		}
	}
}

func TestCancelStopsTheWorker(t *testing.T) {
	client, _, audio := fakeWorkerClient(t, "VIDYOM_AI_FAKE_DELAY_SECONDS=60")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Transcribe(ctx, aiworker.TranscribeParams{AudioPath: audio}, nil)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not return")
	}
	// The worker must stay usable.
	if _, err := client.Health(withTimeout(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidPathIsPermanent(t *testing.T) {
	client, _, _ := fakeWorkerClient(t)
	_, err := client.Transcribe(withTimeout(t, 20*time.Second), aiworker.TranscribeParams{AudioPath: "/nonexistent/audio.opus"}, nil)
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestReconnectsAfterTheWorkerRestarts(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir)
	client, err := FromConfig(config.AIWorker{Endpoint: worker.Endpoint(), Engine: aiworker.EngineFake})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := withTimeout(t, 40*time.Second)
	if _, err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	worker.Stop()

	if _, err := client.Health(ctx); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("with the worker down: %v", err)
	}

	workertest.Start(t, dir)
	if _, err := client.Health(ctx); err != nil {
		t.Fatalf("after the restart: %v", err)
	}
}

func TestUnreachableWorkerIsUnavailable(t *testing.T) {
	client, err := FromConfig(config.AIWorker{Endpoint: "unix://" + filepath.Join(t.TempDir(), "none.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Health(context.Background()); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// I201: a worker running the fake is refused unless configuration asks for it.
func TestRefusesTheFakeUnlessConfigured(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir)
	for _, engine := range []string{"", aiworker.EngineReal} {
		client, err := FromConfig(config.AIWorker{Endpoint: worker.Endpoint(), Engine: engine})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Describe(withTimeout(t, 20*time.Second))
		if !errs.IsCode(err, errs.CodeFailedPrecondition) || !strings.Contains(err.Error(), "no fallback") {
			t.Fatalf("engine %q: err = %v", engine, err)
		}
	}
}

// I201, the other direction, with a stub worker that claims to run the real engine.
func TestRefusesTheRealEngineWhenConfiguredForFake(t *testing.T) {
	socket := filepath.Join(workertest.ShortTempDir(t), "stub.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go serveStub(listener, aiworker.InitializeResult{ProtocolVersion: 1, Engine: aiworker.EngineReal, Capabilities: []string{}})

	client, err := FromConfig(config.AIWorker{Endpoint: "unix://" + socket, Engine: aiworker.EngineFake})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Describe(withTimeout(t, 10*time.Second))
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusesAnotherProtocolVersion(t *testing.T) {
	socket := filepath.Join(workertest.ShortTempDir(t), "stub.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go serveStub(listener, aiworker.InitializeResult{ProtocolVersion: 99, Engine: aiworker.EngineReal})
	client := New(mustDialer(t, "unix://"+socket), Options{})
	if _, err := client.Describe(withTimeout(t, 10*time.Second)); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

func mustDialer(t *testing.T, endpoint string) Dialer {
	t.Helper()
	dial, err := DialerFor(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return dial
}

func serveStub(listener net.Listener, info aiworker.InitializeResult) {
	for {
		stream, err := listener.Accept()
		if err != nil {
			return
		}
		conn := jsonrpc.NewConn(stream, stream, stream, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
			if method == aiworker.MethodInitialize {
				return info, nil
			}
			return struct{}{}, nil
		})
		go func() { _ = conn.Run(context.Background()) }()
	}
}

func TestTCPTransportWithToken(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("fictional-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := workertest.Command(t, []string{"--engine", "fake", "--tcp", "127.0.0.1:0", "--token-file", tokenFile})
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	address := readListeningAddress(t, stderr)

	right, err := FromConfig(config.AIWorker{Endpoint: "tcp://" + address, TokenFile: tokenFile, Engine: aiworker.EngineFake})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = right.Close() })
	if _, err := right.Health(withTimeout(t, 20*time.Second)); err != nil {
		t.Fatal(err)
	}

	wrongFile := filepath.Join(dir, "wrong")
	if err := os.WriteFile(wrongFile, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong, err := FromConfig(config.AIWorker{Endpoint: "tcp://" + address, TokenFile: wrongFile, Engine: aiworker.EngineFake})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrong.Health(withTimeout(t, 20*time.Second))
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("wrong token: %v", err)
	}
	if strings.Contains(err.Error(), "other") {
		t.Fatal("the token leaked into the error")
	}
}

func readListeningAddress(t *testing.T, stderr io.Reader) string {
	t.Helper()
	buffer := make([]byte, 4096)
	var seen strings.Builder
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		n, err := stderr.Read(buffer)
		seen.Write(buffer[:n])
		if _, rest, ok := strings.Cut(seen.String(), "listening on "); ok {
			if line, _, complete := strings.Cut(rest, "\n"); complete {
				go func() { _, _ = io.Copy(io.Discard, stderr) }()
				return strings.TrimSpace(line)
			}
		}
		if err != nil {
			t.Fatalf("worker stderr ended: %v\n%s", err, seen.String())
		}
	}
	t.Fatal("the worker did not start listening")
	return ""
}

func TestDialerForRejects(t *testing.T) {
	for _, endpoint := range []string{
		"",
		"http://127.0.0.1:1",
		"unix://",
		"tcp://0.0.0.0:7000",
		"tcp://192.0.2.1:7000",
		"tcp://media.example.internal:7000",
		"tcp://127.0.0.1",
	} {
		if _, err := DialerFor(endpoint); err == nil {
			t.Errorf("%q must be refused", endpoint)
		}
	}
	for _, endpoint := range []string{"unix:///run/vidyom/ai.sock", "tcp://127.0.0.1:7077", "tcp://[::1]:7077"} {
		if _, err := DialerFor(endpoint); err != nil {
			t.Errorf("%q: %v", endpoint, err)
		}
	}
}

func TestFromConfigRejectsUnknownEngineAndMissingToken(t *testing.T) {
	if _, err := FromConfig(config.AIWorker{Endpoint: "unix:///run/a.sock", Engine: "tiny"}); err == nil {
		t.Fatal("unknown engine accepted")
	}
	_, err := FromConfig(config.AIWorker{Endpoint: "tcp://127.0.0.1:1", TokenFile: "/nonexistent/token"})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

func TestSharedReturnsOneClientPerConfiguration(t *testing.T) {
	cfg := config.AIWorker{Endpoint: "unix:///run/vidyom-test-shared.sock"}
	a, err := Shared(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Shared(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("same configuration must share one client")
	}
	c, err := Shared(config.AIWorker{Endpoint: cfg.Endpoint, Engine: aiworker.EngineFake})
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatal("another accepted engine must not share a client")
	}
}
