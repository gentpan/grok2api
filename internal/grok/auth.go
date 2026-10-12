package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
)

// birthDateLocked is the marker in the 429 body upstream returns when the
// account's birth date was already set and can no longer be changed.
const birthDateLocked = "birth-date-change-limit-reached"

// nsfwTimeout is the per-call bound of the ToS / NSFW / birth-date calls.
func nsfwTimeout() time.Duration { return configSeconds("nsfw.timeout", 30) }

// buildAcceptTOSPayload builds the gRPC-Web payload for
// SetTosAcceptedVersion (proto field 2 = true).
//
// Wire format: 0x10 0x01 (proto field 2, varint, value 1).
func buildAcceptTOSPayload() []byte {
	return encodeGRPCWebPayload([]byte{0x10, 0x01})
}

// buildNSFWMgmtPayload builds the gRPC-Web payload that toggles
// always_show_nsfw_content.
//
// Wire layout (mirrors xai_auth.py build_nsfw_mgmt_payload):
//
//	inner = 0x0a + len(name) + name                       # field 1: string
//	protobuf = 0x0a 0x02 0x10 <bool> 0x12 + len(inner) + inner
//	└ field 1 (length-delimited): nested { field 2 (varint bool),
//	  field 1 (string: "always_show_nsfw_content") }
//	frame = 0x00 + uint32 BE len(protobuf) + protobuf    # gRPC-Web data frame
func buildNSFWMgmtPayload(enabled bool) []byte {
	name := []byte("always_show_nsfw_content")
	inner := append([]byte{0x0a, byte(len(name))}, name...)
	var boolByte byte
	if enabled {
		boolByte = 0x01
	}
	protobuf := []byte{0x0a, 0x02, 0x10, boolByte, 0x12, byte(len(inner))}
	protobuf = append(protobuf, inner...)
	return encodeGRPCWebPayload(protobuf)
}

// buildSetBirthPayload returns the JSON payload for /rest/auth/set-birth-date
// with a random adult birth date.
func buildSetBirthPayload() map[string]any {
	now := time.Now().UTC()
	year := now.Year() - (20 + rand.IntN(29)) // 20–48 years old
	month := 1 + rand.IntN(12)
	day := 1 + rand.IntN(28)
	stamp := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03dZ",
		year, month, day, rand.IntN(24), rand.IntN(60), rand.IntN(60), rand.IntN(1000))
	return map[string]any{"birthDate": stamp}
}

// grpcCall posts a gRPC-Web frame and turns a failing gRPC status into an
// AppError (a missing status, code -1, is treated as success).
func grpcCall(ctx context.Context, t *Transport, label, endpoint, token string, payload []byte, opts ...RequestOption) (GRPCStatus, error) {
	_, trailers, err := t.PostGRPCWeb(ctx, endpoint, token, payload, opts...)
	if err != nil {
		return GRPCStatus{}, err
	}
	status := getGRPCStatus(trailers)
	if status.OK() || status.Code == -1 {
		return status, nil
	}
	return status, platform.UpstreamError(
		fmt.Sprintf("%s: gRPC error code=%d message=%s", label, status.Code, status.Message),
		status.HTTPEquiv(), "",
	)
}

// acceptTOS calls SetTosAcceptedVersion on accounts.x.ai.
func acceptTOS(ctx context.Context, t *Transport, token string) (GRPCStatus, error) {
	return grpcCall(ctx, t, "accept_tos", AcceptTOSURL, token, buildAcceptTOSPayload(),
		WithOrigin(AccountsBase),
		WithReferer(AccountsBase+"/accept-tos"),
		WithTimeout(nsfwTimeout()),
	)
}

// setNSFW toggles always_show_nsfw_content via gRPC-Web.
func setNSFW(ctx context.Context, t *Transport, token string, enabled bool) (GRPCStatus, error) {
	label := "enable_nsfw"
	if !enabled {
		label = "disable_nsfw"
	}
	return grpcCall(ctx, t, label, NSFWMgmtURL, token, buildNSFWMgmtPayload(enabled),
		WithOrigin(Base),
		WithReferer(Base+"/?_s=data"),
		WithTimeout(nsfwTimeout()),
	)
}

// DisableNSFW turns always_show_nsfw_content off for token.
func DisableNSFW(ctx context.Context, t *Transport, token string) (GRPCStatus, error) {
	return setNSFW(ctx, t, token, false)
}

// setBirthDate posts a random adult birth date for token via REST.
func setBirthDate(ctx context.Context, t *Transport, token string) (map[string]any, error) {
	body, err := json.Marshal(buildSetBirthPayload())
	if err != nil {
		return nil, platform.UpstreamError("set_birth_date: encode failed: "+err.Error(), 500, "")
	}
	return t.PostJSON(ctx, SetBirthURL, token, body,
		WithOrigin(Base),
		WithReferer(Base+"/?_s=data"),
		WithTimeout(nsfwTimeout()),
	)
}

// isBirthDateLocked reports whether err is upstream's "birth date already set
// and locked" answer (HTTP 429 carrying birthDateLocked in the body).
func isBirthDateLocked(err error) bool {
	appErr, ok := asAppError(err)
	return ok && appErr.Status == 429 && strings.Contains(appErr.Body, birthDateLocked)
}

// NSFWSequence runs accept_tos → set_birth_date → enable_nsfw.
//
// The set_birth_date 429-with-birth-date-change-limit-reached response means
// the birth date is already locked; this is treated as a no-op success.
func NSFWSequence(ctx context.Context, t *Transport, token string) error {
	if _, err := acceptTOS(ctx, t, token); err != nil {
		return err
	}
	if _, err := setBirthDate(ctx, t, token); err != nil && !isBirthDateLocked(err) {
		return err
	}
	_, err := setNSFW(ctx, t, token, true)
	return err
}
