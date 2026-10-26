package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// listCursor is a body of the list endpoints with every integer param
// written as a fraction or an exponent.
const listCursor = `{"limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`

// TestAdminHandlers_IntegerParamsAcceptFractionForms pins that the integer
// params of admin/ad/*, admin/avatar-decorations/list, admin/invite/*,
// admin/promo/create and admin/show-moderation-logs accept every JSON number
// with an integer value (10.0, 1e1), as upstream's ajv "type": "integer"
// does, and read it as that integer (#3854).
func TestAdminHandlers_IntegerParamsAcceptFractionForms(t *testing.T) {
	t.Run("admin/ad/create", func(t *testing.T) {
		h, repo := setupAdHandler(t)
		rec := doPost(h.AdCreate, `{"url":"https://x","imageUrl":"https://y","memo":"","place":"square","priority":"middle",`+
			`"ratio":2.0,"dayOfWeek":3e0,"expiresAt":4102444800000.0,"startsAt":1e3}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got model.Ad
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		ad := repo.Ads[got.ID]
		require.NotNil(t, ad)
		assert.Equal(t, 2, ad.Ratio)
		assert.Equal(t, 3, ad.DayOfWeek)
		assert.Equal(t, int64(4102444800000), ad.ExpiresAt.UnixMilli())
		assert.Equal(t, int64(1000), ad.StartsAt.UnixMilli())
	})
	t.Run("admin/ad/update", func(t *testing.T) {
		h, repo := setupAdHandler(t, &model.Ad{ID: "ad1", URL: "https://x", Place: "square"})
		rec := doPost(h.AdUpdate, `{"id":"ad1","ratio":2.0,"dayOfWeek":3e0,"expiresAt":4102444800000.0,"startsAt":1e3}`, adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		ad := repo.Ads["ad1"]
		assert.Equal(t, 2, ad.Ratio)
		assert.Equal(t, 3, ad.DayOfWeek)
		assert.Equal(t, int64(4102444800000), ad.ExpiresAt.UnixMilli())
		assert.Equal(t, int64(1000), ad.StartsAt.UnixMilli())
	})
	t.Run("admin/ad/list", func(t *testing.T) {
		h, _ := setupAdHandler(t)
		rec := doPost(h.AdList, listCursor, adminUser)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Run("admin/avatar-decorations/list", func(t *testing.T) {
		h, _ := setupAvatarDecorationHandler(t)
		rec := doPost(h.AvatarDecorationsList, listCursor, adminUser)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Run("admin/invite/create", func(t *testing.T) {
		h, repo := setupInviteHandler(t)
		rec := doPost(h.InviteCreate, `{"count":2.0}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Len(t, repo.Tickets, 2)
	})
	t.Run("admin/invite/list", func(t *testing.T) {
		h, _ := setupInviteHandler(t,
			&model.RegistrationTicket{ID: "t1", Code: "c1"},
			&model.RegistrationTicket{ID: "t2", Code: "c2"},
		)
		rec := doPost(h.InviteList, `{"limit":1e1,"offset":1.0}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		assert.Len(t, rows, 1, "offset 1.0 skips one ticket")
	})
	t.Run("admin/promo/create", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		promo := &stubPromoRepo{}
		h.SetPromoNoteRepo(promo)
		h.SetNoteFinder(&stubNoteFinder{note: &model.Note{ID: "n1", UserID: "authorU", Visibility: model.NoteVisibilityPublic}})
		rec := doPost(h.PromoCreate, `{"noteId":"n1","expiresAt":4102444800000.0}`, adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		require.NotNil(t, promo.created)
		assert.Equal(t, int64(4102444800000), promo.created.ExpiresAt.UnixMilli())
	})
	t.Run("admin/show-moderation-logs", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		gen, _ := id.NewGenerator("aidx")
		h.SetModLogService(moderationlog.New(testutil.NewMockModerationLogRepository(), gen, nil))
		rec := doPost(h.ShowModerationLogs, listCursor, adminUser)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// 本家の admin/ad/list の paramDef に offset は無い。本家は読まないので、型の違う値も
// 通し、件数も飛ばさない (#3854)。
func TestAdList_OffsetIsNotRead(t *testing.T) {
	now := time.Now()
	ads := make([]*model.Ad, 3)
	for i := range ads {
		ads[i] = &model.Ad{
			ID: fmt.Sprintf("a%02d", i), URL: "u", ImageURL: "i", Place: "square", Priority: "middle", Ratio: 1,
			StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		}
	}
	h, _ := setupAdHandler(t, ads...)
	for _, body := range []string{`{"offset":2}`, `{"offset":"x"}`} {
		rec := doPost(h.AdList, body, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, body)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		assert.Len(t, rows, 3, body)
	}
}

// 同梱の frontend は期限を datetime-local の値 (秒もオフセットも無い) で送る。本家の
// Date.parse と同じく受けて、サーバーの時刻帯の時刻として保存する (#3854)。
func TestInviteCreate_AcceptsDateParseForms(t *testing.T) {
	h, repo := setupInviteHandler(t)
	rec := doPost(h.InviteCreate, `{"expiresAt":"2099-10-27T12:34"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, repo.Tickets, 1)
	for _, tkt := range repo.Tickets {
		require.NotNil(t, tkt.ExpiresAt)
		assert.True(t, time.Date(2099, 10, 27, 12, 34, 0, 0, time.Local).Equal(*tkt.ExpiresAt), tkt.ExpiresAt.String())
	}
	// 本家も Date.parse が NaN になる値は INVALID_DATE_TIME にする。
	rec = doPost(h.InviteCreate, `{"expiresAt":"hello"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_DATE_TIME")
}
