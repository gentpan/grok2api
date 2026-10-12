package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/gentpan/grok2api/internal/platform"
)

// errorEnvelope is the wire shape an endpoint family reports errors in.
type errorEnvelope int

const (
	// envOpenAI is {"error":{"message","type","code"[,"param"]}} — the
	// /v1 OpenAI-compatible API and the admin JSON API.
	envOpenAI errorEnvelope = iota
	// envAnthropic is {"type":"error","error":{"type","message"}} — /v1/messages.
	envAnthropic
)

// envelopeKey is the gin context key holding the route group's envelope.
const envelopeKey = "error_envelope"

// useEnvelope marks every handler downstream of it as speaking e.
func useEnvelope(e errorEnvelope) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(envelopeKey, e)
		c.Next()
	}
}

func envelopeOf(c *gin.Context) errorEnvelope {
	if v, ok := c.Get(envelopeKey); ok {
		if e, ok := v.(errorEnvelope); ok {
			return e
		}
	}
	return envOpenAI
}

// platformAppError returns the *platform.AppError inside err, or nil.
func platformAppError(err error) *platform.AppError {
	var appErr *platform.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return nil
}

// asAppError normalises any error into an *platform.AppError: an AppError is
// returned as is, anything else becomes a 500 internal_error.
func asAppError(err error) *platform.AppError {
	if appErr := platformAppError(err); appErr != nil {
		return appErr
	}
	return platform.NewAppError(err.Error(), platform.ErrServer, "internal_error", http.StatusInternalServerError)
}

// writeAppError answers with err in the envelope of the current route group.
func writeAppError(c *gin.Context, err error) {
	appErr := asAppError(err)
	switch envelopeOf(c) {
	case envAnthropic:
		c.JSON(appErr.Status, anthropicErrorBody(appErr))
	default:
		c.JSON(appErr.Status, appErr.ToDict())
	}
}

// anthropicErrorType maps an error to Anthropic's error type vocabulary.
func anthropicErrorType(e *platform.AppError) string {
	switch {
	case e.Status == http.StatusUnauthorized || e.Kind == platform.ErrAuthentication:
		return "authentication_error"
	case e.Status == http.StatusForbidden:
		return "permission_error"
	case e.Status == http.StatusNotFound:
		return "not_found_error"
	case e.Status == http.StatusTooManyRequests || e.Kind == platform.ErrRateLimit:
		return "rate_limit_error"
	case e.Status == http.StatusServiceUnavailable || e.Status == 529:
		return "overloaded_error"
	case e.Status >= 400 && e.Status < 500:
		return "invalid_request_error"
	}
	return "api_error"
}

// anthropicErrorBody is the Anthropic error envelope.
func anthropicErrorBody(e *platform.AppError) map[string]any {
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(e),
			"message": e.Message,
		},
	}
}

// authErr answers in the admin sign-in envelope
// {"error":{"message","type":"authentication_error"}}.
func authErr(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": gin.H{"message": msg, "type": "authentication_error"}})
}

// errModelNotFound is the single "unknown or disabled model" error.
func errModelNotFound(name string) *platform.AppError {
	return platform.ValidationErrorCode("Model '"+name+"' not found", "model", "model_not_found")
}

// readJSON decodes the request body into v using gin's binding.
func readJSON(c *gin.Context, v any) error {
	if err := c.ShouldBindJSON(v); err != nil {
		return platform.ValidationError("Invalid JSON body: "+err.Error(), "body")
	}
	return nil
}

// codeEmptyUpstream marks an upstream stream that ended without one event.
const codeEmptyUpstream = "upstream_empty_response"

// errEmptyUpstream reports a stream that ended without a single event. The
// client gets a 502 instead of an empty 200, but the account is not blamed:
// an empty (e.g. moderated) answer says nothing about its health.
func errEmptyUpstream() *platform.AppError {
	return platform.NewAppError("Upstream returned an empty response", platform.ErrUpstream, codeEmptyUpstream, http.StatusBadGateway)
}
