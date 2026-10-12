package grok

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gentpan/grok2api/internal/platform"
)

// All errors that leave this package towards internal/api are
// *platform.AppError values: api maps them with errors.As and the account
// layer reads Status/Body for cooldown and credential decisions. The helpers
// in this file are the only place that decides which status an upstream
// failure maps to.

// grpcResourceExhausted is the gRPC status code (RESOURCE_EXHAUSTED) grok uses
// for rate limiting inside stream errors.
const grpcResourceExhausted = 8

// rateLimitMarkers are the lower-cased substrings that mark a code or message
// as an upstream rate limit. They are matched against error *codes* and
// *messages* only as whole phrases: a bare "rate" or "limit" is deliberately not
// a marker (it would match "generate_failed" or "context_limit_exceeded").
var rateLimitMarkers = []string{
	"rate_limit", "rate-limit", "rate limit", "ratelimit",
	"too many requests", "too_many_requests",
	"resource_exhausted",
}

func hasRateLimitMarker(s string) bool {
	s = strings.ToLower(s)
	for _, m := range rateLimitMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isRateLimited reports whether an in-band upstream error (REST stream
// {"error":{...}}, Gateway stream_error / error events, Console response.failed)
// describes a rate limit. code may be a JSON number, a string or nil.
func isRateLimited(code any, message string) bool {
	switch c := code.(type) {
	case float64:
		if int(c) == grpcResourceExhausted {
			return true
		}
	case int:
		if c == grpcResourceExhausted {
			return true
		}
	case json.Number:
		if n, err := c.Int64(); err == nil && n == grpcResourceExhausted {
			return true
		}
	case string:
		if strings.TrimSpace(c) == "8" || hasRateLimitMarker(c) {
			return true
		}
	}
	return hasRateLimitMarker(message)
}

// streamErrorStatus maps an in-band upstream error to the HTTP status used for
// account feedback: rate limits become 429 (cooldown), everything else 502.
func streamErrorStatus(code any, message string) int {
	if isRateLimited(code, message) {
		return 429
	}
	return 502
}

// upstreamErr is shorthand for a 502 upstream error without a body.
func upstreamErr(format string, args ...any) *platform.AppError {
	return platform.UpstreamError(fmt.Sprintf(format, args...), 502, "")
}

// prefixError returns err with prefix prepended to its message. An *AppError
// keeps its status, body and code (so cooldown / credential classification
// still works); any other error becomes a 502 upstream error.
func prefixError(prefix string, err error) *platform.AppError {
	var appErr *platform.AppError
	if errors.As(err, &appErr) {
		cp := *appErr
		cp.Message = prefix + ": " + appErr.Message
		return &cp
	}
	return platform.UpstreamError(prefix+": "+err.Error(), 502, "")
}

// asAppError returns the *platform.AppError carried by err, if any.
func asAppError(err error) (*platform.AppError, bool) {
	var appErr *platform.AppError
	if errors.As(err, &appErr) && appErr != nil {
		return appErr, true
	}
	return nil, false
}

// isInvalidCredentials reports whether err says the SSO token itself is bad
// (as opposed to a transient or quota failure).
func isInvalidCredentials(err error) bool {
	appErr, ok := asAppError(err)
	if !ok {
		return false
	}
	switch appErr.Status {
	case 400, 401, 403:
		return platform.IsInvalidCredentialsBody(appErr.Body)
	}
	return false
}

// statusError converts a non-2xx upstream response into an AppError. The body
// is truncated to bodyLimit bytes on a rune boundary; withBodyInMessage
// appends it to the message (stream endpoints surface it to clients).
func statusError(status int, body []byte, bodyLimit int, withBodyInMessage bool) *platform.AppError {
	bt := truncBody(body, bodyLimit)
	msg := fmt.Sprintf("Upstream returned %d", status)
	if withBodyInMessage && bt != "" {
		msg += ": " + bt
	}
	return platform.UpstreamError(msg, status, bt)
}

// errUntrustedHost is returned when Console credentials would be sent to a
// host outside the trusted set.
func errUntrustedHost() *platform.AppError {
	return platform.UpstreamError("refusing to send Console credentials to an untrusted host", 502, "")
}

// errTransport wraps a failure of the HTTP round trip itself.
func errTransport(err error) *platform.AppError {
	return platform.UpstreamError("transport request failed: "+err.Error(), 502, "")
}

// truncBody shortens b to at most n bytes without splitting a UTF-8 rune.
func truncBody(b []byte, n int) string { return truncUTF8(string(b), n) }
