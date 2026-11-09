// Package openai is the single OpenAI-compatible LLM driver (D9): one driver for
// vLLM, Ollama, OpenRouter, OpenAI and the like, each configured as an endpoint
// classified local or external (D8).
//
// The driver enforces what it can of D8 on its own (I209). A local-class endpoint can
// only connect to private or loopback addresses, an external one only over HTTPS, and
// no redirect is ever followed. Whether content may go to a given endpoint at all is
// the caller's check (connector.LLM), through config.AllowedEndpoints.
//
// Prompts and completions never reach logs or errors, because they hold transcript
// text. The API key is resolved from the vault at the moment of each request and
// stays in memory.
package openai
