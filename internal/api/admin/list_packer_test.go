package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingListPacker records each DetailedMany / FillLites call and marks the
// packed users so the test can see the packer's output reached the response.
type recordingListPacker struct {
	viewers   []*model.User
	userIDs   [][]string
	liteCalls int
}

func (r *recordingListPacker) DetailedMany(_ context.Context, viewer *model.User, users []*model.User, profiles map[string]*model.UserProfile) []entity.UserDetailed {
	r.viewers = append(r.viewers, viewer)
	ids := make([]string, 0, len(users))
	out := make([]entity.UserDetailed, len(users))
	for i, u := range users {
		ids = append(ids, u.ID)
		out[i] = entity.PackUserDetailed(u, profiles[u.ID])
		out[i].PinnedNoteIDs = []string{"pin-" + u.ID}
	}
	r.userIDs = append(r.userIDs, ids)
	return out
}

func (r *recordingListPacker) FillLites(users []*model.User, lites []*entity.UserLite) {
	r.liteCalls++
	for i := range users {
		host := "remote.example"
		lites[i].Instance = &entity.InstanceLite{Name: &host}
	}
}

func TestHasListPacker(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.False(t, h.HasListPacker())
	h.SetListPacker(&recordingListPacker{})
	assert.True(t, h.HasListPacker())
}

// 本家 admin/show-users は packMany(users, me) で、自分自身は MeDetailed (#3330)。
func TestShowUsers_UsesListPackerWithViewer(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	userRepo.Users["admin1"] = &model.User{ID: "admin1", Username: "admin"}
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "a"}
	p := &recordingListPacker{}
	h.SetListPacker(p)

	rec := doPost(h.ShowUsers, `{"limit":10}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	require.Len(t, p.viewers, 1, "一覧は 1 回でまとめて pack する")
	assert.Equal(t, adminUser, p.viewers[0])
	for _, u := range resp {
		assert.Equal(t, []any{"pin-" + u["id"].(string)}, u["pinnedNoteIds"])
		_, isMe := u["avatarId"]
		assert.Equal(t, u["id"] == "admin1", isMe, "自分自身だけ MeDetailed")
	}
}

// 本家 admin/roles/users も packMany(users, me, {schema: 'UserDetailed'}) (#3330)。
func TestRolesUsers_UsesListPackerWithViewer(t *testing.T) {
	h, userRepo, roleRepo, assignRepo := rolesUsersFixture(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1"}
	require.NoError(t, userRepo.Create(&model.User{ID: "u1", Username: "alice"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "9c2bw9q5fa0000000000000000", UserID: "u1", RoleID: "r1"}))
	require.NoError(t, userRepo.Create(&model.User{ID: adminUser.ID, Username: "admin"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "9c2bw9q5fb0000000000000000", UserID: adminUser.ID, RoleID: "r1"}))
	p := &recordingListPacker{}
	h.SetListPacker(p)

	rec := doPost(h.RolesUsers, `{"roleId":"r1","limit":10}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	for _, row := range resp {
		user := row["user"].(map[string]any)
		assert.Equal(t, []any{"pin-" + user["id"].(string)}, user["pinnedNoteIds"])
		// 本家の pack は isMe なら MeDetailed (avatarId は MeDetailed にだけある)。
		_, isMe := user["avatarId"]
		assert.Equal(t, user["id"] == adminUser.ID, isMe, "自分自身だけ MeDetailed")
	}
	require.Len(t, p.viewers, 1)
	assert.Equal(t, adminUser, p.viewers[0])
}

// 本家 AbuseUserReportEntityService.packMany は 3 者をまとめて
// packMany(users, null) で組む (#3330)。
func TestAbuseReports_PacksUsersOnceAsAnonymous(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	reporter := &model.User{ID: "rep", Username: "rep"}
	target := &model.User{ID: "tgt", Username: "tgt"}
	assignee := &model.User{ID: "asg", Username: "asg"}
	abuseRepo.Reports["r1"] = &model.AbuseUserReport{ID: "r1", ReporterID: "rep", Reporter: reporter, TargetUserID: "tgt", TargetUser: target, Assignee: assignee}
	abuseRepo.Reports["r2"] = &model.AbuseUserReport{ID: "r2", ReporterID: "rep", Reporter: reporter, TargetUserID: "tgt", TargetUser: target}
	h.SetAbuseRepo(abuseRepo)
	p := &recordingListPacker{}
	h.SetListPacker(p)

	rec := doPost(h.AbuseReports, `{"state":"all"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	require.Len(t, p.viewers, 1, "通報の利用者は 1 回でまとめて pack する")
	assert.Nil(t, p.viewers[0], "本家は閲覧者 null で pack する")
	assert.ElementsMatch(t, []string{"rep", "tgt", "asg"}, p.userIDs[0])
	for _, r := range resp {
		assert.Equal(t, []any{"pin-rep"}, r["reporter"].(map[string]any)["pinnedNoteIds"])
		assert.Equal(t, []any{"pin-tgt"}, r["targetUser"].(map[string]any)["pinnedNoteIds"])
		if r["id"] == "r2" {
			assert.Nil(t, r["assignee"])
		} else {
			assert.Equal(t, []any{"pin-asg"}, r["assignee"].(map[string]any)["pinnedNoteIds"])
		}
	}
}

// 本家 ModerationLogEntityService.packMany は packMany(users, null) (#3330)。
func TestShowModerationLogs_PacksActorsOnceAsAnonymous(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	modLogRepo := testutil.NewMockModerationLogRepository()
	mod := &model.User{ID: "mod1", Username: "modu"}
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{ID: "l1", UserID: "mod1", Type: "suspend", User: mod}))
	require.NoError(t, modLogRepo.Create(&model.ModerationLog{ID: "l2", UserID: "mod1", Type: "unsuspend", User: mod}))
	gen, _ := id.NewGenerator("aidx")
	h.SetModLogService(moderationlog.New(modLogRepo, gen))
	p := &recordingListPacker{}
	h.SetListPacker(p)

	rec := doPost(h.ShowModerationLogs, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	require.Len(t, p.viewers, 1)
	assert.Nil(t, p.viewers[0], "本家は閲覧者 null で pack する")
	assert.Equal(t, []string{"mod1"}, p.userIDs[0], "同じ actor は 1 回だけ")
	for _, l := range resp {
		assert.Equal(t, []any{"pin-mod1"}, l["user"].(map[string]any)["pinnedNoteIds"])
	}
}

// find-by-email の利用者にも instance と絵文字を埋める (#3330)。
func TestAccountsFindByEmail_FillsLites(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	email := "x@example.test"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "a"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email}
	p := &recordingListPacker{}
	h.SetListPacker(p)

	rec := doPost(h.AccountsFindByEmail, `{"email":"x@example.test"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, p.liteCalls)
	assert.Equal(t, "remote.example", resp["instance"].(map[string]any)["name"])
}

// 本家 update-proxy-account は pack(proxy.id, proxy, {schema: 'MeDetailed'})。
// proxy 自身を閲覧者にしてピン留めを埋め、MeDetailed で返す (#3330)。
func TestUpdateProxyAccount_PacksAsProxySelf(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	proxy := &model.User{ID: "9c2bw9q5fa", Username: "proxy.actor", UsernameLower: "proxy.actor"}
	require.NoError(t, userRepo.Create(proxy))
	require.NoError(t, userRepo.CreateProfile(&model.UserProfile{UserID: proxy.ID}))
	h.SetSystemAccountFetcher(&stubSystemAccountFetcher{user: proxy})
	extras := &recordingExtras{}
	h.SetDetailExtras(extras)

	rec := doPost(h.UpdateProxyAccount, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, extras.called)
	require.NotNil(t, extras.viewer)
	assert.Equal(t, proxy.ID, extras.viewer.ID, "閲覧者は proxy 自身")
	assert.Equal(t, []any{"pinned"}, resp["pinnedNoteIds"])
	assert.Contains(t, resp, "avatarId", "MeDetailed")
	assert.NotEmpty(t, resp["createdAt"])
}
