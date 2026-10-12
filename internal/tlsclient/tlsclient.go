// Package tlsclient wraps bogdanfinn/tls-client and returns standard
// *http.Response, keeping fhttp internal.  Modeled on aurora-develop/aurora.
package tlsclient

import (
	"io"
	"net/http"
	"net/url"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/net/proxy"
)

// Client wraps a tls_client.HttpClient and exposes a standard-http-like API.
type Client struct {
	inner tls_client.HttpClient
	// jar is kept here instead of inside the wrapped client so that cookies it
	// replays can be de-duplicated against the Cookie header the caller already
	// built (fhttp would blindly append them, producing duplicate cookies).
	jar       tls_client.CookieJar
	ReqBefore func(r *fhttp.Request) error
}

// Option is a builder for New().
type Option func(*options)

type options struct {
	proxyURL       string
	timeoutSeconds int
}

// defaultTimeoutSeconds is the whole-request hard deadline (it also covers
// reading the response body) applied when WithTimeout is not given.
const defaultTimeoutSeconds = 120

// WithProxy sets the proxy URL.
func WithProxy(raw string) Option {
	return func(o *options) { o.proxyURL = raw }
}

// WithTimeout sets the client-level hard deadline in seconds. tls-client uses
// this single value as the whole-exchange deadline (including reading the
// response body, which cuts streams), as the TCP dial timeout and as the proxy
// CONNECT deadline. It cannot be 0: a zero value makes the CONNECT deadline
// expire immediately. Streaming users should pass a large backstop and rely on
// the request context for the real deadline. Values <= 0 are ignored.
func WithTimeout(seconds int) Option {
	return func(o *options) {
		if seconds > 0 {
			o.timeoutSeconds = seconds
		}
	}
}

// New creates a *Client with Chrome_146 TLS fingerprinting. It panics when the
// client cannot be built (e.g. an invalid proxy URL); request paths should use
// NewClient instead.
func New(opts ...Option) *Client {
	c, err := NewClient(opts...)
	if err != nil {
		panic("tlsclient.New: " + err.Error())
	}
	return c
}

// NewClient is like New but reports construction failures as an error.
func NewClient(opts ...Option) (*Client, error) {
	o := &options{timeoutSeconds: defaultTimeoutSeconds}
	for _, fn := range opts {
		fn(o)
	}
	jar := tls_client.NewCookieJar()
	baseOpts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(o.timeoutSeconds),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithForceHttp1(),
		tls_client.WithNotFollowRedirects(),
	}
	if o.proxyURL != "" {
		baseOpts = append(baseOpts, tls_client.WithProxyUrl(o.proxyURL))
	}
	inner, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), baseOpts...)
	if err != nil {
		return nil, err
	}
	return &Client{inner: inner, jar: jar}, nil
}

// Do sends a standard *http.Request and returns a standard *http.Response.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	freq, err := toFhttp(req)
	if err != nil {
		return nil, err
	}
	if c.ReqBefore != nil {
		if err := c.ReqBefore(freq); err != nil {
			return nil, err
		}
	}
	c.attachJarCookies(freq)
	fresp, err := c.inner.Do(freq)
	if err != nil {
		return nil, err
	}
	if rc := fresp.Cookies(); len(rc) > 0 {
		c.jar.SetCookies(freq.URL, rc)
	}
	resp := toNetHTTP(fresp)
	resp.Request = req
	return resp, nil
}

// attachJarCookies appends jar cookies to the request, skipping names the
// caller already supplied in its own Cookie header (caller's value wins).
func (c *Client) attachJarCookies(freq *fhttp.Request) {
	have := map[string]struct{}{}
	for _, ck := range freq.Cookies() {
		have[ck.Name] = struct{}{}
	}
	for _, ck := range c.jar.Cookies(freq.URL) {
		if _, dup := have[ck.Name]; dup {
			continue
		}
		have[ck.Name] = struct{}{}
		freq.AddCookie(ck)
	}
}

// SetProxy changes the proxy at runtime.
func (c *Client) SetProxy(raw string) error { return c.inner.SetProxy(raw) }

// GetDialer returns the browser-profiled network dialer used by the wrapped client.
func (c *Client) GetDialer() proxy.ContextDialer { return c.inner.GetDialer() }

// GetTLSDialer returns the browser-profiled TLS dialer used by the wrapped client.
func (c *Client) GetTLSDialer() tls_client.TLSDialerFunc { return c.inner.GetTLSDialer() }

// CloseIdleConnections closes idle connections held by the wrapped client.
func (c *Client) CloseIdleConnections() { c.inner.CloseIdleConnections() }

// Close releases the connections the client holds. In-flight requests are not
// interrupted; the client must not be used for new requests afterwards.
func (c *Client) Close() { c.inner.CloseIdleConnections() }

// GetCookies returns cookies from the jar (as net/http.Cookie).
func (c *Client) GetCookies(u *url.URL) []*http.Cookie {
	fcs := c.jar.Cookies(u)
	if len(fcs) == 0 {
		return nil
	}
	cs := make([]*http.Cookie, len(fcs))
	for i, fc := range fcs {
		cs[i] = cookieToNet(fc)
	}
	return cs
}

// SetCookies stores net/http.Cookies into the jar.
func (c *Client) SetCookies(u *url.URL, cookies []*http.Cookie) {
	fcs := make([]*fhttp.Cookie, len(cookies))
	for i, ck := range cookies {
		fcs[i] = &fhttp.Cookie{
			Name:     ck.Name,
			Value:    ck.Value,
			Path:     ck.Path,
			Domain:   ck.Domain,
			Expires:  ck.Expires,
			Secure:   ck.Secure,
			HttpOnly: ck.HttpOnly,
			SameSite: fhttp.SameSite(ck.SameSite),
		}
	}
	c.jar.SetCookies(u, fcs)
}

// ---- conversions ---------------------------------------------------------

func toFhttp(req *http.Request) (*fhttp.Request, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if req.Body != nil {
		body = req.Body
	}
	// The caller's context must travel with the request so cancellation and
	// deadlines abort the upstream exchange.
	freq, err := fhttp.NewRequestWithContext(req.Context(), method, req.URL.String(), body)
	if err != nil {
		return nil, err
	}
	// The Cookie header (if any) is copied verbatim with the other headers;
	// re-adding req.Cookies() on top of it would send every cookie twice.
	// Every value of a multi-valued header is kept (Set would collapse them to
	// the last one).
	for k, vs := range req.Header {
		for i, v := range vs {
			if i == 0 {
				freq.Header.Set(k, v)
			} else {
				freq.Header.Add(k, v)
			}
		}
	}
	return freq, nil
}

func toNetHTTP(fresp *fhttp.Response) *http.Response {
	return &http.Response{
		Status:           fresp.Status,
		StatusCode:       fresp.StatusCode,
		Proto:            fresp.Proto,
		ProtoMajor:       fresp.ProtoMajor,
		ProtoMinor:       fresp.ProtoMinor,
		Header:           http.Header(fresp.Header),
		Body:             fresp.Body,
		ContentLength:    fresp.ContentLength,
		TransferEncoding: fresp.TransferEncoding,
		Close:            fresp.Close,
		Uncompressed:     fresp.Uncompressed,
		Trailer:          http.Header(fresp.Trailer),
	}
}

func cookieToNet(fc *fhttp.Cookie) *http.Cookie {
	return &http.Cookie{
		Name:     fc.Name,
		Value:    fc.Value,
		Path:     fc.Path,
		Domain:   fc.Domain,
		Expires:  fc.Expires,
		Secure:   fc.Secure,
		HttpOnly: fc.HttpOnly,
		SameSite: http.SameSite(fc.SameSite),
	}
}
