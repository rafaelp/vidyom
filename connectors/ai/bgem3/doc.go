// Package bgem3 is the embedding connector, a client of the Python AI worker (D9, I6).
// The model runs in the worker; this package splits batches and maps results.
//
// Vectors report the model that produced them: a worker started with --engine fake
// returns "fake-hash-embed@1", which only matches shared words, and this connector
// reaches such a worker only when aiWorker.engine says "fake" (I27, I201).
package bgem3
