package notifications

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 申請の受付通知 (#2987) の read 側。**型ごとに判定式が違う**のが要点で、
// 絵文字は `canManageCustomEmojis`、アカウントの登録申請はモデレーター。

func wireEmojiApplicationLookup(h *Handler, status string) {
	h.SetEmojiApplicationLookup(func(string) (entity.EmojiApplicationStatus, bool) {
		return entity.EmojiApplicationStatus{Name: "sushi", Status: status}, true
	})
}

func wireSignupApplicationLookup(h *Handler, status string) {
	h.SetSignupApplicationLookup(func(string) (entity.SignupApplicationStatus, bool) {
		return entity.SignupApplicationStatus{Status: status}, true
	})
}

func seedEmojiApplicationReceived(t *testing.T, svc *notification.Service, notifieeID string) {
	t.Helper()
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: notifieeID, NotifierID: "applicant",
		Type:  notification.TypeEmojiApplicationReceived,
		Extra: map[string]any{"applicationId": "a1"},
	})
	require.NoError(t, err)
}

func seedSignupApplicationReceived(t *testing.T, svc *notification.Service, notifieeID string) {
	t.Helper()
	// **notifier は無い。** 申請者はまだアカウントを持っていない。
	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: notifieeID,
		Type:       notification.TypeSignupApplicationReceived,
		Extra:      map[string]any{"applicationId": "s1"},
	})
	require.NoError(t, err)
}

// **絵文字の申請は `canManageCustomEmojis` で判定する。** モデレーターであることは
// 条件にならない (`HasRolePolicy` は root / 管理者しか短絡しない)。
func TestShow_EmojiApplicationReceived_VisibleToPolicyHolder(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{
		policies: map[string]map[string]bool{
			"emojimgr": {role.PolicyCanManageCustomEmojis: true},
		},
	})
	wireEmojiApplicationLookup(h, "pending")
	seedEmojiApplicationReceived(t, svc, "emojimgr")

	out := listNotifications(t, h, "emojimgr")
	require.Len(t, out, 1)
	assert.Equal(t, string(notification.TypeEmojiApplicationReceived), out[0]["type"])
	app, ok := out[0]["emojiApplication"].(map[string]any)
	require.True(t, ok, "申請の状態が引き直されていない: %v", out[0])
	assert.Equal(t, "sushi", app["name"])
	assert.Equal(t, "pending", app["status"])
}

// **モデレーターでも policy が無ければ見えない。** 審査 endpoint は
// `RequireRolePolicy(canManageCustomEmojis)` なので、押せない人に見せると
// 「届いたのに押せない」になる。
func TestShow_EmojiApplicationReceived_HiddenFromPlainModerator(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireEmojiApplicationLookup(h, "pending")
	seedEmojiApplicationReceived(t, svc, "mod1")

	assert.Empty(t, listNotifications(t, h, "mod1"))
}

// 管理者は policy を持たなくても通る (HasRolePolicy が短絡する)。
func TestShow_EmojiApplicationReceived_VisibleToAdministrator(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{admins: map[string]bool{"admin1": true}})
	wireEmojiApplicationLookup(h, "pending")
	seedEmojiApplicationReceived(t, svc, "admin1")

	assert.Len(t, listNotifications(t, h, "admin1"), 1)
}

// **権限を失うと read 時に消える (fail-closed)。** 通知は作成時点の権限で
// 作られるので、これが無いと元管理者が申請の存在を見続けられる。
func TestShow_EmojiApplicationReceived_HiddenAfterLosingPolicy(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{})
	wireEmojiApplicationLookup(h, "pending")
	seedEmojiApplicationReceived(t, svc, "exmgr")

	assert.Empty(t, listNotifications(t, h, "exmgr"))
}

// checker 未配線でも出さない (判定できないまま出すより出さない)。
func TestShow_EmojiApplicationReceived_HiddenWhenCheckerUnwired(t *testing.T) {
	h, svc := newTestHandler(t)
	wireEmojiApplicationLookup(h, "pending")
	seedEmojiApplicationReceived(t, svc, "someone")

	assert.Empty(t, listNotifications(t, h, "someone"))
}

// lookup 未配線 / 申請が消えていたら通知ごと drop する。
func TestShow_EmojiApplicationReceived_DroppedWhenApplicationGone(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{admins: map[string]bool{"admin1": true}})
	h.SetEmojiApplicationLookup(func(string) (entity.EmojiApplicationStatus, bool) {
		return entity.EmojiApplicationStatus{}, false
	})
	seedEmojiApplicationReceived(t, svc, "admin1")

	assert.Empty(t, listNotifications(t, h, "admin1"))
}

// --- アカウントの登録申請 ---

func TestShow_SignupApplicationReceived_VisibleToModerator(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireSignupApplicationLookup(h, "pending")
	seedSignupApplicationReceived(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 1)
	app, ok := out[0]["signupApplication"].(map[string]any)
	require.True(t, ok, "申請の状態が引き直されていない: %v", out[0])
	assert.Equal(t, "pending", app["status"])
	// **回答は出さない。** 氏名や連絡先が入りうる。
	assert.NotContains(t, out[0], "answers")
	// notifier が無いので user は載らない。
	assert.NotContains(t, out[0], "userId")
}

// **絵文字の policy では見えない。** 審査 endpoint が RequireModerator なので、
// 絵文字管理者に見せると「届いたのに押せない」になる。
func TestShow_SignupApplicationReceived_HiddenFromEmojiManager(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{
		policies: map[string]map[string]bool{
			"emojimgr": {role.PolicyCanManageCustomEmojis: true},
		},
	})
	wireSignupApplicationLookup(h, "pending")
	seedSignupApplicationReceived(t, svc, "emojimgr")

	assert.Empty(t, listNotifications(t, h, "emojimgr"))
}

// 処理済みかどうかが読み出し時に分かる (二重で当たらないため)。
func TestShow_SignupApplicationReceived_ShowsCurrentStatus(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireSignupApplicationLookup(h, "approved")
	seedSignupApplicationReceived(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 1)
	assert.Equal(t, "approved", out[0]["signupApplication"].(map[string]any)["status"])
}

// applicationId は packed entity の id として出しているので、raw のまま二重に
// surface しない (roleId / invitationId と同じ扱い)。
func TestShow_ApplicationReceived_DoesNotSurfaceRawApplicationID(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	wireSignupApplicationLookup(h, "pending")
	seedSignupApplicationReceived(t, svc, "mod1")

	out := listNotifications(t, h, "mod1")
	require.Len(t, out, 1)
	assert.NotContains(t, out[0], "applicationId")
}

// **申請が消えていたら通知ごと drop する。** 経緯を辿れない「申請が来ました」
// だけの通知は読んだ人に何も伝えないし、状態が分からないまま「未処理」に
// 見えると二重で当たる (絵文字側と同じ形)。
func TestShow_SignupApplicationReceived_DroppedWhenApplicationGone(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	h.SetSignupApplicationLookup(func(string) (entity.SignupApplicationStatus, bool) {
		return entity.SignupApplicationStatus{}, false
	})
	seedSignupApplicationReceived(t, svc, "mod1")

	assert.Empty(t, listNotifications(t, h, "mod1"))
}

// lookup 未配線でも出さない (fail-closed)。状態を引けないまま出すと、
// 処理済みの申請に二重で当たる。
func TestShow_SignupApplicationReceived_DroppedWhenLookupUnwired(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{moderators: map[string]bool{"mod1": true}})
	seedSignupApplicationReceived(t, svc, "mod1")

	assert.Empty(t, listNotifications(t, h, "mod1"))
}

// **運営向け通知はミュートで落とさない (#2868 と同じ判断)。** 絵文字の申請の
// notifier は申請者なので、ミュートしている相手からの申請が審査者の通知欄に
// 一切現れなくなる。
func TestShow_EmojiApplicationReceived_NotFilteredByMute(t *testing.T) {
	h, svc := newTestHandler(t)
	h.SetModeratorChecker(stubModeratorChecker{admins: map[string]bool{"admin1": true}})
	wireEmojiApplicationLookup(h, "pending")
	userRepo := testutil.NewMockUserRepository()
	mutingRepo := testutil.NewMockMutingRepository()
	userRepo.Users["applicant"] = &model.User{ID: "applicant", Username: "applicant"}
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: "mu1", MuterID: "admin1", MuteeID: "applicant"}))
	h.SetRepos(userRepo, testutil.NewMockNoteRepository())
	h.SetMutingRepo(mutingRepo)
	seedEmojiApplicationReceived(t, svc, "admin1")

	assert.Len(t, listNotifications(t, h, "admin1"), 1)
}

// **staff 以外の通知は巻き添えにしない。** 権限判定は型ごとの話で、
// 権限を失った人のページに古い運営向け通知が 1 行混ざっただけで、その人の
// 通常の通知まで消えてはいけない。
func TestShow_StaffFilterDoesNotDropOrdinaryNotifications(t *testing.T) {
	h, svc := newTestHandler(t)
	// 権限なし (= 運営向け通知は落ちる)。
	h.SetModeratorChecker(stubModeratorChecker{})
	wireSignupApplicationLookup(h, "pending")
	seedSignupApplicationReceived(t, svc, "u1")

	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "u1", NotifierID: "someone", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	out := listNotifications(t, h, "u1")
	require.Len(t, out, 1, "通常の通知まで消えている: %v", out)
	assert.Equal(t, string(notification.TypeFollow), out[0]["type"])
}
