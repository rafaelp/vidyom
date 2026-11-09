package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Usage is what LLM calls consumed, for the budget (D19, I216).
type Usage struct {
	Provider     string
	Class        config.EndpointClass
	Model        string
	InputTokens  int
	OutputTokens int
}

func (u *Usage) add(llm connector.LLM, response connector.ChatResponse) {
	u.Provider = llm.Name()
	u.Class = llm.Class()
	if response.Model != "" {
		u.Model = response.Model
	}
	u.InputTokens += response.InputTokens
	u.OutputTokens += response.OutputTokens
}

// IsExternal reports whether content sent to an endpoint leaves the host's trust
// boundary. Anything not exactly classified local counts as external (D8).
func IsExternal(llm connector.LLM) bool {
	return llm.Class() != config.EndpointLocal
}

// PolicyAllows applies D8's second check to an endpoint already chosen: local_only
// content may only use a local endpoint.
func PolicyAllows(processing domain.AIProcessing, llm connector.LLM) bool {
	return EffectivePolicy(processing, true) == domain.AIExternalAllowed || !IsExternal(llm)
}

// EffectivePolicy reads D8 fail closed: external_allowed needs the consent record, and
// any other value means local_only (I208).
func EffectivePolicy(processing domain.AIProcessing, hasConsent bool) domain.AIProcessing {
	if processing == domain.AIExternalAllowed && hasConsent {
		return domain.AIExternalAllowed
	}
	return domain.AILocalOnly
}

// ForEndpoint returns the transcript an endpoint may see: sensitive segments are gone
// unless it is local (D30, I210).
func ForEndpoint(llm connector.LLM, t domain.Transcript, marks []domain.SensitiveSegment) domain.Transcript {
	if IsExternal(llm) {
		return RemoveSensitive(t, marks)
	}
	return t
}

// ErrInvalidOutput means an LLM answered with something other than the JSON asked for.
var ErrInvalidOutput = errors.New("the LLM did not answer with the expected JSON")

// decodeLLMJSON reads the JSON object out of an LLM's answer, tolerating the code fences
// and the surrounding words many models add even in JSON mode.
func decodeLLMJSON(content string, into any) error {
	data := extractJSONObject(content)
	if data == nil {
		return ErrInvalidOutput
	}
	// The decoder's own error is dropped: it can quote the answer, which can quote the
	// transcript, and errors must not carry that.
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(into); err != nil {
		return ErrInvalidOutput
	}
	return nil
}

func extractJSONObject(content string) []byte {
	start := strings.IndexByte(content, '{')
	if start < 0 {
		return nil
	}
	depth := 0
	inString, escaped := false, false
	for i := start; i < len(content); i++ {
		c := content[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return []byte(content[start : i+1])
			}
		}
	}
	return nil
}

// completeJSON asks for a JSON object, once more if the first answer is not one.
func completeJSON(ctx context.Context, llm connector.LLM, system, user string, maxTokens int, usage *Usage, into any) error {
	request := connector.ChatRequest{
		Messages: []connector.ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0.1,
		MaxTokens:   maxTokens,
		JSONMode:    true,
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		response, err := llm.Complete(ctx, request)
		if err != nil {
			return err
		}
		usage.add(llm, response)
		if err := decodeLLMJSON(response.Content, into); err != nil {
			last = errs.Wrap(errs.CodeUnavailable, err, "the LLM's answer could not be read")
			continue
		}
		return nil
	}
	return last
}

// transcriptLines renders sentences as the timestamped lines prompts carry.
func transcriptLine(s Sentence) string {
	return fmt.Sprintf("[%.3f-%.3f] %s", s.Start, s.End, s.Text)
}

func capRunes(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}
