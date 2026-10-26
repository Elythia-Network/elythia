package admin_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
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

// TestAdminDriveEmojiLists_IntegerParamsAcceptFractionForms pins that the
// limit, sinceDate and untilDate of admin/drive/files, admin/emoji/list and
// admin/emoji/list-remote accept every JSON number with an integer value
// (10.0, 1e1), as upstream's ajv "type": "integer" does, and read it as that
// integer (#3857).
func TestAdminDriveEmojiLists_IntegerParamsAcceptFractionForms(t *testing.T) {
	// sinceDate (2e3) より前の id と、後の id を 2 件ずつ置く。limit (1.0) と
	// sinceDate を読めば、後の 1 件だけが返る。
	cut := id.AidxCutoffPrefix(time.UnixMilli(2000))
	ids := []string{"0", "00", cut + "a", cut + "b"}
	const body = `{"limit":1.0,"sinceDate":2e3}`
	check := func(t *testing.T, h func() *httptest.ResponseRecorder, hc *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusOK, hc.Code, hc.Body.String())
		rec := h()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		require.Len(t, rows, 1)
		assert.True(t, strings.HasPrefix(rows[0]["id"].(string), cut), rows[0]["id"])
	}
	t.Run("admin/drive/files", func(t *testing.T) {
		u := "u1"
		var files []*model.DriveFile
		for _, i := range ids {
			files = append(files, &model.DriveFile{ID: i, UserID: &u, Type: "image/png"})
		}
		h, _ := setupDriveFileHandler(t, files...)
		check(t, func() *httptest.ResponseRecorder { return doPost(h.DriveFiles, body, adminUser) }, doPost(h.DriveFiles, listCursor, adminUser))
	})
	t.Run("admin/emoji/list", func(t *testing.T) {
		var emojis []*model.Emoji
		for _, i := range ids {
			emojis = append(emojis, &model.Emoji{ID: i, Name: "e" + i})
		}
		h, _ := setupEmojiHandler(t, emojis...)
		check(t, func() *httptest.ResponseRecorder { return doPost(h.EmojiList, body, adminUser) }, doPost(h.EmojiList, listCursor, adminUser))
	})
	t.Run("admin/emoji/list-remote", func(t *testing.T) {
		host := "remote.example"
		var emojis []*model.Emoji
		for _, i := range ids {
			emojis = append(emojis, &model.Emoji{ID: i, Name: "e" + i, Host: &host})
		}
		h, _ := setupEmojiHandler(t, emojis...)
		check(t, func() *httptest.ResponseRecorder { return doPost(h.EmojiListRemote, body, adminUser) }, doPost(h.EmojiListRemote, listCursor, adminUser))
	})
}

// 本家の admin/emoji/list・admin/emoji/list-remote の paramDef に offset は無い。
// 本家は読まないので、型の違う値も通し、件数も飛ばさない (#3857)。
func TestEmojiLists_OffsetIsNotRead(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "l1", Name: "l1"}, &model.Emoji{ID: "l2", Name: "l2"}, &model.Emoji{ID: "l3", Name: "l3"},
		&model.Emoji{ID: "r1", Name: "r1", Host: &host}, &model.Emoji{ID: "r2", Name: "r2", Host: &host}, &model.Emoji{ID: "r3", Name: "r3", Host: &host},
	)
	for name, handle := range map[string]func(echo.Context) error{"admin/emoji/list": h.EmojiList, "admin/emoji/list-remote": h.EmojiListRemote} {
		for _, body := range []string{`{"offset":2}`, `{"offset":"x"}`} {
			rec := doPost(handle, body, adminUser)
			require.Equal(t, http.StatusOK, rec.Code, name+" "+body)
			var rows []map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
			assert.Len(t, rows, 3, name+" "+body)
		}
	}
}

// 本家の admin/drive/show-file は 'fileId' in ps で分岐する。fileId のキーがあれば
// url は見ず、文字列でない fileId には当たるファイルが無い。fileId の null は、
// Elythia では無いものとして url で引く。空の url は NO_SUCH_FILE になる。もう片方の
// 値の型が違っても 400 にしない (#3857)。
func TestDriveShowFile_BranchesOnKeyPresence(t *testing.T) {
	u := "u1"
	h, _ := setupDriveFileHandler(t, &model.DriveFile{ID: "d1", UserID: &u, Name: "a.png", Type: "image/png"})
	for body, want := range map[string]string{
		`{"url":""}`:                   "NO_SUCH_FILE",
		`{"fileId":"","url":"x"}`:      "NO_SUCH_FILE",
		`{"fileId":"d1","url":""}`:     "",
		`{"fileId":"d1","url":123}`:    "",
		`{"fileId":123,"url":"x"}`:     "NO_SUCH_FILE",
		`{"fileId":{"a":1},"url":"x"}`: "NO_SUCH_FILE",
		`{"fileId":null,"url":""}`:     "NO_SUCH_FILE",
		`{"fileId":null}`:              "INVALID_PARAM",
		`{"url":123}`:                  "INVALID_PARAM",
		`{}`:                           "INVALID_PARAM",
	} {
		rec := doPost(h.DriveShowFile, body, adminUser)
		if want == "" {
			assert.Equal(t, http.StatusOK, rec.Code, body+": "+rec.Body.String())
			continue
		}
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), `"code":"`+want+`"`, body)
	}
}

// url で引く経路を実 DB で確かめる (#3857)。空の url では引かず、thumbnailUrl などを
// 空で持つ行があっても、その行を返さない。fileId のキーがあれば、空や文字列でない
// 値でも url では引かない。fileId の null は無いものとして url で引く。
func TestDriveShowFile_URLLookup(t *testing.T) {
	if integrationDB == nil {
		t.Skip("integrationDB unavailable")
	}
	const emptyID, namedID, namedURL = "showfileemptyurl1", "showfilenamedurl1", "showfile-named-url"
	empty := ""
	require.NoError(t, integrationDB.Create(&model.DriveFile{
		ID: emptyID, Name: "a.png", Type: "image/png", URL: "", ThumbnailURL: &empty, WebpublicURL: &empty,
	}).Error)
	require.NoError(t, integrationDB.Create(&model.DriveFile{ID: namedID, Name: "b.png", Type: "image/png", URL: namedURL}).Error)
	t.Cleanup(func() {
		require.NoError(t, integrationDB.Exec(`DELETE FROM drive_file WHERE id IN (?, ?)`, emptyID, namedID).Error)
	})
	h, _ := setupDriveFileHandler(t)
	h.SetAdminDB(integrationDB)
	for body, want := range map[string]string{
		`{"url":""}`:                               "NO_SUCH_FILE",
		`{"fileId":"","url":"` + namedURL + `"}`:   "NO_SUCH_FILE",
		`{"url":"` + namedURL + `"}`:               "",
		`{"fileId":null,"url":"` + namedURL + `"}`: "",
		`{"fileId":123,"url":"` + namedURL + `"}`:  "NO_SUCH_FILE",
	} {
		rec := doPost(h.DriveShowFile, body, adminUser)
		if want == "" {
			require.Equal(t, http.StatusOK, rec.Code, body+": "+rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"id":"`+namedID+`"`, body)
			continue
		}
		assert.Equal(t, http.StatusBadRequest, rec.Code, body+": "+rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"code":"`+want+`"`, body)
	}
}

// admin/emoji/list の category は本家の paramDef に無い Elythia 独自の引数。本家は
// 無視するので、文字列でない値も 400 にしない (#3857)。
func TestEmojiList_NonStringCategoryIsIgnored(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "l1", Name: "l1"}, &model.Emoji{ID: "l2", Name: "l2"})
	for _, body := range []string{`{"category":123}`, `{"category":{"a":1}}`, `{"category":null}`} {
		rec := doPost(h.EmojiList, body, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, body+": "+rec.Body.String())
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		assert.Len(t, rows, 2, body)
	}
}

// 本家の admin/emoji/update は id の値を CustomEmojiService.update へ渡し、
// `data.id ? getEmojiById : getEmojiByName` で真偽値として見る。JavaScript で偽に
// なる値 (null・空文字・0・false) なら name で引き、文字列でない真の値は当たる
// 絵文字が無いので NO_SUCH_EMOJI になる (#3857)。
func TestEmojiUpdate_IDShapes(t *testing.T) {
	for body, wantCode := range map[string]int{
		`{"id":123,"name":"hello","category":"c"}`:     http.StatusNotFound,
		`{"id":true,"name":"hello","category":"c"}`:    http.StatusNotFound,
		`{"id":[],"name":"hello","category":"c"}`:      http.StatusNotFound,
		`{"id":{},"name":"hello","category":"c"}`:      http.StatusNotFound,
		`{"id":-1,"name":"hello","category":"c"}`:      http.StatusNotFound,
		`{"id":1e400,"name":"hello","category":"c"}`:   http.StatusNotFound,
		`{"id":0,"name":"hello","category":"c"}`:       http.StatusNoContent,
		`{"id":-0,"name":"hello","category":"c"}`:      http.StatusNoContent,
		`{"id":0.0,"name":"hello","category":"c"}`:     http.StatusNoContent,
		`{"id":-0.0,"name":"hello","category":"c"}`:    http.StatusNoContent,
		`{"id":0e5,"name":"hello","category":"c"}`:     http.StatusNoContent,
		`{"id":1e-400,"name":"hello","category":"c"}`:  http.StatusNoContent,
		`{"id":false,"name":"hello","category":"c"}`:   http.StatusNoContent,
		`{"id":{"a":1},"name":"hello","category":"c"}`: http.StatusNotFound,
		`{"id":null,"name":"hello","category":"c"}`:    http.StatusNoContent,
		`{"id":"","name":"hello","category":"c"}`:      http.StatusNoContent,
	} {
		h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "hello"})
		rec := doPost(h.EmojiUpdate, body, adminUser)
		require.Equal(t, wantCode, rec.Code, body+": "+rec.Body.String())
		e := findEmojiByID(t, repo, "e1")
		if wantCode == http.StatusNotFound {
			assert.Contains(t, rec.Body.String(), `"code":"NO_SUCH_EMOJI"`, body)
			assert.Nil(t, e.Category, body)
			continue
		}
		require.NotNil(t, e.Category, body)
		assert.Equal(t, "c", *e.Category, body)
	}
}

// 文字列でない id の NO_SUCH_EMOJI は、解決した id が空なので、送られた生の値も
// 警告に残す (#3857)。
func TestEmojiUpdate_LogsRawNonStringID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "hello"})
	rec := doPost(h.EmojiUpdate, `{"id":123,"name":"hello"}`, adminUser)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, buf.String(), "rawId=123")
}
