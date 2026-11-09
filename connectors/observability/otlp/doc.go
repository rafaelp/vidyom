// Package otlp is the OTLP exporter for metrics and traces over HTTP (§7.4, D38),
// built on the official OpenTelemetry exporters (I278).
//
// Tested only against a local mock of an OTLP collector, decoding real OTLP protobuf
// (I12). No real collector is contacted by the tests.
package otlp
