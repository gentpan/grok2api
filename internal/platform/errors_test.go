package platform

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestIsInvalidCredentialsBody(t *testing.T) {
	cases := map[string]bool{
		"":                                  false,
		"all good":                          false,
		`{"error":"invalid-credentials"}`:   true,
		`{"error":"Invalid-Credentials"}`:   true,
		"Failed To Look Up Session ID: abc": true,
		"... TOKEN EXPIRED ...":             true,
		"blocked-user":                      true,
		"invalid credentials (no dash)":     false,
		"ünïcödé body with session not found ✓": true,
	}
	for body, want := range cases {
		if got := IsInvalidCredentialsBody(body); got != want {
			t.Errorf("IsInvalidCredentialsBody(%q) = %v, want %v", body, got, want)
		}
	}
}

// The body can be large upstream HTML; matching must not copy/lower-case it.
func TestIsInvalidCredentialsBodyDoesNotAllocate(t *testing.T) {
	body := strings.Repeat("<div>Some Upstream Error PAGE</div>", 400) + "Bad-Credentials"
	if !IsInvalidCredentialsBody(body) {
		t.Fatal("marker not found")
	}
	if n := testing.AllocsPerRun(20, func() { IsInvalidCredentialsBody(body) }); n != 0 {
		t.Fatalf("IsInvalidCredentialsBody allocates %v times per call", n)
	}
}

func TestIsInvalidCredentials(t *testing.T) {
	bad := UpstreamError("upstream", 401, `{"error":"bad-credentials"}`)
	if !IsInvalidCredentials(bad) {
		t.Error("401 + marker must be invalid credentials")
	}
	if !IsInvalidCredentials(fmt.Errorf("fetch usage: %w", bad)) {
		t.Error("wrapped AppError must be detected via errors.As")
	}
	for _, status := range []int{400, 403} {
		if !IsInvalidCredentials(UpstreamError("x", status, "invalid-credentials")) {
			t.Errorf("status %d + marker must match", status)
		}
	}
	if IsInvalidCredentials(UpstreamError("x", 500, "invalid-credentials")) {
		t.Error("5xx must not count as invalid credentials")
	}
	if IsInvalidCredentials(UpstreamError("x", 401, "rate limited")) {
		t.Error("401 without marker must not match")
	}
	if IsInvalidCredentials(errors.New("invalid-credentials")) {
		t.Error("a plain error carries no status and must not match")
	}
	if IsInvalidCredentials(nil) {
		t.Error("nil error must not match")
	}
	var nilApp *AppError
	if IsInvalidCredentials(nilApp) {
		t.Error("typed nil *AppError must not panic or match")
	}
}

func TestValidationErrorParam(t *testing.T) {
	e := ValidationError("bad", "")
	if e.Param != "" || e.Status != 400 || e.Kind != ErrValidation {
		t.Fatalf("unexpected: %+v", e)
	}
	if _, ok := e.ToDict()["error"].(map[string]any)["param"]; ok {
		t.Fatal("empty param must be omitted from the envelope")
	}
}
