package bubblegame

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/apierr"
)

// TestBubbleGame_InvalidParamShape pins upstream's ajv envelope for
// bubble-game/register and bubble-game/ranking (endpoint-base.ts: message
// "Invalid param.", id 3d81ceae) instead of mk-go's former id ed1d7571-…,
// with ajv's info where the violated keyword is known.
func TestBubbleGame_InvalidParamShape(t *testing.T) {
	h, _ := newTestHandler()
	cases := []struct {
		name     string
		call     func() (int, []byte)
		wantInfo any
	}{
		{"register without params", func() (int, []byte) {
			r := post(h.Register, `{}`, u1)
			return r.Code, r.Body.Bytes()
		}, nil},
		{"register negative score", func() (int, []byte) {
			r := post(h.Register, fmt.Sprintf(`{"score":-1,"seed":"%s","logs":[],"gameMode":"normal","gameVersion":1}`, validSeed()), u1)
			return r.Code, r.Body.Bytes()
		}, map[string]any{"param": "#/properties/score/minimum", "reason": "must be >= 0"}},
		{"ranking without gameMode", func() (int, []byte) {
			r := post(h.Ranking, `{}`, nil)
			return r.Code, r.Body.Bytes()
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := tc.call()
			require.Equal(t, http.StatusBadRequest, code, string(body))
			var out map[string]any
			require.NoError(t, json.Unmarshal(body, &out))
			errObj := out["error"].(map[string]any)
			assert.Equal(t, "INVALID_PARAM", errObj["code"])
			assert.Equal(t, "Invalid param.", errObj["message"])
			assert.Equal(t, apierr.UUIDInvalidParam, errObj["id"])
			assert.Equal(t, tc.wantInfo, errObj["info"])
		})
	}
}
