package users

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// stubSelfEnricher stands in for the /api/i handler that fills the
// handler-owned MeDetailed fields in production (router が配線する)。
type stubSelfEnricher struct {
	adminID string
}

func (s stubSelfEnricher) EnrichSelf(_ context.Context, u *model.User, _ *model.UserProfile, resp map[string]any) {
	resp["isAdmin"] = u.ID == s.adminID
	resp["isModerator"] = u.ID == s.adminID
}

// #2091: users/show の self-view (MeDetailed) は isAdmin / isModerator を
// roleService (moderatorChecker、root fallback 込み) から populate しなければ
// ならない。upstream / /api/i と同値にする。修正前は entity packer が field を
// 宣言するだけで常に false だった。
func TestShow_SelfView_PopulatesIsAdminIsModerator(t *testing.T) {
	h, repo := newTestHandler(t)
	alice := newTargetWithProfile(repo, "alice", &model.UserProfile{})
	// alice を admin/moderator (= root 相当) として扱う stub。
	h.SetModeratorChecker(modByID{id: "alice"})
	meself.SetEnricher(stubSelfEnricher{adminID: "alice"})
	t.Cleanup(func() { meself.SetEnricher(nil) })

	resp := showWithViewer(t, h, "alice", alice)
	assert.Equal(t, true, resp["isAdmin"], "self-view は root/admin を isAdmin=true で返す")
	assert.Equal(t, true, resp["isModerator"], "self-view は moderator を isModerator=true で返す")
}

// 非 admin の self-view は false (zero value ではなく明示的に false を populate)。
func TestShow_SelfView_NonAdminIsFalse(t *testing.T) {
	h, repo := newTestHandler(t)
	bob := newTargetWithProfile(repo, "bob", &model.UserProfile{})
	// bob 以外を admin にする = bob は非 admin。
	h.SetModeratorChecker(modByID{id: "someone-else"})
	meself.SetEnricher(stubSelfEnricher{adminID: "someone-else"})
	t.Cleanup(func() { meself.SetEnricher(nil) })

	resp := showWithViewer(t, h, "bob", bob)
	assert.Equal(t, false, resp["isAdmin"])
	assert.Equal(t, false, resp["isModerator"])
}
