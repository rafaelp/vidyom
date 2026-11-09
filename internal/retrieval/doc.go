// Package retrieval is the AI component's logic with no provider behind it: transcript
// chunking (D15), sensitive-segment redaction (D30), hybrid search with rank fusion
// (§4.4, I43), complete-story detection (D51), and the analysis and editorial prompts
// that go through the single LLM driver (D9).
//
// It depends only on contract packages, so it can link into vidyom-worker: nodes in
// pool ai use chunking and redaction from here.
package retrieval
