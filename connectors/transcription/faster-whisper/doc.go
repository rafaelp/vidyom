// Package fasterwhisper is the transcription connector, a client of the Python AI
// worker (D9, I6). The model runs in the worker; this package carries requests and
// maps results.
//
// Transcripts report what actually produced them: a worker started with
// --engine fake returns canned text labeled "fake/...", and this connector reaches
// such a worker only when aiWorker.engine says "fake" (I27, I201).
package fasterwhisper
