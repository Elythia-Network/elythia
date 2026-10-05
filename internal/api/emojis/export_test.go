package emojis

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coretransfer "github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

type stubEnqueuer struct {
	got  []queue.ExportPayload
	err  error
	call int
}

func (s *stubEnqueuer) EnqueueExport(p queue.ExportPayload) error {
	s.call++
	s.got = append(s.got, p)
	return s.err
}

func callExport(t *testing.T, h *Handler, user *model.User) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	require.NoError(t, h.ExportCustomEmojis(c))
	return rec
}

func TestExportCustomEmojis(t *testing.T) {
	t.Run("queues the export for the caller", func(t *testing.T) {
		q := &stubEnqueuer{}
		h := NewHandler(nil)
		h.SetExportEnqueuer(q)

		rec := callExport(t, h, &model.User{ID: "u1"})

		assert.Equal(t, http.StatusNoContent, rec.Code)
		require.Len(t, q.got, 1)
		assert.Equal(t, "u1", q.got[0].UserID)
		assert.Equal(t, coretransfer.ExportCustomEmojis, q.got[0].Type)
	})

	t.Run("enqueue failure still returns 204", func(t *testing.T) {
		// **upstream も await しない。** `createExportCustomEmojisJob` を投げっぱなしで
		// 即 204 を返し、結果はドライブに出るファイルで確認する作り。ここを 500 に
		// すると挙動がずれる。
		q := &stubEnqueuer{err: errors.New("boom")}
		h := NewHandler(nil)
		h.SetExportEnqueuer(q)

		rec := callExport(t, h, &model.User{ID: "u1"})

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, 1, q.call)
	})

	t.Run("anonymous caller enqueues nothing", func(t *testing.T) {
		// route には RequireAuth が付くが、handler 単体でも user 無しで
		// job を作らないことを固定する (誰の export か決まらない)。
		q := &stubEnqueuer{}
		h := NewHandler(nil)
		h.SetExportEnqueuer(q)

		rec := callExport(t, h, nil)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, 0, q.call)
	})

	t.Run("unwired queue returns 204 without panicking", func(t *testing.T) {
		rec := callExport(t, NewHandler(nil), &model.User{ID: "u1"})
		assert.Equal(t, http.StatusNoContent, rec.Code)
	})
}
