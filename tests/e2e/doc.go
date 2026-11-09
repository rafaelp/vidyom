// Package e2e is the acceptance suite of the first executable version: the §1.1
// scenario of docs/plans/00-master-plan.md, driven through the real binaries over the
// real API. Everything lives behind the e2e build tag, so `go test ./...` stays free of
// Redis, FFmpeg, Node and Python. Run it with `make e2e`. Against Docker Compose:
// `make e2e-compose`.
package e2e
