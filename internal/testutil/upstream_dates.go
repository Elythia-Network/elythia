package testutil

import (
	"encoding/json"
	"regexp"
	"sort"
)

// upstreamDate is the format of Date.prototype.toISOString, which upstream
// sends for every time in a response (jsdate.ISOLayout).
var upstreamDate = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

// DateChecker is the part of *testing.T that AssertUpstreamDates uses.
type DateChecker interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertUpstreamDates checks that every value of the named keys, at any depth
// of the JSON body, is null or a time in upstream's format (#4091). Each key
// must appear at least once with a string value, so that a key that is
// renamed or no longer set fails instead of passing with nothing to check.
//
// Go の encoding/json は time.Time を RFC3339Nano で書く (末尾の 0 が落ち、
// オフセットは値のロケーションに従う)。名前で見るのは、応答の文字列のうち
// どれが日時かを本文だけからは決められないため。
func AssertUpstreamDates(t DateChecker, body []byte, keys ...string) {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Errorf("the body is not JSON: %v: %s", err, body)
		return
	}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			for k, val := range x {
				if want[k] {
					switch s := val.(type) {
					case string:
						seen[k] = true
						if !upstreamDate.MatchString(s) {
							t.Errorf("%s = %q, want the format of Date.toISOString (2006-01-02T15:04:05.000Z)", k, s)
						}
					case nil:
					default:
						t.Errorf("%s = %v, want a string or null", k, val)
					}
				}
				walk(val)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	var missing []string
	for _, k := range keys {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("no string value for %v in %s", missing, body)
	}
}
