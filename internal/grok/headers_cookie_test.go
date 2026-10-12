package grok

import (
	"strings"
	"testing"
)

func TestBuildSSOCookieKeepsCookieSeparators(t *testing.T) {
	cases := []struct {
		name      string
		cfCookies string
		clearance string
		want      string
	}{
		{"middle", "__cf_bm=abc; cf_clearance=xyz; o=1", "xyz", "__cf_bm=abc; cf_clearance=xyz; o=1"},
		{"middle replaced", "__cf_bm=abc; cf_clearance=xyz; o=1", "NEW", "__cf_bm=abc; cf_clearance=NEW; o=1"},
		{"first", "cf_clearance=xyz; __cf_bm=abc", "NEW", "cf_clearance=NEW; __cf_bm=abc"},
		{"last", "__cf_bm=abc; cf_clearance=xyz", "NEW", "__cf_bm=abc; cf_clearance=NEW"},
		{"dollar in value", "a=1; cf_clearance=xyz", "n$1w", "a=1; cf_clearance=n$1w"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSSOCookie("tok", proxyProfile{CFCookies: tc.cfCookies, CFClearance: tc.clearance})
			if !strings.HasSuffix(got, tc.want) {
				t.Fatalf("cookie = %q, want suffix %q", got, tc.want)
			}
			if strings.Count(got, "cf_clearance=") != 1 {
				t.Fatalf("cf_clearance appears %d times in %q", strings.Count(got, "cf_clearance="), got)
			}
		})
	}
}

func TestBuildSSOCookieAppendsMissingClearance(t *testing.T) {
	got := buildSSOCookie("tok", proxyProfile{CFCookies: "__cf_bm=abc", CFClearance: "xyz"})
	if !strings.HasSuffix(got, "__cf_bm=abc; cf_clearance=xyz") {
		t.Fatalf("cookie = %q", got)
	}
}
