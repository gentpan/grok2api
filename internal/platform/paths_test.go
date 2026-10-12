package platform

import "testing"

func TestMaskTokenNeverPanics(t *testing.T) {
	cases := map[string]string{
		"":                 "***",
		"a":                "***",
		"12345678":         "***",
		"123456789":        "1234...6789",
		"eyJhbGciOiJIUzI1": "eyJh...UzI1",
	}
	for in, want := range cases {
		if got := MaskToken(in); got != want {
			t.Errorf("MaskToken(%q) = %q, want %q", in, got, want)
		}
	}
}
