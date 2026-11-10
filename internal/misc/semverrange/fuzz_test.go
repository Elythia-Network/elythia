package semverrange

import (
	"testing"
	"time"
)

// FuzzSatisfies checks that matching a version a remote reports against an
// administrator's range never panics, ends, and gives the same answer from
// the parsed range as from the cached one (#4116). The range is turned into
// regular expressions ported from node-semver, so a malformed one must not
// make them run long.
func FuzzSatisfies(f *testing.F) {
	for _, s := range [][2]string{
		{"2024.11.1", "<2025.1.0"}, {"1.2.3-alpha.1+b", "^1.2.3-0"}, {"4.3.0", ">=4.0.0 <4.3.1 || 5.x"},
		{"1.2.3", "1.2.3 - 2.3.4"}, {"1.2.3", "~1"}, {"1.2.3", "*"}, {"v1.2.3", "=1.2.3"},
		{"1.2.3", ">=1.2.3-0 <=1.2.3"}, {"01.2.3", "1.x.x"}, {"9007199254740992.0.0", ">0"},
		{"1.2.3", "||"}, {"", "*"}, {"1.2.3", ""}, {"1.2", "1.2"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, version, rng string) {
		if len(version)+len(rng) > 4<<10 {
			t.Skip()
		}
		var uncached, first, second bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			set, ok := parseRange(rng)
			if ok && version != "" {
				if v, ok := parseSemVer(version); ok {
					for _, comps := range set {
						if testSet(comps, v) {
							uncached = true
							break
						}
					}
				}
			}
			first = Satisfies(version, rng)
			second = Satisfies(version, rng)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%q in %q did not return", version, rng)
		}
		if first != second || first != uncached {
			t.Fatalf("%q in %q: parsed %v, then %v and %v", version, rng, uncached, first, second)
		}
	})
}
