package statsig

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"testing"
)

// Live browser reference (sY real output, build at capture time):
//
//	seed   = 1zyptn4udMOQU5tdgJBcp9Zu71BLajtU53nI+Mi+4VPN+d9BXkYvGxeEnBRa3ow0  (seed[5]%4=2)
//	number = 99789180, key = 143
//	full 70-byte x-statsig-id captured below.
//
// If reconstructing with refHEX (the seed[5]%4=2 fingerprint captured on an
// earlier load) reproduces these exact bytes, then refHEX is the correct stable
// per-build constant AND the pure-Go layout is byte-exact vs grok's own JS.
func TestCrossCheckAgainstLiveBrowser(t *testing.T) {
	const liveSeed = "1zyptn4udMOQU5tdgJBcp9Zu71BLajtU53nI+Mi+4VPN+d9BXkYvGxeEnBRa3ow0"
	const liveNumber = 99789180
	const liveKey = 143
	live := []byte{143, 88, 179, 38, 57, 241, 161, 251, 76, 31, 220, 20, 210, 15, 31, 211, 40, 89, 225, 96, 223, 196, 229, 180, 219, 104, 246, 71, 119, 71, 49, 110, 220, 66, 118, 80, 206, 209, 201, 160, 148, 152, 11, 19, 155, 213, 81, 3, 187, 243, 38, 125, 138, 27, 62, 96, 63, 212, 65, 52, 228, 53, 177, 114, 125, 99, 165, 182, 110, 140}

	seed, _ := base64.StdEncoding.DecodeString(liveSeed)
	if int(seed[5])%4 != 2 {
		t.Fatalf("seed[5]%%4=%d want 2", int(seed[5])%4)
	}

	input := "POST!/rest/app-chat/conversations/new!" + strconv.Itoa(liveNumber) + "obfiowerehiring" + "ad36d100100"
	sum := sha256.Sum256([]byte(input))

	got := make([]byte, 70)
	got[0] = liveKey
	for i := 0; i < 48; i++ {
		got[1+i] = seed[i] ^ liveKey
	}
	n := uint32(liveNumber)
	tail := []byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	tail = append(tail, sum[:16]...)
	tail = append(tail, 0x03)
	for i := 0; i < 21; i++ {
		got[49+i] = tail[i] ^ liveKey
	}

	for i := 0; i < 70; i++ {
		if got[i] != live[i] {
			t.Fatalf("byte %d differs: pure-Go=%d browser=%d (refHEX wrong for this build, or layout bug)", i, got[i], live[i])
		}
	}
	t.Logf("✓ pure-Go reproduced the live browser x-statsig-id BYTE-EXACT (70/70). refHEX is the correct seed[5]%%4=2 constant.")
}
