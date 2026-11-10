package federation

import (
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 連合の相手が中身を決める入力を読む関数の fuzz (#4116)。

const fuzzInputMax = 64 << 10

// fuzzDeadline bounds one call. The seeds also run in the required test under
// -race, so it leaves a wide margin.
const fuzzDeadline = 5 * time.Second

func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(fuzzDeadline):
		t.Fatalf("did not return within %s", fuzzDeadline)
	}
}

// FuzzNormalizeActivity checks that the inbox reads the same identity fields
// each time, and the same ones again from the normalized body. The normalized
// body (act.raw) is what the processing hands on, for example inside the
// Accept of a Follow, where the receiver normalizes it again.
func FuzzNormalizeActivity(f *testing.F) {
	for _, s := range []string{
		`{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/a/1","type":"Follow","actor":"https://remote.example/users/a","object":"https://local.example/users/b"}`,
		`[{"id":"https://remote.example/a/1","type":"Like","actor":{"id":"https://remote.example/users/a"}}]`,
		`[{"type":"Like"},{"type":"Undo"}]`,
		`{"type":["Delete"],"as:actor":{"@id":"https://remote.example/users/a"},"object":{"type":"Tombstone"}}`,
		`{"actor":"https://a.example/u","https://www.w3.org/ns/activitystreams#actor":"https://b.example/u"}`,
		`{"actor":{"id":7},"id":["x"],"published":12345}`,
		`[]`, `null`, `{`, ``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) > fuzzInputMax {
			t.Skip()
		}
		var a, b NormalizedActivity
		within(t, func() {
			a = NormalizeActivity(in)
			b = NormalizeActivity(in)
		})
		if a.Fields() != b.Fields() || (a.err == nil) != (b.err == nil) {
			t.Fatalf("not deterministic: %+v / %+v", a.Fields(), b.Fields())
		}
		// actor の読めない活動は処理が捨てるので、正規化した本文が外へ出ることは無い。
		// 2 重の配列 ([[{...}]]) は 1 回目に外側だけを剥がして actor が読めず、読み直すと
		// もう 1 段剥がして読めるので、比べると誤って落ちる
		if a.err != nil || a.act.raw == nil || a.Fields().Actor == "" {
			return
		}
		if again := NormalizeActivity(a.act.raw); again.Fields() != a.Fields() {
			t.Fatalf("the normalized body reads %+v, the body %+v", again.Fields(), a.Fields())
		}
	})
}

// FuzzNormalizeGateHost checks that the host the federation gates compare is
// lower case and does not change when normalized again. A host that changed
// would let one remote be blocked under one form and reached under another.
func FuzzNormalizeGateHost(f *testing.F) {
	for _, s := range []string{
		"https://Remote.Example/users/a", "https://remote.example:443/", "http://remote.example:80/",
		"https://remote.example:8443/inbox", "https://例え.テスト/", "https://xn--r8jz45g.xn--zckzah/",
		"https://[::1]:8080/", "https://[::ffff:1.2.3.4]/", "https://u:p@remote.example/", "https://REMOTE.EXAMPLE./",
		"https://%45.example/", "https://remote.example:0/", "remote.example", "//remote.example/", "https://", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > fuzzInputMax {
			t.Skip()
		}
		var host string
		within(t, func() { host = NormalizeGateHost(in) })
		if host == "" {
			return
		}
		if host != strings.ToLower(host) {
			t.Fatalf("%q -> %q is not lower case", in, host)
		}
		u, err := url.Parse(in)
		if err != nil {
			t.Fatalf("%q gave %q but does not parse: %v", in, host, err)
		}
		// 既定のポートを剥がすかはスキームで決まるので、同じスキームで組み直す
		prefix := "//"
		if u.Scheme != "" {
			prefix = u.Scheme + "://"
		}
		again := NormalizeGateHost(prefix + host + "/")
		if again != host {
			t.Fatalf("not idempotent: %q -> %q -> %q", in, host, again)
		}
	})
}

// FuzzMatchSuspendedSoftware checks that matching the software a remote
// reports (name and version, both chosen by the remote) against the
// administrator's ranges never panics, ends, and gives the same answer each
// time. The ranges are compiled once and cached, so the second answer comes
// from the cache.
func FuzzMatchSuspendedSoftware(f *testing.F) {
	for _, s := range [][3]string{
		{"misskey", "2024.11.1", "<2025.1.0"},
		{"Misskey", "2024.11.1-cherrypick-4.14.0", "/cherrypick/i"},
		{"mastodon", "4.3.0+glitch", ">=4.0.0 <4.3.1 || 5.x"},
		{"pleroma", "2.7", "*"},
		{"misskey", "1.0.0-alpha.1", "^1.0.0-0"},
		{"misskey", "1.2.3", "/(a+)+$/"},
		{"misskey", "1.2.3", "/[/"},
		{"misskey", "", "1.2.3 - 2.3.4"},
		{"", "1.0.0", "~1"},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, name, version, rng string) {
		if len(name)+len(version)+len(rng) > fuzzInputMax || !utf8.ValidString(rng) {
			t.Skip()
		}
		entries := []SuspendedSoftwareEntry{{Software: name, VersionRange: rng}}
		var first, second bool
		within(t, func() {
			first = MatchSuspendedSoftware(&name, &version, entries)
			second = MatchSuspendedSoftware(&name, &version, entries)
		})
		if first != second {
			t.Fatalf("%q %q in %q: %v then %v", name, version, rng, first, second)
		}
		if !first {
			return
		}
		// 名前の違う相手には当たらない
		other := name + "-other"
		if MatchSuspendedSoftware(&other, &version, entries) {
			t.Fatalf("%q matched the entry for %q", other, name)
		}
	})
}
