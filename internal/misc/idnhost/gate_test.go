package idnhost

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchesBlockList(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		host     string
		want     bool
	}{
		{name: "exact", patterns: []string{"evil.example"}, host: "evil.example", want: true},
		{name: "subdomain", patterns: []string{"evil.example"}, host: "a.b.evil.example", want: true},
		{name: "lookalike prefix is not a subdomain", patterns: []string{"evil.example"}, host: "notevil.example", want: false},
		{name: "unrelated", patterns: []string{"evil.example"}, host: "good.example", want: false},
		{name: "empty host", patterns: []string{"evil.example"}, host: "", want: false},
		{name: "empty and blank patterns are skipped", patterns: []string{"", "  ", "."}, host: "evil.example", want: false},

		// ポートを変えるだけでブロックを回避できてはいけない。
		{name: "non-default port", patterns: []string{"evil.example"}, host: "evil.example:8443", want: true},
		{name: "subdomain with non-default port", patterns: []string{"evil.example"}, host: "sub.evil.example:8443", want: true},
		{name: "explicit default port", patterns: []string{"evil.example"}, host: "evil.example:443", want: true},
		{name: "empty port", patterns: []string{"evil.example"}, host: "evil.example:", want: true},
		{name: "lookalike with port", patterns: []string{"evil.example"}, host: "notevil.example:8443", want: false},
		{name: "port-specific pattern matches that port", patterns: []string{"evil.example:8443"}, host: "evil.example:8443", want: true},
		{name: "port-specific pattern matches subdomain on that port", patterns: []string{"evil.example:8443"}, host: "sub.evil.example:8443", want: true},
		{name: "port-specific pattern does not cover other ports", patterns: []string{"evil.example:8443"}, host: "evil.example:9443", want: false},
		{name: "port-specific pattern does not cover default port", patterns: []string{"evil.example:8443"}, host: "evil.example", want: false},

		{name: "trailing dot", patterns: []string{"evil.example"}, host: "evil.example.", want: true},
		{name: "trailing dot with port", patterns: []string{"evil.example"}, host: "evil.example.:8443", want: true},
		{name: "trailing dot in pattern", patterns: []string{"evil.example."}, host: "evil.example", want: true},
		{name: "case-insensitive host with port", patterns: []string{"evil.example"}, host: "EVIL.Example:8443", want: true},
		{name: "case-insensitive pattern", patterns: []string{"EVIL.example"}, host: "evil.example:8443", want: true},
		{name: "unicode case folding", patterns: []string{"müNSTer.example"}, host: "MÜNSTER.example", want: true},
		{name: "unicode pattern against punycode host with port", patterns: []string{"パイ.example"}, host: "xn--eckve.example:8443", want: true},
		{name: "punycode pattern against unicode host", patterns: []string{"xn--eckve.example"}, host: "パイ.example", want: true},

		{name: "ipv6 literal with port", patterns: []string{"[2001:db8::1]"}, host: "[2001:db8::1]:8443", want: true},
		{name: "ipv6 literal exact", patterns: []string{"[2001:db8::1]"}, host: "[2001:db8::1]", want: true},
		{name: "ipv6 other address", patterns: []string{"[2001:db8::1]"}, host: "[2001:db8::2]:8443", want: false},
		{name: "ipv4 with port", patterns: []string{"192.0.2.1"}, host: "192.0.2.1:8443", want: true},
		{name: "ipv4 other address with port", patterns: []string{"192.0.2.1"}, host: "192.0.2.10:8443", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, MatchesBlockList(tc.patterns, tc.host))
		})
	}
}

func TestMatchesAllowList(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		host     string
		want     bool
	}{
		{name: "exact", patterns: []string{"good.example"}, host: "good.example", want: true},
		{name: "subdomain", patterns: []string{"good.example"}, host: "sub.good.example", want: true},
		{name: "unrelated", patterns: []string{"good.example"}, host: "evil.example", want: false},
		{name: "empty host", patterns: []string{"good.example"}, host: "", want: false},
		{name: "empty pattern", patterns: []string{""}, host: "good.example", want: false},

		// 許可側はポートを落とさない (upstream isFederationAllowedHost と同じ)。
		{name: "non-default port is not admitted by bare entry", patterns: []string{"good.example"}, host: "good.example:8443", want: false},
		{name: "subdomain non-default port is not admitted", patterns: []string{"good.example"}, host: "sub.good.example:8443", want: false},
		{name: "port-specific entry admits that port", patterns: []string{"good.example:8443"}, host: "good.example:8443", want: true},
		{name: "port-specific entry does not admit other ports", patterns: []string{"good.example:8443"}, host: "good.example:9443", want: false},
		{name: "port-specific entry does not admit default port", patterns: []string{"good.example:8443"}, host: "good.example", want: false},

		{name: "trailing dot is the same authority", patterns: []string{"good.example"}, host: "good.example.", want: true},
		{name: "case-insensitive", patterns: []string{"Good.Example"}, host: "good.EXAMPLE", want: true},
		{name: "unicode pattern against punycode host", patterns: []string{"パイ.example"}, host: "xn--eckve.example", want: true},
		{name: "ipv6 literal port is significant", patterns: []string{"[2001:db8::1]"}, host: "[2001:db8::1]:8443", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, MatchesAllowList(tc.patterns, tc.host))
		})
	}
}

func TestSplitGatePort(t *testing.T) {
	cases := []struct {
		in, name, port string
	}{
		{"evil.example", "evil.example", ""},
		{"evil.example:8443", "evil.example", "8443"},
		{"evil.example:", "evil.example", ""},
		{"evil.example:https", "evil.example:https", ""},
		{"[::1]", "[::1]", ""},
		{"[::1]:8443", "[::1]", "8443"},
		{"[::1]:", "[::1]", ""},
		{"[::1]x", "[::1]x", ""},
		{"[::1", "[::1", ""},
		{"::1", "::1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, port := splitGatePort(tc.in)
			assert.Equal(t, tc.name, name)
			assert.Equal(t, tc.port, port)
		})
	}
}

func TestBareHost(t *testing.T) {
	for in, want := range map[string]string{
		"evil.example:8443": "evil.example",
		"Evil.Example.":     "evil.example",
		"evil.example":      "evil.example",
		"[::1]:8443":        "[::1]",
		"":                  "",
	} {
		assert.Equal(t, want, BareHost(in), in)
	}
}
