package entity

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// PackSignin converts a model.Signin into the map shape returned by the
// `signin` WebSocket event on the `main` channel. Matches Misskey's
// SigninEntityService.pack (id / createdAt / ip / headers / success).
// createdAt is derived from the aidx-encoded signin ID when an idGen is
// supplied. Returns nil when s is nil so callers can assign the result
// directly to a nilable field.
func PackSignin(s *model.Signin, idGens ...id.Generator) map[string]any {
	if s == nil {
		return nil
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	out := map[string]any{
		"id":      s.ID,
		"ip":      s.IP,
		"success": s.Success,
	}
	// headersはjsonbとして保存されているのでそのままRawMessageにして
	// frontendに渡す(文字列化を避ける)。
	if len(s.Headers) > 0 {
		out["headers"] = json.RawMessage(s.Headers)
	} else {
		out["headers"] = map[string]any{}
	}
	if len(idGens) > 0 && idGens[0] != nil {
		if t, err := idGens[0].ParseTime(s.ID); err == nil {
			out["createdAt"] = t.UTC().Format(tsFormat)
		}
	}
	return out
}
