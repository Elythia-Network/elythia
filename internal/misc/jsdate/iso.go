package jsdate

import "time"

// ISOLayout is the layout of Date.prototype.toISOString: always 3 fractional
// digits and a trailing Z, in UTC. Upstream sends every time in a response
// this way.
const ISOLayout = "2006-01-02T15:04:05.000Z"

// ISOString formats t as Date.prototype.toISOString does. t is converted to
// UTC first.
func ISOString(t time.Time) string {
	return t.UTC().Format(ISOLayout)
}

// Time is a time.Time that marshals to JSON with ISOString, the format
// upstream sends (#4091). A struct that goes out in a response uses it in
// place of time.Time, whose encoding/json output is RFC3339Nano: the fraction
// loses its trailing zeros and the offset follows the value's location.
//
// It unmarshals any RFC 3339 time, so a struct that is also decoded (the
// status the backup service sends, for one) reads both formats. A nil *Time
// marshals to null.
type Time struct{ time.Time }

// NewTime wraps t.
func NewTime(t time.Time) Time { return Time{t} }

// NewTimePtr wraps *t, or returns nil for a nil t.
func NewTimePtr(t *time.Time) *Time {
	if t == nil {
		return nil
	}
	return &Time{*t}
}

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, len(ISOLayout)+2)
	b = append(b, '"')
	b = t.UTC().AppendFormat(b, ISOLayout)
	return append(b, '"'), nil
}
