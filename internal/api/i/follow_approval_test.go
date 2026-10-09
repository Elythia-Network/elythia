package i

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

func followApprovalUser(repo *testutil.MockUserRepository, locked bool) *model.User {
	u := &model.User{ID: "user1", Username: "user1", IsLocked: locked, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = u
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}
	return u
}

// i/update は期間を本家の paramDef (integer、nullable、0〜2592000) どおりに
// 受け、MeDetailed に同じ値を返す。省略は不変、null はクリア。null の項目は
// 応答に出さない (entity.MeDetailed の注記、#3466)。
func TestUpdate_FollowApprovalSeconds(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := followApprovalUser(repo, false)

	rec := post(h.Update, `{"followApprovalLocalSeconds":43200,"followApprovalRemoteSeconds":2592000}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	p := repo.Profiles["user1"]
	require.NotNil(t, p.FollowApprovalLocalSeconds)
	require.NotNil(t, p.FollowApprovalRemoteSeconds)
	assert.Equal(t, 43200, *p.FollowApprovalLocalSeconds)
	assert.Equal(t, 2592000, *p.FollowApprovalRemoteSeconds, "上限の 30 日は受け付ける")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.EqualValues(t, 43200, body["followApprovalLocalSeconds"])
	assert.EqualValues(t, 2592000, body["followApprovalRemoteSeconds"])

	rec = post(h.Update, `{"followApprovalLocalSeconds":null}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, p.FollowApprovalLocalSeconds, "null はクリア")
	require.NotNil(t, p.FollowApprovalRemoteSeconds, "省略は不変")
	assert.Equal(t, 2592000, *p.FollowApprovalRemoteSeconds)
	body = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotContains(t, body, "followApprovalLocalSeconds", "null は出さない")
	assert.EqualValues(t, 2592000, body["followApprovalRemoteSeconds"])

	// 本家の ajv (type: integer) と同じく、整数の値なら小数や指数の書き方でも受ける。
	rec = post(h.Update, `{"followApprovalLocalSeconds":2.0,"followApprovalRemoteSeconds":1e3}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, p.FollowApprovalLocalSeconds)
	assert.Equal(t, 2, *p.FollowApprovalLocalSeconds)
	require.NotNil(t, p.FollowApprovalRemoteSeconds)
	assert.Equal(t, 1000, *p.FollowApprovalRemoteSeconds)

	rec = post(h.Update, `{"followApprovalRemoteSeconds":0}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, p.FollowApprovalRemoteSeconds)
	assert.Equal(t, 0, *p.FollowApprovalRemoteSeconds, "0 は null と区別して保存する")
}

// 範囲外・小数・文字列は INVALID_PARAM で、何も保存しない (本家の e2e と同じ
// -1 / 1.5 / 2592001)。
func TestUpdate_FollowApprovalSeconds_Invalid(t *testing.T) {
	for _, key := range []string{"followApprovalLocalSeconds", "followApprovalRemoteSeconds"} {
		for _, v := range []string{"-1", "1.5", "2592001", "2592000.5", "-0.5", `"3600"`, "true", "{}"} {
			t.Run(key+"="+v, func(t *testing.T) {
				h, repo, _, _ := newTestHandler(t)
				user := followApprovalUser(repo, false)
				rec := post(h.Update, `{"`+key+`":`+v+`,"followApprovalAction":"silentFollow"}`, user)
				assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
				p := repo.Profiles["user1"]
				assert.Nil(t, p.FollowApprovalLocalSeconds)
				assert.Nil(t, p.FollowApprovalRemoteSeconds)
				assert.Empty(t, p.FollowApprovalAction, "同じリクエストの他の項目も保存しない")
			})
		}
	}
}

// 止め方は 3 つの値だけを受け、MeDetailed に返す。既定の request と未設定は
// 応答に出さない (#3466)。
func TestUpdate_FollowApprovalAction(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := followApprovalUser(repo, false)

	rec := post(h.Update, `{}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	for _, k := range []string{"followApprovalLocalSeconds", "followApprovalRemoteSeconds", "followApprovalAction"} {
		assert.NotContains(t, body, k, "未設定の利用者の i は本家 2026.10.0 と同じ key のまま")
	}

	for _, action := range []string{"silentRequest", "silentFollow", "request"} {
		rec = post(h.Update, `{"followApprovalAction":"`+action+`"}`, user)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, action, repo.Profiles["user1"].FollowApprovalAction)
		body = nil
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		if action == "request" {
			assert.NotContains(t, body, "followApprovalAction")
		} else {
			assert.Equal(t, action, body["followApprovalAction"])
		}
	}

	for _, bad := range []string{`"silent"`, `""`, `1`} {
		rec = post(h.Update, `{"followApprovalAction":`+bad+`}`, user)
		assert.Equal(t, http.StatusBadRequest, rec.Code, bad)
	}
	assert.Equal(t, "request", repo.Profiles["user1"].FollowApprovalAction)
}

type recordingBulkAccepter struct {
	calls []string
	err   error
}

func (r *recordingBulkAccepter) AcceptAllRequests(followeeID string) error {
	r.calls = append(r.calls, followeeID)
	return r.err
}

// 鍵を外したとき (保存されていた isLocked が true で、false を送った) だけ
// 溜まったリクエストを承認する。frontend はプライバシーの設定を保存するたびに
// isLocked を送るので、未施錠のまま false を送っただけでは承認しない (#3466)。
func TestUpdate_UnlockAcceptsPendingRequests(t *testing.T) {
	cases := []struct {
		name string
		// stored は DB 上の isLocked、cached は認証キャッシュの写し (me)。
		stored, cached bool
		body           string
		want           bool
	}{
		{name: "unlock", stored: true, cached: true, body: `{"isLocked":false}`, want: true},
		{name: "unlock with a stale cache", stored: true, cached: false, body: `{"isLocked":false}`, want: true},
		{name: "already unlocked", stored: false, cached: false, body: `{"isLocked":false}`},
		{name: "already unlocked with a stale cache", stored: false, cached: true, body: `{"isLocked":false}`},
		{name: "stay locked", stored: true, cached: true, body: `{"isLocked":true}`},
		{name: "isLocked omitted", stored: true, cached: true, body: `{"followApprovalLocalSeconds":3600}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _, _ := newTestHandler(t)
			followApprovalUser(repo, tc.stored)
			h.SetUserRepo(repo)
			acc := &recordingBulkAccepter{}
			h.SetFollowRequestBulkAccepter(acc)
			me := &model.User{ID: "user1", Username: "user1", IsLocked: tc.cached, AvatarDecorations: datatypes.JSON([]byte("[]"))}

			rec := post(h.Update, tc.body, me)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			if tc.want {
				assert.Equal(t, []string{"user1"}, acc.calls)
			} else {
				assert.Empty(t, acc.calls)
			}
		})
	}
}

// 一括承認が失敗しても、設定は保存済みなので成功を返す。
func TestUpdate_UnlockAcceptFailureStillSucceeds(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := followApprovalUser(repo, true)
	h.SetUserRepo(repo)
	acc := &recordingBulkAccepter{err: errors.New("db down")}
	h.SetFollowRequestBulkAccepter(acc)
	assert.True(t, h.HasFollowRequestBulkAccepter())

	rec := post(h.Update, `{"isLocked":false}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, acc.calls, 1)
	assert.False(t, repo.Users["user1"].IsLocked)
}
