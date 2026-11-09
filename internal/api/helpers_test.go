package apiserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

type errString string

func (e errString) Error() string { return string(e) }

func idRef(s string) id.Ref { return id.Ref(s) }

func newRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, path, nil)
}

func withBody(r *http.Request, body string) *http.Request {
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
	return r
}

func serveRequest(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
