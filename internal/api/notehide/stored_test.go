package notehide

import (
	"testing"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
)

// **保存済み ID から引いた top-level note にも intrinsic な判定を掛けること。**
//
// i/favorites は favorite した後で閲覧資格を失った followers / specified note を
// 引く。upstream は pack(note, me) の hideNote で本文を空にするので、top-level
// にも全判定 (HideEmbedDecision) を通す。
func TestHideStoredAt_TopLevelFullDecision(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo([2]string{"viewer", "followed"})

	unfollowed := *followersEmbed("f1", "stranger")
	followed := *followersEmbed("f2", "followed")
	own := *followersEmbed("f3", "viewer")
	replyToMe := *followersEmbed("f4", "stranger")
	replyToMe.Reply = &entity.NoteEntity{ID: "mine", UserID: "viewer", Visibility: "public"}
	specifiedOther := *followersEmbed("s1", "stranger")
	specifiedOther.Visibility = "specified"
	specifiedOther.VisibleUserIDs = []string{"someone"}
	specifiedMe := *followersEmbed("s2", "stranger")
	specifiedMe.Visibility = "specified"
	specifiedMe.VisibleUserIDs = []string{"viewer"}

	packed := []entity.NoteEntity{unfollowed, followed, own, replyToMe, specifiedOther, specifiedMe}
	hideAt(viewer, packed, repo, heNowMs, true)

	want := map[string]bool{"f1": true, "f2": false, "f3": false, "f4": false, "s1": true, "s2": false}
	for _, n := range packed {
		if n.IsHidden != want[n.ID] {
			t.Errorf("%s: IsHidden = %v, want %v", n.ID, n.IsHidden, want[n.ID])
		}
		if want[n.ID] && n.Text != nil {
			t.Errorf("%s: hidden note must have text blanked", n.ID)
		}
	}
	if repo.calls != 1 {
		t.Errorf("expected exactly 1 batched follow query, got %d", repo.calls)
	}
}

// HideEmbeds (通常の一覧) は top-level の intrinsic followers を素通しする
// (別のゲートが担う、#1568)。全判定は HideStoredNotes を選んだときだけ。
func TestHideEmbedsAt_TopLevelFollowersUntouched(t *testing.T) {
	packed := []entity.NoteEntity{*followersEmbed("f1", "stranger")}
	hideEmbedsAt(&model.User{ID: "viewer"}, packed, followsRepo(), heNowMs)
	if packed[0].IsHidden {
		t.Error("HideEmbeds must not apply the intrinsic followers gate to top-level notes")
	}
}

// 公開用の入口は配線された followingRepo を使う。未配線なら fail-closed。
func TestHideStoredNotes_NilRepoFailClosed(t *testing.T) {
	prev := followingRepo
	defer func() { followingRepo = prev }()
	followingRepo = nil
	packed := []entity.NoteEntity{*followersEmbed("f1", "stranger")}
	HideStoredNotes(&model.User{ID: "viewer"}, packed)
	if !packed[0].IsHidden {
		t.Error("nil followingRepo must fail closed (blank followers top-level note)")
	}
}
