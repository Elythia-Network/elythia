package testutil

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

type recordingChecker struct{ errs []string }

func (r *recordingChecker) Helper() {}
func (r *recordingChecker) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func TestAssertUpstreamDates(t *testing.T) {
	cases := []struct {
		name string
		body string
		keys []string
		errs int
	}{
		{"upstream format at any depth", `{"at":"2026-11-09T03:34:56.789Z","rows":[{"createdAt":"2026-01-02T03:04:05.000Z","next":null}]}`, []string{"at", "createdAt"}, 0},
		{"null is allowed when another value is set", `{"rows":[{"at":null},{"at":"2026-01-02T03:04:05.000Z"}]}`, []string{"at"}, 0},
		{"RFC3339Nano drops the trailing zeros", `{"at":"2026-01-02T03:04:05Z"}`, []string{"at"}, 1},
		{"an offset other than Z", `{"at":"2026-01-02T12:04:05.123+09:00"}`, []string{"at"}, 1},
		{"nanoseconds", `{"rows":[{"at":"2026-01-02T03:04:05.123456789Z"}]}`, []string{"at"}, 1},
		{"a number is not a date", `{"at":1700000000000}`, []string{"at"}, 2},
		{"a key that never has a value", `{"at":null}`, []string{"at"}, 1},
		{"a key that is missing", `{"other":"2026-01-02T03:04:05.000Z"}`, []string{"at"}, 1},
		{"not JSON", `{`, []string{"at"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingChecker{}
			AssertUpstreamDates(r, []byte(tc.body), tc.keys...)
			assert.Len(t, r.errs, tc.errs, r.errs)
		})
	}
}
