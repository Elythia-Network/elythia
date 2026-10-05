package promo

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
)

// stubPromoRepo records what was marked read and can fail on demand.
type stubPromoRepo struct {
	got  []*model.PromoRead
	err  error
	call int
}

func (s *stubPromoRepo) MarkRead(p *model.PromoRead) error {
	s.call++
	s.got = append(s.got, p)
	return s.err
}

func (s *stubPromoRepo) IsRead(string, string) bool { return false }

func newHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository, *stubPromoRepo) {
	t.Helper()
	notes := testutil.NewMockNoteRepository()
	promo := &stubPromoRepo{}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return NewHandler(notes, promo, idGen), notes, promo
}

func callRead(t *testing.T, h *Handler, body string, user *model.User) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	require.NoError(t, h.Read(c))
	return rec
}

func TestRead(t *testing.T) {
	me := &model.User{ID: "u1"}

	t.Run("marks the note read and returns 204", func(t *testing.T) {
		h, notes, promo := newHandler(t)
		notes.Notes["n1"] = &model.Note{ID: "n1"}

		rec := callRead(t, h, `{"noteId":"n1"}`, me)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		require.Len(t, promo.got, 1)
		assert.Equal(t, "u1", promo.got[0].UserID)
		assert.Equal(t, "n1", promo.got[0].NoteID)
		assert.NotEmpty(t, promo.got[0].ID, "id を採番していない")
	})

	t.Run("unknown note returns the endpoint-specific NO_SUCH_NOTE id", func(t *testing.T) {
		// **汎用の UUIDNoSuchNote は upstream `notes/show` の id。** upstream は
		// NO_SUCH_NOTE を定義する 21 endpoint すべてに別 id を割り当てるので、
		// error.id で分岐する drop-in クライアントが誤分類する (#2784)。
		h, _, promo := newHandler(t)

		rec := callRead(t, h, `{"noteId":"missing"}`, me)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "NO_SUCH_NOTE")
		assert.Contains(t, rec.Body.String(), apierr.UUIDNoSuchNotePromoRead)
		assert.NotContains(t, rec.Body.String(), apierr.UUIDNoSuchNote)
		assert.Equal(t, 0, promo.call)
	})

	t.Run("a real DB failure is a 500, not a silent 204", func(t *testing.T) {
		// **捨てると「既読にならないのに 204」という気付けない壊れ方をする。**
		// 重複は OnConflict{DoNothing} が握るので、ここに来るのは本物の障害だけ。
		h, notes, promo := newHandler(t)
		notes.Notes["n1"] = &model.Note{ID: "n1"}
		promo.err = errors.New("boom")

		rec := callRead(t, h, `{"noteId":"n1"}`, me)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, 1, promo.call)
	})

	t.Run("missing noteId is a 400", func(t *testing.T) {
		h, _, promo := newHandler(t)

		rec := callRead(t, h, `{}`, me)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		assert.Equal(t, 0, promo.call)
	})

	t.Run("malformed body is a 400", func(t *testing.T) {
		h, _, _ := newHandler(t)

		rec := callRead(t, h, `{`, me)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("anonymous caller is a 401", func(t *testing.T) {
		// route には RequireAuth が付くが、handler 単体でも誰の既読か決まらない
		// 状態で書き込まないことを固定する。
		h, notes, promo := newHandler(t)
		notes.Notes["n1"] = &model.Note{ID: "n1"}

		rec := callRead(t, h, `{"noteId":"n1"}`, nil)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, 0, promo.call)
	})

	t.Run("unwired repos return a 500 instead of panicking", func(t *testing.T) {
		idGen, err := id.NewGenerator("aidx")
		require.NoError(t, err)

		rec := callRead(t, NewHandler(nil, nil, idGen), `{"noteId":"n1"}`, me)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}
