package mockllm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/connectors/ai/openai"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func client(t *testing.T, server *Server) *openai.LLM {
	t.Helper()
	http := httptest.NewServer(server)
	t.Cleanup(http.Close)
	llm, err := openai.New(config.LLMEndpoint{Name: "mock", BaseURL: http.URL + "/v1", Model: "mock", Class: config.EndpointLocal}, nil, openai.Options{Backoff: 1})
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

func ask(t *testing.T, llm *openai.LLM, system, user string) string {
	t.Helper()
	response, err := llm.Complete(context.Background(), connector.ChatRequest{
		Messages: []connector.ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		JSONMode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response.Content
}

const transcript = "Request: the woman who had no remorse\nTranscript:\n" +
	"[0.000-5.000] Welcome to the class.\n" +
	"[5.000-10.000] There was a woman of ninety.\n" +
	"[10.000-15.000] She had no remorse.\n" +
	"[15.000-20.000] Now lift the arms.\n"

func TestFindSegmentPicksThePassage(t *testing.T) {
	server := &Server{}
	content := ask(t, client(t, server), "Task: find_segment\nrules", transcript)
	var answer struct {
		Candidates []struct {
			Start     float64 `json:"start_seconds"`
			End       float64 `json:"end_seconds"`
			Rationale string  `json:"rationale"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(content), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Candidates) != 1 || answer.Candidates[0].Start != 5 || answer.Candidates[0].End != 15 {
		t.Fatalf("answer = %+v", answer)
	}
	if !strings.HasPrefix(answer.Candidates[0].Rationale, "[mock]") {
		t.Fatal("everything written is labeled")
	}
	requests := server.Requests()
	if len(requests) != 1 || requests[0].Task != "find_segment" || !requests[0].JSONMode || !strings.Contains(requests[0].Text(), "no remorse") {
		t.Fatalf("requests = %+v", requests)
	}
}

func TestFindSegmentWithNothingInCommon(t *testing.T) {
	content := ask(t, client(t, &Server{}), "Task: find_segment", "Request: quarterly invoice\nTranscript:\n[0.000-5.000] Welcome.\n")
	if content != `{"candidates":[]}` {
		t.Fatalf("content = %s", content)
	}
}

func TestAnalyzeAndEditorialAreLabeled(t *testing.T) {
	llm := client(t, &Server{})
	var analysis struct {
		Summary  string           `json:"summary"`
		Chapters []map[string]any `json:"chapters"`
		Tags     []string         `json:"tags"`
	}
	if err := json.Unmarshal([]byte(ask(t, llm, "Task: analyze", transcript)), &analysis); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(analysis.Summary, "[mock]") || len(analysis.Chapters) == 0 || len(analysis.Tags) == 0 {
		t.Fatalf("analysis = %+v", analysis)
	}
	var editorial struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(ask(t, llm, "Task: editorial", transcript)), &editorial); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(editorial.Title, "[mock]") {
		t.Fatalf("title = %q", editorial.Title)
	}
}

func TestFailureInjectionAndOverride(t *testing.T) {
	server := &Server{}
	server.FailNext(http.StatusBadRequest)
	llm := client(t, server)
	_, err := llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "x"}}})
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	server.Reply(func(Request) (string, bool) { return "custom", true })
	if got := ask(t, llm, "s", "u"); got != "custom" {
		t.Fatalf("got %q", got)
	}
}

func TestOnlyChatCompletionsIsServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Server{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
}
