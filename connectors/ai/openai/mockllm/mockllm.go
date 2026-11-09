// Package mockllm is a MOCK of an OpenAI-compatible endpoint, for tests and the demo.
// It understands nothing: it picks transcript lines by counting words shared with the
// request and labels everything it writes with "[mock]". It must never be configured
// for real content (I217).
package mockllm

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Request is one recorded call, kept so tests can assert what reached the endpoint.
type Request struct {
	Task          string
	Authorization string
	Messages      []Message
	JSONMode      bool
}

// Text returns everything the request carried.
func (r Request) Text() string {
	var parts []string
	for _, m := range r.Messages {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n")
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Server is the mock. The zero value is ready; use it as an http.Handler.
type Server struct {
	mu       sync.Mutex
	requests []Request
	fail     []int
	reply    func(Request) (string, bool)
}

// Requests returns what has been received.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// FailNext makes the next calls answer these HTTP statuses, in order.
func (s *Server) FailNext(statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, statuses...)
}

// Reply overrides answers: when f returns true, its string is the completion.
func (s *Server) Reply(f func(Request) (string, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = f
}

// ServeHTTP answers POST .../chat/completions.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	var wire struct {
		Messages       []Message `json:"messages"`
		ResponseFormat *struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	request := Request{
		Task:          taskOf(wire.Messages),
		Authorization: r.Header.Get("Authorization"),
		Messages:      wire.Messages,
		JSONMode:      wire.ResponseFormat != nil && wire.ResponseFormat.Type == "json_object",
	}

	s.mu.Lock()
	s.requests = append(s.requests, request)
	status := 0
	if len(s.fail) > 0 {
		status, s.fail = s.fail[0], s.fail[1:]
	}
	reply := s.reply
	s.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	content, overridden := "", false
	if reply != nil {
		content, overridden = reply(request)
	}
	if !overridden {
		content = answer(request)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   "mock-llm@1",
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": Message{Role: "assistant", Content: content}}},
		"usage":   map[string]any{"prompt_tokens": len(body) / 4, "completion_tokens": len(content) / 4},
	})
}

var taskLine = regexp.MustCompile(`(?m)^Task: (\S+)`)

func taskOf(messages []Message) string {
	for _, m := range messages {
		if match := taskLine.FindStringSubmatch(m.Content); match != nil {
			return match[1]
		}
	}
	return ""
}

func answer(r Request) string {
	user := ""
	for _, m := range r.Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}
	lines := parseLines(user)
	var value any
	switch r.Task {
	case "find_segment":
		value = findSegment(lines, requestOf(user))
	case "analyze":
		value = analyze(lines)
	case "condense_summaries":
		value = map[string]string{"summary": "[mock] Combined summary of the recording."}
	case "editorial":
		value = editorial(lines, requestOf(user), workingTitleOf(user), transcriptOf(user))
	default:
		value = map[string]string{"message": "[mock] unknown task"}
	}
	data, _ := json.Marshal(value)
	return string(data)
}

type line struct {
	start, end float64
	text       string
}

var linePattern = regexp.MustCompile(`(?m)^\[(\d+(?:\.\d+)?)-(\d+(?:\.\d+)?)\] (.*)$`)

func parseLines(text string) []line {
	var out []line
	for _, match := range linePattern.FindAllStringSubmatch(text, -1) {
		start, _ := strconv.ParseFloat(match[1], 64)
		end, _ := strconv.ParseFloat(match[2], 64)
		out = append(out, line{start, end, match[3]})
	}
	return out
}

var requestPattern = regexp.MustCompile(`(?m)^Request: (.*)$`)

func requestOf(text string) string {
	if match := requestPattern.FindStringSubmatch(text); match != nil {
		return match[1]
	}
	return ""
}

var workingTitlePattern = regexp.MustCompile(`(?m)^Working title: (.*)$`)

func workingTitleOf(text string) string {
	if match := workingTitlePattern.FindStringSubmatch(text); match != nil {
		return match[1]
	}
	return ""
}

func transcriptOf(text string) string {
	_, after, _ := strings.Cut(text, "Transcript:\n")
	return after
}

func wordsOf(text string) map[string]bool {
	out := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len([]rune(word)) > 3 {
			out[word] = true
		}
	}
	return out
}

func sharedWords(a, b map[string]bool) int {
	count := 0
	for word := range a {
		if b[word] {
			count++
		}
	}
	return count
}

func findSegment(lines []line, request string) map[string]any {
	wanted := wordsOf(request)
	best, bestScore := -1, 0
	for i, l := range lines {
		if score := sharedWords(wanted, wordsOf(l.text)); score > bestScore {
			best, bestScore = i, score
		}
	}
	candidates := []map[string]any{}
	if best >= 0 {
		// The passage around the best line: neighbors that also share a word.
		first, last := best, best
		for first > 0 && sharedWords(wanted, wordsOf(lines[first-1].text)) > 0 {
			first--
		}
		for last+1 < len(lines) && sharedWords(wanted, wordsOf(lines[last+1].text)) > 0 {
			last++
		}
		candidates = append(candidates, map[string]any{
			"start_seconds": lines[first].start,
			"end_seconds":   lines[last].end,
			"confidence":    0.85,
			"rationale":     "[mock] the passage sharing the most words with the request",
		})
	}
	return map[string]any{"candidates": candidates}
}

func analyze(lines []line) map[string]any {
	chapters := []map[string]any{}
	stories := []map[string]any{}
	if len(lines) > 0 {
		size := max(1, len(lines)/3)
		for start := 0; start < len(lines); start += size {
			end := min(start+size, len(lines)) - 1
			chapters = append(chapters, map[string]any{
				"title":         "[mock] Chapter starting at " + strconv.FormatFloat(lines[start].start, 'f', 0, 64),
				"start_seconds": lines[start].start,
				"end_seconds":   lines[end].end,
				"summary":       "[mock] " + lines[start].text,
			})
		}
	}
	return map[string]any{
		"summary":  "[mock] Summary of a recording with " + strconv.Itoa(len(lines)) + " sentences.",
		"chapters": chapters,
		"tags":     topWords(lines, 5),
		"stories":  stories,
	}
}

func topWords(lines []line, n int) []string {
	counts := map[string]int{}
	for _, l := range lines {
		for word := range wordsOf(l.text) {
			counts[word]++
		}
	}
	words := make([]string, 0, len(counts))
	for word := range counts {
		words = append(words, word)
	}
	sort.Slice(words, func(a, b int) bool {
		if counts[words[a]] != counts[words[b]] {
			return counts[words[a]] > counts[words[b]]
		}
		return words[a] < words[b]
	})
	if len(words) > n {
		words = words[:n]
	}
	return words
}

func editorial(lines []line, request, workingTitle, transcript string) map[string]any {
	title := request
	if title == "" {
		title = workingTitle
	}
	// The editorial prompt carries plain sentences, without the timestamps of the
	// other tasks.
	if len(lines) == 0 {
		lines = []line{{text: transcript}}
	}
	return map[string]any{
		"title":       "[mock] " + strings.TrimPrefix(title, "[mock] "),
		"description": "[mock] Draft description, for review.",
		"tags":        topWords(lines, 3),
	}
}
