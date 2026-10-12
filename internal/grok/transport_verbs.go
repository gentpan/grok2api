package grok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
)

// Request options and the HTTP verbs of Transport. The request pipeline they
// sit on (headers per host class, DPoP, retry, reset policy) is in
// transport.go.

// --- request options -------------------------------------------------------

type reqOpts struct {
	timeout      time.Duration
	origin       string
	referer      string
	contentType  string
	consoleMode  bool
	extraHeaders http.Header
}

// RequestOption customises one Transport request.
type RequestOption func(*reqOpts)

// WithTimeout bounds the request. For stream requests it only bounds the wait
// for response headers; the body then lives until the caller's context ends or
// the returned body is closed. A non-positive value disables the bound.
func WithTimeout(d time.Duration) RequestOption { return func(o *reqOpts) { o.timeout = d } }

// WithOrigin and WithReferer are accepted for source compatibility. grok.com
// requests deliberately send neither header (see proxyProfile.httpHeaders), so
// they currently have no effect.
func WithOrigin(v string) RequestOption  { return func(o *reqOpts) { o.origin = v } }
func WithReferer(v string) RequestOption { return func(o *reqOpts) { o.referer = v } }

// WithConsoleMode targets console.x.ai: DPoP-bound credentials and the
// console browser header set.
func WithConsoleMode() RequestOption { return func(o *reqOpts) { o.consoleMode = true } }

// WithExtraHeaders overlays headers on the built request.
func WithExtraHeaders(h http.Header) RequestOption { return func(o *reqOpts) { o.extraHeaders = h } }

func applyOpts(opts []RequestOption) reqOpts {
	o := reqOpts{
		timeout:     defaultRequestTimeout,
		origin:      Base,
		referer:     Base + "/",
		contentType: defaultJSONType,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// --- HTTP verbs ------------------------------------------------------------

// PostJSON POSTs payload and decodes the JSON object response.
func (t *Transport) PostJSON(ctx context.Context, urlStr, token string, payload []byte, opts ...RequestOption) (map[string]any, error) {
	if payload == nil {
		payload = []byte{}
	}
	return t.doJSON(ctx, http.MethodPost, urlStr, token, payload, opts)
}

// GetJSON GETs urlStr and decodes the JSON object response.
func (t *Transport) GetJSON(ctx context.Context, urlStr, token string, opts ...RequestOption) (map[string]any, error) {
	return t.doJSON(ctx, http.MethodGet, urlStr, token, nil, opts)
}

// DeleteJSON sends DELETE to urlStr and decodes the JSON object response.
func (t *Transport) DeleteJSON(ctx context.Context, urlStr, token string, opts ...RequestOption) (map[string]any, error) {
	return t.doJSON(ctx, http.MethodDelete, urlStr, token, nil, opts)
}

// doJSON is the shared body of the JSON verbs: buffered response, non-2xx ->
// AppError, empty body -> {}.
func (t *Transport) doJSON(ctx context.Context, method, urlStr, token string, payload []byte, opts []RequestOption) (map[string]any, error) {
	resp, err := t.do(ctx, method, urlStr, token, payload, applyOpts(opts), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxJSONResponseBytes)
	if err != nil {
		return nil, platform.UpstreamError("read response body failed: "+err.Error(), resp.StatusCode, "")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError(resp.StatusCode, body, 400, false)
	}
	return decodeJSONObject(body, resp.StatusCode)
}

func decodeJSONObject(body []byte, status int) (map[string]any, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, platform.UpstreamError("failed to decode JSON body: "+err.Error(), status, truncBody(body, 400))
	}
	return out, nil
}

// readLimited reads all of r, failing when it exceeds limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d MiB", limit>>20)
	}
	return b, nil
}

// PostStream POSTs payload and returns the response body for streaming. The
// caller must Close it; the request context lives until then (or until ctx
// ends).
func (t *Transport) PostStream(ctx context.Context, urlStr, token string, payload []byte, opts ...RequestOption) (io.ReadCloser, error) {
	if payload == nil {
		payload = []byte{}
	}
	return t.doStream(ctx, http.MethodPost, urlStr, token, payload, opts, true)
}

// GetBytes GETs urlStr and returns the body for the caller to read and Close.
func (t *Transport) GetBytes(ctx context.Context, urlStr, token string, opts ...RequestOption) (io.ReadCloser, error) {
	return t.doStream(ctx, http.MethodGet, urlStr, token, nil, opts, false)
}

// doStream returns the body of a 2xx response; a non-2xx response is drained
// (bounded) and reported as an AppError. stream selects stream timeout
// semantics and whether the body text is echoed in the error message.
func (t *Transport) doStream(ctx context.Context, method, urlStr, token string, payload []byte, opts []RequestOption, stream bool) (io.ReadCloser, error) {
	resp, err := t.do(ctx, method, urlStr, token, payload, applyOpts(opts), stream)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, statusError(resp.StatusCode, body, 400, stream)
	}
	return resp.Body, nil
}

// PostGRPCWeb POSTs a gRPC-Web frame and returns the decoded messages and
// trailers.
func (t *Transport) PostGRPCWeb(ctx context.Context, urlStr, token string, payload []byte, opts ...RequestOption) ([][]byte, map[string]string, error) {
	o := applyOpts(opts)
	o.contentType = "application/grpc-web+proto"
	resp, err := t.do(ctx, http.MethodPost, urlStr, token, payload, o, false)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxJSONResponseBytes)
	if err != nil {
		return nil, nil, platform.UpstreamError("read response body failed: "+err.Error(), resp.StatusCode, "")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, statusError(resp.StatusCode, body, 400, false)
	}
	msgs, trailers := parseGRPCWebResponse(body, resp.Header.Get("Content-Type"), resp.Header)
	return msgs, trailers, nil
}
