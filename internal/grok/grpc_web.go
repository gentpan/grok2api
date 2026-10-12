package grok

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// Minimal gRPC-Web codec: enough to send unary requests and read the data
// frames and trailers of the answer.

const (
	grpcFlagCompressed = 0x01
	grpcFlagTrailers   = 0x80
)

// encodeGRPCWebPayload wraps data in a gRPC-Web data frame (flag=0x00).
// Format: 0x00 + uint32 BE length + payload.
func encodeGRPCWebPayload(data []byte) []byte {
	out := make([]byte, 5+len(data))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], uint32(len(data)))
	copy(out[5:], data)
	return out
}

// GRPCStatus carries the gRPC code and message extracted from trailers.
// Code is -1 when the response carried no status.
type GRPCStatus struct {
	Code    int
	Message string
}

// OK returns true when the gRPC code is 0 (OK).
func (s GRPCStatus) OK() bool { return s.Code == 0 }

// HTTPEquiv returns the best-effort HTTP-equivalent status code for the gRPC
// code.
func (s GRPCStatus) HTTPEquiv() int {
	switch s.Code {
	case 0:
		return 200
	case 4: // DEADLINE_EXCEEDED
		return 504
	case 7: // PERMISSION_DENIED
		return 403
	case 8: // RESOURCE_EXHAUSTED
		return 429
	case 14: // UNAVAILABLE
		return 503
	case 16: // UNAUTHENTICATED
		return 401
	}
	return 502
}

// parseGRPCWebResponse parses a gRPC-Web response body. It returns the data
// messages and the trailers ("grpc-status" / "grpc-message", lower-cased keys,
// grpc-message percent-decoded). Trailers carried as HTTP headers instead of
// a trailer frame are merged in.
func parseGRPCWebResponse(body []byte, contentType string, headers http.Header) ([][]byte, map[string]string) {
	decoded := maybeDecodeBase64(body, contentType)
	messages := [][]byte{}
	trailers := map[string]string{}

	for i, n := 0, len(decoded); n-i >= 5; {
		flag := decoded[i]
		length := binary.BigEndian.Uint32(decoded[i+1 : i+5])
		i += 5
		if uint32(n-i) < length {
			break
		}
		payload := decoded[i : i+int(length)]
		i += int(length)

		switch {
		case flag&grpcFlagTrailers != 0:
			for k, v := range parseGRPCTrailers(payload) {
				trailers[k] = v
			}
		case flag&grpcFlagCompressed != 0:
			// compressed frames are not supported; ignore
		default:
			messages = append(messages, payload)
		}
	}

	// Supplement from HTTP headers (some servers send trailers as headers).
	for _, key := range []string{"grpc-status", "grpc-message"} {
		if _, ok := trailers[key]; ok {
			continue
		}
		if val := strings.TrimSpace(headerGetCI(headers, key)); val != "" {
			trailers[key] = val
		}
	}
	// grpc-message is percent-encoded on the wire regardless of where it came
	// from; it is decoded exactly once, here.
	if raw, ok := trailers["grpc-message"]; ok {
		trailers["grpc-message"] = unquoteGRPCMessage(raw)
	}
	return messages, trailers
}

// getGRPCStatus extracts a GRPCStatus from parsed trailers.
func getGRPCStatus(trailers map[string]string) GRPCStatus {
	raw := strings.TrimSpace(trailers["grpc-status"])
	msg := strings.TrimSpace(trailers["grpc-message"])
	code := -1
	if raw != "" {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err == nil {
			code = n
		}
	}
	return GRPCStatus{Code: code, Message: msg}
}

func parseGRPCTrailers(payload []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimRight(line, "\r")
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(line[:idx]))] = strings.TrimSpace(line[idx+1:])
	}
	return out
}

// maybeDecodeBase64 unwraps grpc-web-text payloads (base64-encoded bodies).
func maybeDecodeBase64(body []byte, contentType string) []byte {
	if strings.Contains(strings.ToLower(contentType), "grpc-web-text") {
		if decoded, err := base64.StdEncoding.DecodeString(string(removeSpace(body))); err == nil {
			return decoded
		}
		return body
	}
	// Heuristic: if the leading bytes look like base64 and not a gRPC frame,
	// attempt to decode.
	if len(body) >= 1 && body[0] != 0x00 && body[0] != grpcFlagTrailers {
		head := body
		if len(head) > 2048 {
			head = head[:2048]
		}
		if isProbablyBase64(head) {
			if decoded, err := base64.StdEncoding.DecodeString(string(removeSpace(body))); err == nil {
				return decoded
			}
		}
	}
	return body
}

func isProbablyBase64(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+', c == '/', c == '=', c == '\r', c == '\n', c == ' ', c == '\t':
		default:
			return false
		}
	}
	return true
}

// removeSpace drops all whitespace from b.
func removeSpace(b []byte) []byte {
	return bytes.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, b)
}

// unquoteGRPCMessage percent-decodes a grpc-message value. It is lenient: a
// malformed escape is kept verbatim.
func unquoteGRPCMessage(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			hi, lo := fromHex(s[i+1]), fromHex(s[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func fromHex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// headerGetCI does a case-insensitive header lookup.
func headerGetCI(h http.Header, key string) string {
	for k, vs := range h {
		if len(vs) > 0 && strings.EqualFold(k, key) {
			return vs[0]
		}
	}
	return ""
}
