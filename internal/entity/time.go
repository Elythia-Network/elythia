package entity

import (
	"time"

	"github.com/elythia-network/elythia/internal/misc/jsdate"
)

// ISOMillis formats t as a Misskey-compatible ISO8601 millisecond timestamp,
// mirroring upstream's Date.toISOString() (jsdate.ISOString).
func ISOMillis(t time.Time) string {
	return jsdate.ISOString(t)
}

// ISOMillisPtr returns nil for a nil pointer, otherwise the ISOMillis string,
// mirroring upstream's `x ? x.toISOString() : null`. The return type is `any`
// so callers can drop it straight into a map[string]any response.
func ISOMillisPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ISOMillis(*t)
}
