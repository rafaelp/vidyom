package errs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPStatus(t *testing.T) {
	cases := map[Code]int{
		CodeNotFound:             http.StatusNotFound,
		CodeForbidden:            http.StatusForbidden,
		CodeForbiddenStorageRole: http.StatusForbidden,
		CodeTenantRequired:       http.StatusBadRequest,
		CodeUnauthenticated:      http.StatusUnauthorized,
		CodeBudgetExceeded:       http.StatusPaymentRequired,
		CodeSealed:               http.StatusServiceUnavailable,
		CodeNotImplemented:       http.StatusNotImplemented,
		CodeGone:                 http.StatusGone,
		Code("anything-else"):    http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := HTTPStatus(code); got != want {
			t.Errorf("HTTPStatus(%s) = %d, want %d", code, got, want)
		}
	}
}

func TestPublicHidesCauses(t *testing.T) {
	secret := errors.New("token=abc123")
	wrapped := Wrap(CodeUnavailable, secret, "storage unavailable")
	body, err := json.Marshal(Body{Error: Public(wrapped)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "abc123") {
		t.Fatalf("cause leaked into the public body: %s", body)
	}
	if !errors.Is(wrapped, secret) {
		t.Fatal("cause must stay reachable for logs")
	}

	plain := Public(fmt.Errorf("db path /secret/place: boom"))
	if plain.Code != CodeInternal || strings.Contains(plain.Message, "secret") {
		t.Fatalf("uncoded error leaked: %+v", plain)
	}
}

func TestCodeOfThroughWrapping(t *testing.T) {
	err := fmt.Errorf("outer: %w", NotFound("asset"))
	if CodeOf(err) != CodeNotFound || !IsCode(err, CodeNotFound) {
		t.Fatalf("CodeOf = %s", CodeOf(err))
	}
	if CodeOf(errors.New("x")) != CodeInternal {
		t.Fatal("uncoded errors are internal")
	}
}

func TestWithCopies(t *testing.T) {
	base := New(CodeConflict, "x")
	withDetail := base.With("offset", 10)
	if base.Details != nil {
		t.Fatal("With must not mutate the receiver")
	}
	if withDetail.Details["offset"] != 10 {
		t.Fatal("detail missing")
	}
}
