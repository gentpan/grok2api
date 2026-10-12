package tlsclient

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientExposesBrowserDialers(t *testing.T) {
	client := New()
	if client.GetDialer() == nil {
		t.Fatal("browser dialer is nil")
	}
	if client.GetTLSDialer() == nil {
		t.Fatal("browser TLS dialer is nil")
	}
}

// rawCapture is a one-shot TCP server that records the raw request head and
// answers 200 OK, so tests can assert on exactly what the client put on the
// wire (net/http's server would hide framing details such as chunking).
type rawCapture struct {
	addr string
	mu   sync.Mutex
	head string
	body string
	done chan struct{}
}

func startRawCapture(t *testing.T) *rawCapture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rc := &rawCapture{addr: ln.Addr().String(), done: make(chan struct{})}
	go func() {
		defer close(rc.done)
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		var head strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			head.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		rc.mu.Lock()
		rc.head = head.String()
		rc.mu.Unlock()
		lower := strings.ToLower(head.String())
		var body []byte
		switch {
		case strings.Contains(lower, "transfer-encoding: chunked"):
			// Read until the terminating zero-size chunk.
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					break
				}
				body = append(body, line...)
				if line == "0\r\n" {
					_, _ = br.ReadString('\n')
					break
				}
			}
		default:
			n := 0
			for _, l := range strings.Split(lower, "\r\n") {
				if v, ok := strings.CutPrefix(l, "content-length:"); ok {
					for _, c := range strings.TrimSpace(v) {
						n = n*10 + int(c-'0')
					}
				}
			}
			body = make([]byte, n)
			_, _ = io.ReadFull(br, body)
		}
		rc.mu.Lock()
		rc.body = string(body)
		rc.mu.Unlock()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
	}()
	return rc
}

func (rc *rawCapture) wait(t *testing.T) (head, body string) {
	t.Helper()
	select {
	case <-rc.done:
	case <-time.After(10 * time.Second):
		t.Fatal("capture server timed out")
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.head, rc.body
}

func doRaw(t *testing.T, c *Client, req *http.Request) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// POST bodies must reach the server intact. The request is currently framed
// with Transfer-Encoding: chunked because toFhttp hands fhttp an opaque body
// (no ContentLength). A real browser sends Content-Length; changing this is
// wire-visible and deliberately deferred until it can be validated against the
// real upstream. This test pins today's behaviour so a change is a conscious
// one: flip the framing assertion when Content-Length support is introduced.
func TestPostBodyReachesServerIntact(t *testing.T) {
	rc := startRawCapture(t)
	c := New()
	req, err := http.NewRequest(http.MethodPost, "http://"+rc.addr+"/p", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	doRaw(t, c, req)
	head, body := rc.wait(t)
	if !strings.Contains(body, `{"a":1}`) {
		t.Fatalf("body = %q", body)
	}
	if !strings.Contains(strings.ToLower(head), "transfer-encoding: chunked") {
		t.Fatalf("request framing changed (no longer chunked); update this pin deliberately:\n%s", head)
	}
}

// Every value of a multi-valued request header must be sent.
func TestMultiValueHeadersArePreserved(t *testing.T) {
	rc := startRawCapture(t)
	c := New()
	req, _ := http.NewRequest(http.MethodGet, "http://"+rc.addr+"/", nil)
	req.Header.Add("X-Multi", "one")
	req.Header.Add("X-Multi", "two")
	doRaw(t, c, req)
	head, _ := rc.wait(t)
	lower := strings.ToLower(head)
	if !strings.Contains(lower, "x-multi: one\r\n") || !strings.Contains(lower, "x-multi: two\r\n") {
		t.Fatalf("multi-valued header collapsed: %q", head)
	}
}

func TestResponseCarriesItsRequest(t *testing.T) {
	rc := startRawCapture(t)
	c := New()
	req, _ := http.NewRequest(http.MethodGet, "http://"+rc.addr+"/where", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.Request != req {
		t.Fatalf("resp.Request = %v, want the originating request", resp.Request)
	}
	rc.wait(t)
}

func TestHeaderValuesAreSentVerbatim(t *testing.T) {
	rc := startRawCapture(t)
	c := New()
	req, _ := http.NewRequest(http.MethodGet, "http://"+rc.addr+"/", nil)
	req.Header.Set("User-Agent", "UA/1")
	req.Header.Set("Cookie", "a=1; b=2")
	doRaw(t, c, req)
	head, _ := rc.wait(t)
	for _, want := range []string{"User-Agent: UA/1\r\n", "Cookie: a=1; b=2\r\n"} {
		if !strings.Contains(head, want) {
			t.Fatalf("head lacks %q: %q", want, head)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte("HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:1/\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
	}()
	c := New()
	req, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (redirect must be surfaced, not followed)", resp.StatusCode)
	}
}
