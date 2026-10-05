package announcements_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Show エンドポイントの 3 経路 (bad param / not found / ok) をカバーする。
// 0% だった handler_show.go を完全に exercise する目的。
func TestShow_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Show, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := doPost(h.Show, `{"announcementId":"ghost"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestShow_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "hi", Text: "hello", IsActive: true}
	rec := doPost(h.Show, `{"announcementId":"a1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "Announcement", resp) // L3 (#1318)
}

// #1955: 匿名 caller には isRead key を出さない (upstream pack() は me==null で
// isRead を undefined にする)。
func TestShow_AnonymousOmitsIsRead(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "hi", Text: "hello", IsActive: true}
	rec := doPost(h.Show, `{"announcementId":"a1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	_, hasIsRead := resp["isRead"]
	assert.False(t, hasIsRead, "匿名には isRead key を出さない (#1955)")
}

// #1955: 既読のログインユーザーには isRead:true を返す (旧実装は常に false)。
func TestShow_AuthenticatedReflectsReadState(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "hi", Text: "hello", IsActive: true}
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{UserID: "u1", AnnouncementID: "a1"}))

	rec := doPost(h.Show, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isRead"], "既読ユーザーには isRead:true (#1955)")
}

// 未読のログインユーザーには isRead:false。
func TestShow_AuthenticatedUnreadIsReadFalse(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "hi", Text: "hello", IsActive: true}

	rec := doPost(h.Show, `{"announcementId":"a1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["isRead"], "未読ユーザーには isRead:false (#1955)")
}

// upstream Misskey TS 2026.5.4 fix: userId-targeted announcement に対して
// 非ログイン user (me == nil) は 404 を返す。旧 mk-go では Show 経路に権限
// check が一切なく、admin が特定 user に向けた通知が漏れる drop-in regression
// 状態だった (#1164 Phase C)。
func TestShow_UserTargeted_AnonymousReturns404(t *testing.T) {
	h, repo := newTestHandler(t)
	uid := "alice"
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "private", Text: "to alice", IsActive: true, UserID: &uid}

	rec := doPost(h.Show, `{"announcementId":"a1"}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// upstream 2026.5.3 以前から閉じている穴 (= 他人宛 announcement へのアクセス)
// が mk-go 単体では未対応だったため同時に塞ぐ。
func TestShow_UserTargeted_OtherUserReturns404(t *testing.T) {
	h, repo := newTestHandler(t)
	uid := "alice"
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "private", Text: "to alice", IsActive: true, UserID: &uid}

	bob := &model.User{ID: "bob"}
	rec := doPost(h.Show, `{"announcementId":"a1"}`, bob)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 当該 user 本人は閲覧できる (= upstream の挙動と一致)。
func TestShow_UserTargeted_OwnerReturnsOK(t *testing.T) {
	h, repo := newTestHandler(t)
	uid := "alice"
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "private", Text: "to alice", IsActive: true, UserID: &uid}

	alice := &model.User{ID: "alice"}
	rec := doPost(h.Show, `{"announcementId":"a1"}`, alice)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1955: user-targeted announcement の owner でも isRead が read 状態を反映する。
func TestShow_UserTargeted_OwnerReflectsReadState(t *testing.T) {
	h, repo := newTestHandler(t)
	uid := "alice"
	repo.Items["a1"] = &model.Announcement{ID: "a1", Title: "private", Text: "to alice", IsActive: true, UserID: &uid}
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{UserID: "alice", AnnouncementID: "a1"}))

	rec := doPost(h.Show, `{"announcementId":"a1"}`, &model.User{ID: "alice"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isRead"], "owner-targeted でも既読は isRead:true (#1955)")
}
