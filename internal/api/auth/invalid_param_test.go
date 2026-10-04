package auth

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/model"
)

// TestAuth_MissingParamsUseAjvID pins upstream's ajv envelope (message
// "Invalid param.", id 3d81ceae) for the auth/session, auth/accept and
// miauth/gen-token endpoints when a required param is missing; they used to
// answer with mk-go's own id ed1d7571-….
func TestAuth_MissingParamsUseAjvID(t *testing.T) {
	h, _ := newTestHandler()
	user := &model.User{ID: "u1"}
	for name, fn := range map[string]func(echo.Context) error{
		"auth/session/generate": h.SessionGenerate,
		"auth/session/show":     h.SessionShow,
		"auth/session/userkey":  h.SessionUserkey,
		"auth/accept":           h.Accept,
		"miauth/gen-token":      h.GenToken,
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(fn, `{}`, user)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			errObj := out["error"].(map[string]any)
			assert.Equal(t, "INVALID_PARAM", errObj["code"])
			assert.Equal(t, "Invalid param.", errObj["message"])
			assert.Equal(t, apierr.UUIDInvalidParam, errObj["id"])
		})
	}
}
