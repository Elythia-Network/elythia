package charts

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
)

// castFailingBinder stands in for the server's apiBinder rejecting a GET
// query value it cannot cast (it marks the request and fails Bind).
type castFailingBinder struct{}

func (castFailingBinder) Bind(_ any, c echo.Context) error {
	apierr.MarkCastFailure(c, "limit", "integer")
	return errors.New("cannot cast")
}

// TestChartBindFailure_CastMark pins that a chart handler answers a Bind
// failure the binder marked as a cast failure with upstream's cast error
// (0b5f1631, info {param, reason}) on both the instance-wide and the
// per-user path, instead of its own "#" "Invalid request body." info.
func TestChartBindFailure_CastMark(t *testing.T) {
	h, _ := newHandlerWithAllCharts(t)
	for name, fn := range map[string]echo.HandlerFunc{"notes": h.Notes, "user/notes": h.UserNotes} {
		t.Run(name, func(t *testing.T) {
			e := echo.New()
			e.Binder = castFailingBinder{}
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/?span=day&limit=abc", nil), rec)
			require.NoError(t, fn(c))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			errObj := out["error"].(map[string]any)
			assert.Equal(t, apierr.UUIDInvalidParamCast, errObj["id"])
			assert.Equal(t, map[string]any{"param": "limit", "reason": "cannot cast to integer"}, errObj["info"])
		})
	}
}
