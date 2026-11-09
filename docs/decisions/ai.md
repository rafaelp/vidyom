# Implementation decisions: `ai`

Component decisions for the first executable version, numbered `I200`–`I219` per section 8 of the [master plan](../plans/00-master-plan.md). They fill gaps; they never change the maintainer's `Dn` decisions or the shared `I` log in [`docs/decisions.md`](../decisions.md). What a decision says the code does is a **target** until `ai` commits it. **Validate** marks a working default that rests on a §17.2 validation still unproven.

## Python worker

### I200. Package layout and real-engine libraries

- **Choice.** `aiworker/vidyom/` is the import package, name `vidyom` on PyPI (D28), started with `python -m vidyom` or the `vidyom-ai-worker` script. `--engine` and one transport flag are required: there is no default engine and no default socket. Extras: `asr` is `faster-whisper`; `embeddings` is `sentence-transformers`, which loads BGE-M3's dense head; `all` is both. Embedding model name `bge-m3@1`. `kind` does not change BGE-M3's input, since the model takes no query or passage prefix. Sparse and ColBERT heads are not used. The worker loads each model lazily, once, and serializes inference per model, so a query embedding never waits for a transcription.
- **Why.** I37 keeps the core stdlib-only. `sentence-transformers` is the smaller maintained path to the dense vectors that D5's sqlite-vec index stores; `FlagEmbedding` adds sparse and multi-vector output nothing here uses.
- **Revisit if.** Sparse retrieval is wanted, or a smaller ONNX runtime is preferred. **Validate:** both engines on the Docker image (I37); accuracy (D9).

### I201. The client refuses an unexpected engine

- **Choice.** Additive `config.AIWorker.Engine`: `real` when empty, or `fake`. `workerclient` compares it with what `initialize` reports and fails with `failed_precondition` on a mismatch, so a `fake` worker left running can never fill a production catalog with canned text, and a configuration asking for `fake` never silently runs a real model. The demo and E2E configuration must say `"engine": "fake"`.
- **Why.** I27 forbids fallbacks in either direction, but the engine is chosen on the Python side, out of the daemon's sight. Fail closed at the client.
- **Revisit if.** Engines become per-request.

### I202. Labeling and duration of the fake engine

- **Choice.** The fake is labeled at every surface a person or a client reads: a stderr banner at startup, `engine: "fake"` and capability `fake` in `initialize`, `detail` in `health`, `engine: "fake/canned@1"` or `"fake/fixture@1"` on every transcript, a first canned segment saying the text is not a transcription, and `connector.Info.Reason` in Go. The reported language is `en`, the canned text's actual language, whatever was requested. Audio duration comes from the stdlib for WAV and Ogg, then `ffprobe` when on `PATH`; with no way to read the duration, `transcribe` fails with `-32001` rather than inventing a timeline. A fixture's timestamps are used as written. `VIDYOM_AI_FAKE_DELAY_SECONDS` stretches the run for cancellation tests.
- **Why.** I27 asks for a canned transcript "scaled to the audio's duration" and the maintainer asked that nobody mistake it for real transcription.
- **Revisit if.** A tiny real model is usable in CI (I27).

### I203. `fake-hash-embed@1` lowercases rune by rune

- **Choice.** Python lowercases per code point and keeps the first character of a multi-character result, which matches Go's `strings.ToLower` on `İ` (Go gives `i`, Python's `str.lower` gives `i` plus a combining dot) and on word-final `Σ` (Go gives `σ`, Python's `str.lower` gives `ς`). Tokens are runs of Unicode categories `L*` and `N*`. A Go test compares both implementations over a Unicode corpus. Test vectors for the two cases are added to the schema so `library` can match them; JavaScript's `toLowerCase` has the same two differences as Python's `str.lower`.
- **Why.** The schema says "lowercase the text" and three implementations must agree. Go's behavior is the reference because it is what the contract package ships.
- **Revisit if.** Never for this model name; a change means a new model version (R18).

## Chunking

### I204. Chunking details under D15

- **Choice.** Strategy name `sentence-window-30-60s-overlap15@1`, stored per vector.
  - Sentences end at `.`, `!`, `?`, `…` and their CJK and Arabic forms followed by a space or the end of text. Timing comes from word timestamps; without them, proportional to characters inside the segment.
  - A window grows sentence by sentence until it reaches 45 s, the middle of the range, or until the next sentence would pass 60 s.
  - The next window starts at the sentence whose start is closest to 15% of the closed window's span before its end, always at least one sentence further on, so chunking terminates.
  - A sentence over 60 s, common in unpunctuated ASR output, is split at word boundaries, or proportionally when there are no words, into pieces of up to 45 s.
  - A tail under 30 s starts earlier when earlier sentences exist, so it reaches 30 s. A recording under 30 s gets one short window. A silence that makes 30 s impossible yields a short window: spans never pass 60 s.
  - A chapter with a summary becomes one `chapter`-level chunk, `"<title>. <summary>"`, spanning the chapter.
- **Why.** D15 fixes the numbers but not the edge cases. Overlap is measured in time, not sentences, because sentence length varies. Sentence boundaries always win over the exact 15%.
- **Revisit if.** Retrieval quality on real content (Validate, D9) calls for other sizes: a new strategy name, then a reindex.

## Story detection and review evidence

### I205. Confidence and the ambiguity rule (D51)

- **Choice.** `ambiguous` is true when any of these holds:
  1. no LLM was used for the result (I28);
  2. the top candidate's confidence is under `0.6`;
  3. another candidate with no substantial overlap (IoU at most 0.5) is within `0.15` of the top.

  With an LLM, a candidate's confidence is what the LLM stated, clamped to 0–1, and capped at `0.5` when the interval overlaps no retrieved hit, so unsupported output cannot look certain. Without one, it is the normalized fused score, where rank 1 in both lists gives 1, capped at `0.59`, because retrieval alone never establishes a complete story. Excerpts always come from the transcript, never from LLM text, up to 1000 characters, with sensitive text redacted. At most 5 candidates are returned.
- **Why.** D51 gives the rule but not the numbers. Retrieval always returns several hits, so "several candidates" has to mean competing ones, or every clip would be ambiguous. The editor approves in every case (D53), so these numbers change only what the editor is warned about.
- **Revisit if.** Editors report false certainty or too many warnings on real content.

### I206. A failing LLM degrades; it does not block ingest

- **Choice.** The driver retries 429, 5xx and connection errors up to 3 times with backoff. After that, or on invalid output after one more try, `ai.analyze` returns `available: false` with additive `unavailable_reason` (`no_endpoint_allowed`, `endpoint_failed` or `no_speech`), `ai.find_segment` returns retrieval candidates marked ambiguous and `ai.editorial` returns its fallback. Cancellation still propagates as cancellation. The failure is logged without content.
- **Why.** The consolidated condition of §3.1 (R11) requires no analysis, so an LLM outage must not hold back `ready_for_clipping`. I28 states the fallback for a disallowed endpoint, and an unreachable one leaves the editor in the same place. `reindex-asset` regenerates the analysis later.
- **Revisit if.** Operators want ingest to wait for analysis.

### I211. Nothing retrieved fails the node rather than cutting at random

- **Choice.** When retrieval yields no hit at all, `ai.find_segment` fails permanently with `failed_precondition`: no segment matches the prompt. There is no "whole recording" candidate.
- **Why.** §3.2 step 5: "do not cut at random". An empty `segments` would only fail later in `video.preview`, with a worse message.
- **Revisit if.** Editors want to pick an interval by hand with no proposal; that needs an approval path with no evidence, which D51 does not describe.

## External AI policy

### I208. The effective policy is read from the store

- **Choice.** `ai.analyze`, `ai.find_segment` and `ai.editorial` read the asset from the tenant store, never trusting parameters. The effective policy is `external_allowed` only when the stored `ai_processing` says so **and** a consent record is present. Anything else, including unknown values, is `local_only`. They pass that to `env.LLM`, then check `Class()` again: on `local_only`, an endpoint not exactly `local` is treated as absent and nothing is sent. `ai.transcribe`, in pool `ai` with no store, uses its `aiProcessing` parameter under the same fail-closed reading. The stored column already is the effective answer, collection included, with an asset-level decision winning (`core`'s I104), so this component does not look at the collection.
- **Why.** D8 asks for a consent record, and a missing one means the authorization cannot be shown, so it counts as not given. The second check keeps D8 from depending on one implementation of `env.LLM`.
- **Revisit if.** `core`'s I104 changes where the effective policy lives.

### I209. Endpoint hardening in the OpenAI-compatible driver

- **Choice.**
  - A `local`-class endpoint may connect only to loopback, RFC 1918, RFC 4193, link-local or CGNAT addresses, checked on the resolved address at dial time, so a misclassified public vendor fails closed and DNS cannot move it.
  - An `external` endpoint must use HTTPS. Credentials are refused over plain HTTP unless the host is loopback.
  - Redirects are never followed, because a redirect could carry content to a host the policy never saw.
  - An unknown class, userinfo in the URL or a missing model fails at the factory.
  - Provider response bodies never enter error messages, since they can echo the prompt.
- **Why.** The class is typed by a person and all of D8 rests on it. A vendor classified `local` would send a `local_only` transcript out with no consent.
- **Revisit if.** A local endpoint really lives on a public address; it is then classified `external`, which is honest, and needs `external_allowed`.

### I210. What redaction means in each path

- **Choice.** One function, two outcomes.
  - For LLM input, overlapping words are removed, or the whole segment when it has no word timings (fail closed). It runs only when the endpoint is not `local`, as D30 scopes it to external AI.
  - For `ai.export_transcript`, marked text is replaced by `[redacted]` with its timing kept, which is D30's "redacted in exports". `excludeSensitive` drops the spans instead, for anything public.
  - Review excerpts are redacted for every endpoint class, because approvals travel further than the transcript.
- **Why.** D30 lists different treatments per destination.
- **Revisit if.** Local endpoints turn out to be shared across trust boundaries.

### I207. Sensitive marks follow a clip through its lineage

- **Choice.** When `ai.index` indexes a derivative, it maps every sensitive mark of the source version onto the derivative's timeline, using `asset.Lineage.Segments`, and stores them with `MarkSensitive`, in the same transaction. Marks use reason `inherited from <source>`.
- **Why.** D30 excludes marked segments from `public.db` and captions, and `publishing` reads the clip's own marks. Without this, cutting a sensitive interval into a clip would launder it. `ai.index` is the one node every derivative passes that has the lineage, and it needs no catalog change.
- **Limit, stated plainly.** A mark added to a source **after** a clip was derived does not reach that clip. Neither `service.Assets.AddSensitive` nor this node handles it. Flagged for `core` and `publishing`.
- **Revisit if.** Marks should be evaluated live through the lineage at export time.

### I215. An `external` capability marks a non-local engine

- **Choice.** A `Transcriber` or `Embedder` whose `Info().Capabilities["external"]` is true sends content off the host. `ai.transcribe` refuses one unless the policy is `external_allowed`. `ai.embed` and query embeddings refuse one outright, because no asset policy reaches that path. Both connectors shipped here declare `external: false`.
- **Why.** D8 covers transcription and embeddings too, and future engines (§7.5) must not need another contract change. `connector.Info` already carries capabilities.
- **Revisit if.** An external embedder is wanted, which needs a policy parameter on `ai.embed`.

## Search and indexing

### I212. Search details under I43

- **Choice.**
  - Retrieval passes user text as it is, cut to its first 32 words. `core` tokenizes and quotes it, so it cannot be FTS5 syntax (`core`'s I106).
  - Limits default to 20, at most 100. Each list fetches `3 × limit` before fusion.
  - Hybrid degrades to lexical, reporting `mode: "lexical"`, when no embedder is usable or its model is not the active one. `mode=semantic` answers `unavailable` in the same case.
  - Vector hits with `Score <= 0` are dropped; `Score` is cosine similarity.
  - The repository already limits hits to current versions of live assets (`core`'s I106). `ai.find_segment` still filters on the version it read, because its transcript is that version's timeline.
- **Why.** Fusion uses ranks only, so the only effect of `Score` is this floor. It lets the fake embedder's orthogonal vectors count as no match. A mixed query would break R18, so a query vector of another model is never used, even if that makes search lexical during a model switch.
- **Revisit if.** Load tests (D5) set other limits.

### I213. Who swaps the active embedding model

- **Choice.** The first model written to a tenant becomes active in the repository (`core`'s I106). When `ai.index` indexes vectors of another model, it stores them beside the old ones and then checks every asset holding vectors of the active model at its current version. Once all also hold the new model, it swaps with `SetActiveModel` and removes the old vectors with `DeleteModel`.
- **Why.** D15 says the new index replaces the old one only when complete, and `reindex-asset` works one asset at a time, so the last asset finishes the job. The check costs one walk of the tenant's assets, and only while models differ. Vectors can always be rebuilt from the stored transcript.
- **Revisit if.** Tenants with many assets make the walk slow; a counter can replace it.

### I214. Analysis works in blocks

- **Choice.** A transcript becomes timestamped lines, grouped into blocks of at most 16 000 characters at sentence boundaries. Each block is one JSON-mode call. Chapters, stories and tags are concatenated, sorted and deduplicated. With more than one block, one more call condenses the block summaries. Output is validated: finite numbers, inside the duration, snapped to sentence boundaries, strings capped. Invalid entries are dropped, and a response that is not JSON counts as a failure of the block.
- **Why.** A 3-hour recording does not fit in a local model's context, and §4.3 says LLM cost follows the text sent. 16 000 characters is about 4 000 tokens, which leaves room in an 8K context.
- **Revisit if.** Boundaries between blocks split chapters in practice.

## Budget

### I216. Usage records

- **Choice.** Nodes that call an LLM record one `UsageRecord` per endpoint: operation `ai.external` or `ai.local` by class, unit `tokens`, input plus output tokens, priced from `config.BudgetCosts.PerUnitMinor` when the operation is listed. `ai.transcribe` records `ai.transcription`, unit `seconds`, and `ai.embed` records `ai.embedding`, unit `chunks`, unpriced, because pool `ai` has no tenant configuration and both run locally.
- **Disagreement flagged.** The catalog marks `ai.analyze` and `ai.editorial` as `paid` but not `ai.find_segment`, which can call the same LLM. Per section 7 this component does not change an existing entry, so a job over budget can still spend a bounded prompt on `ai.find_segment`. Its usage is recorded. Listed in the final report for `integration`.
- **Why.** D19 and I8 estimate from a configured table; the driver returns the token counts.
- **Revisit if.** Automatic cost discovery is added.

## Tests and the demo

### I217. Mock LLM

- **Choice.** `connectors/ai/openai/mockllm` is a deterministic OpenAI-compatible handler that picks passages by word overlap, prefixes every string it writes with `[mock]`, records requests for assertions and injects failures. `connectors/ai/openai/mockllm/cmd/vidyom-mock-llm` serves it on a loopback port for `scripts/demo.sh`. The prompts carry a `Task: <name>` line so the mock can route. A real model ignores it.
- **Why.** I12 requires a mock LLM for tests and the demo, and no real vendor may be called.
- **Revisit if.** Never for production; it must stay out of default configuration.
