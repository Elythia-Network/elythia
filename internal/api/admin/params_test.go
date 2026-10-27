package admin_test

import (
	"bytes"
	"context"
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

	"github.com/elythia-network/elythia/internal/bgwork"
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

// TestRolesAbuseLists_IntegerParamsAcceptFractionForms pins that the limit,
// sinceDate and untilDate of admin/roles/users and admin/abuse-user-reports
// accept every JSON number with an integer value (10.0, 1e1), as upstream's
// ajv "type": "integer" does, and read it as that integer (#3859).
func TestRolesAbuseLists_IntegerParamsAcceptFractionForms(t *testing.T) {
	// sinceDate (2e3) より前の id と、後の id を 2 件ずつ置く。limit (1.0) と
	// sinceDate を読めば、後の 1 件だけが返る。
	cut := id.AidxCutoffPrefix(time.UnixMilli(2000))
	ids := []string{"0", "00", cut + "a", cut + "b"}
	const body = `{"limit":1.0,"sinceDate":2e3}`
	check := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		require.Len(t, rows, 1)
		// sinceDate を読めば、カーソルより後を古い順に返すので、先頭は cut+"a"。
		assert.Equal(t, cut+"a", rows[0]["id"])
	}
	t.Run("admin/roles/users", func(t *testing.T) {
		h, userRepo, roleRepo, assignRepo := rolesUsersFixture(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
		for n, i := range ids {
			uid := fmt.Sprintf("u%d", n)
			require.NoError(t, userRepo.Create(&model.User{ID: uid, Username: uid}))
			require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: i, UserID: uid, RoleID: "r1"}))
		}
		rec := doPost(h.RolesUsers, `{"roleId":"r1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		check(t, doPost(h.RolesUsers, `{"roleId":"r1","limit":1.0,"sinceDate":2e3}`, adminUser))
	})
	t.Run("admin/abuse-user-reports", func(t *testing.T) {
		h, _, _, _ := newTestHandler(t)
		abuseRepo := testutil.NewMockAbuseReportRepository()
		for _, i := range ids {
			abuseRepo.Reports[i] = &model.AbuseUserReport{ID: i}
		}
		h.SetAbuseRepo(abuseRepo)
		rec := doPost(h.AbuseReports, listCursor, adminUser)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		check(t, doPost(h.AbuseReports, body, adminUser))
	})
}

// 本家 admin/roles/assign の expiresAt も ajv の "type": "integer" なので、
// 整数を小数の形で書いた値を受け、その整数として読む (#3859)。
func TestRolesAssign_ExpiresAtAcceptsFractionForm(t *testing.T) {
	future := time.Date(4099, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	t.Run("future", func(t *testing.T) {
		h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
		userRepo.Users["u1"] = &model.User{ID: "u1"}
		roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
		rec := doPost(h.RolesAssign, fmt.Sprintf(`{"userId":"u1","roleId":"r1","expiresAt":%d.0}`, future), adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		var got *model.RoleAssignment
		for _, a := range assignRepo.Assignments {
			if a.UserID == "u1" && a.RoleID == "r1" {
				got = a
			}
		}
		require.NotNil(t, got)
		require.NotNil(t, got.ExpiresAt)
		assert.Equal(t, future, got.ExpiresAt.UnixMilli())
	})
	t.Run("past", func(t *testing.T) {
		h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
		userRepo.Users["u1"] = &model.User{ID: "u1"}
		roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
		rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1","expiresAt":1e3}`, adminUser)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		exists, _ := assignRepo.Exists("u1", "r1")
		assert.False(t, exists, "past expiresAt must not create an assignment")
	})
}

// 本家 admin/roles/create の name は minLength を持たないので、空文字も受けて
// 空の名前のロールを作る (#3859)。
func TestRolesCreate_AcceptsEmptyName(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	body := strings.Replace(fullRoleCreatePayload, `"name": "Admin"`, `"name": ""`, 1)
	require.NotEqual(t, fullRoleCreatePayload, body)
	rec := doPost(h.RolesCreate, body, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, roleRepo.Roles, 1)
	for _, r := range roleRepo.Roles {
		assert.Equal(t, "", r.Name)
	}
}

// policies の欠けと null を拒否し、meta に書かない (#3859)。以前は nil の
// map をそのまま書き、既定の policy が JSON の null で消えていた。
func TestRolesUpdateDefaultPolicies_RejectsMissingOrNullPolicies(t *testing.T) {
	for _, body := range []string{`{}`, `{"policies":null}`} {
		t.Run(body, func(t *testing.T) {
			h, _, metaRepo, _, _ := newTestHandlerWithAssign(t)
			metaRepo.Meta.Policies = []byte(`{"canInvite":true}`)
			rec := doPost(h.RolesUpdateDefaultPolicies, body, adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
			assert.Nil(t, metaRepo.LastUpdateFields)
			assert.JSONEq(t, `{"canInvite":true}`, string(metaRepo.Meta.Policies))
		})
	}
}

// 本家 admin/roles/create・update の displayOrder は number なので、整数を
// 小数の形で書いた値も受け、その整数として書く (#3859)。
func TestRolesDisplayOrder_AcceptsFractionForm(t *testing.T) {
	t.Run("admin/roles/create", func(t *testing.T) {
		h, _, _, roleRepo := newTestHandler(t)
		body := strings.Replace(fullRoleCreatePayload, `"displayOrder": 0`, `"displayOrder": 1e1`, 1)
		require.NotEqual(t, fullRoleCreatePayload, body)
		rec := doPost(h.RolesCreate, body, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Len(t, roleRepo.Roles, 1)
		for _, r := range roleRepo.Roles {
			assert.Equal(t, 10, r.DisplayOrder)
		}
	})
	t.Run("admin/roles/update", func(t *testing.T) {
		h, _, _, roleRepo := newTestHandler(t)
		roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "r"}
		rec := doPost(h.RolesUpdate, `{"roleId":"r1","displayOrder":3.0}`, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, 3, roleRepo.Roles["r1"].DisplayOrder)
	})
}

// 本家 admin/roles/assign は expiresAt を真偽値として見るので、0 は期限の
// 無い付与になる (#3859)。
func TestRolesAssign_ZeroExpiresAtAssignsWithoutExpiry(t *testing.T) {
	h, userRepo, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", CanEditMembersByModerator: true}
	rec := doPost(h.RolesAssign, `{"userId":"u1","roleId":"r1","expiresAt":0}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	var got *model.RoleAssignment
	for _, a := range assignRepo.Assignments {
		if a.UserID == "u1" && a.RoleID == "r1" {
			got = a
		}
	}
	require.NotNil(t, got, "expiresAt 0 must assign the role")
	assert.Nil(t, got.ExpiresAt)
}

// 本家 admin/show-users の limit・offset も ajv の "type": "integer" なので、
// 整数を小数の形で書いた値を受け、その整数として読む (#3861)。
func TestShowUsers_IntegerParamsAcceptFractionForms(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	for _, u := range []string{"u1", "u2", "u3", "u4"} {
		require.NoError(t, userRepo.Create(&model.User{ID: u, Username: u, UsernameLower: u}))
	}
	// 4 人のうち offset (3) を飛ばせば、limit (2) に届かず 1 人だけが返る。
	rec := doPost(h.ShowUsers, `{"limit":2.0,"offset":3e0}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 1)
	// limit だけを小数の形で送れば、既定の 10 ではなく 1 人だけが返る。
	rec = doPost(h.ShowUsers, `{"limit":1.0}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 1)
}

// 本家 admin/send-email の to・subject・text は minLength を持たないので、空文字
// も受ける。宛先が空なら送れる相手がいないので、送らずに 204 を返す (#3861)。
func TestSendEmail_AcceptsEmptyValues(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	g := bgwork.New()
	h.SetBackgroundForTest(g)
	t.Cleanup(func() { g.Stop(context.Background()) })
	require.Zero(t, g.Stop(context.Background()))

	rec := doPost(h.SendEmail, `{"to":"a@example.com","subject":"","text":""}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, g.Dropped(), "空の件名と本文でも送信を始める")

	rec = doPost(h.SendEmail, `{"to":"","subject":"s","text":"t"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, g.Dropped(), "空の宛先には送信を始めない")
}

// 本家 admin/accounts/find-by-email の空の email は、見つからずに USER_NOT_FOUND
// になる (#3861)。email が空の利用者がいても当てない。
func TestAccountsFindByEmail_EmptyEmailIsUserNotFound(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	empty := ""
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "u1", UsernameLower: "u1"}))
	require.NoError(t, userRepo.CreateProfile(&model.UserProfile{UserID: "u1", Email: &empty}))
	// 文言も本家と同じにする。
	const want = `{"error":{"code":"USER_NOT_FOUND","id":"cb865949-8af5-4062-a88c-ef55e8786d1d","kind":"client","message":"No such user who has the email address."}}`
	rec := doPost(h.AccountsFindByEmail, `{"email":""}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.JSONEq(t, want, rec.Body.String())
	rec = doPost(h.AccountsFindByEmail, `{"email":"ghost@example.com"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.JSONEq(t, want, rec.Body.String())
}

// 本家 admin/update-proxy-account の description は nullable で、null は説明を
// 消す。省いたときは変えない (#3861)。
func TestUpdateProxyAccount_NullClearsDescription(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	proxy := &model.User{ID: "u-proxy", Username: "proxy.actor", UsernameLower: "proxy.actor"}
	require.NoError(t, userRepo.Create(proxy))
	desc := "hello"
	require.NoError(t, userRepo.CreateProfile(&model.UserProfile{UserID: proxy.ID, Description: &desc}))
	h.SetSystemAccountFetcher(&stubSystemAccountFetcher{user: proxy})

	require.Equal(t, http.StatusOK, doPost(h.UpdateProxyAccount, `{}`, adminUser).Code)
	prof, err := userRepo.FindProfileByUserID(proxy.ID)
	require.NoError(t, err)
	require.NotNil(t, prof.Description, "省いたときは変えない")
	assert.Equal(t, "hello", *prof.Description)

	require.Equal(t, http.StatusOK, doPost(h.UpdateProxyAccount, `{"description":null}`, adminUser).Code)
	prof, err = userRepo.FindProfileByUserID(proxy.ID)
	require.NoError(t, err)
	assert.Nil(t, prof.Description, "null は説明を消す")

	assert.Equal(t, http.StatusBadRequest, doPost(h.UpdateProxyAccount, `{"description":1}`, adminUser).Code)
}
