package note_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

func TestCanSeeNote_Public(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityPublic}
	assert.True(t, note.CanSeeNote(nil, n, nil))
	assert.True(t, note.CanSeeNote(&model.User{ID: "u"}, n, nil))
}

func TestCanSeeNote_Home(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityHome}
	assert.True(t, note.CanSeeNote(nil, n, nil))
}

func TestCanSeeNote_Nil(t *testing.T) {
	assert.False(t, note.CanSeeNote(nil, nil, nil))
}

func TestCanSeeNote_FollowersUnauthenticated(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}
	assert.False(t, note.CanSeeNote(nil, n, nil))
}

func TestCanSeeNote_FollowersAuthor(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}
	assert.True(t, note.CanSeeNote(&model.User{ID: "author"}, n, nil))
}

func TestCanSeeNote_FollowersWithoutChecker(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}
	assert.False(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, nil))
}

func TestCanSeeNote_FollowersFollowing(t *testing.T) {
	repo := testutil.NewMockFollowingRepository()
	repo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "author"}
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}
	assert.True(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, repo))
}

func TestCanSeeNote_FollowersNotFollowing(t *testing.T) {
	repo := testutil.NewMockFollowingRepository()
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}
	assert.False(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, repo))
}

// erroringFollowingRepo simulates a DB error during the follow check.
type erroringFollowingRepo struct{ repository.FollowingRepository }

func (erroringFollowingRepo) Exists(_, _ string) (bool, error) {
	return false, errors.New("db down")
}

func TestCanSeeNote_FollowersErrorTreatedAsHidden(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}
	assert.False(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, erroringFollowingRepo{}))
}

func TestCanSeeNote_SpecifiedUnauthenticated(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilitySpecified}
	assert.False(t, note.CanSeeNote(nil, n, nil))
}

func TestCanSeeNote_SpecifiedAuthor(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilitySpecified}
	assert.True(t, note.CanSeeNote(&model.User{ID: "author"}, n, nil))
}

func TestCanSeeNote_SpecifiedAllowed(t *testing.T) {
	n := &model.Note{
		UserID:         "author",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"viewer"},
	}
	assert.True(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, nil))
}

func TestCanSeeNote_SpecifiedDenied(t *testing.T) {
	n := &model.Note{
		UserID:         "author",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"someoneElse"},
	}
	assert.False(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, nil))
}

func TestCanSeeNote_UnknownVisibility(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibility("unknown")}
	assert.False(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, nil))
}

// #2106 N27: followers note で viewer が mention されていれば read 可 (upstream)。
func TestCanSeeNote_FollowersMentioned(t *testing.T) {
	repo := testutil.NewMockFollowingRepository() // 非フォロワー
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers, Mentions: model.StringArray{"viewer"}}
	assert.True(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, repo))
}

// #2106 N27: followers note で reply 先が viewer なら read 可 (upstream followers 分岐の replyUserId)。
func TestCanSeeNote_FollowersReplyToViewer(t *testing.T) {
	repo := testutil.NewMockFollowingRepository() // 非フォロワー
	rid := "viewer"
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers, ReplyUserID: &rid}
	assert.True(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, repo))
}

// upstream isVisibleForMe / shouldHideNote は specified で visibleUserIds だけを見る。
// 本文で @ されただけの相手には direct note を見せない。
func TestCanSeeNote_SpecifiedMentionedButNotAddressed(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{"other"}, Mentions: model.StringArray{"viewer"}}
	assert.False(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, nil))
}

// specified で visibleUserIds に含まれていれば read 可。
func TestCanSeeNote_SpecifiedAddressed(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{"viewer"}}
	assert.True(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, nil))
}

// #2106 N27 regression guard: followers note で非フォロワー・非mention・非reply は依然 不可視。
func TestCanSeeNote_FollowersStrangerStillHidden(t *testing.T) {
	repo := testutil.NewMockFollowingRepository()
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers, Mentions: model.StringArray{"someone-else"}}
	assert.False(t, note.CanSeeNote(&model.User{ID: "stranger"}, n, repo))
}

// upstream 2026.7.0 #17747: reply は reply 先より広い可視性になれない。
// ローカル投稿経路と AP 受信経路の双方から使う共有ヘルパー。
func TestClampVisibilityForReply(t *testing.T) {
	tests := []struct {
		name       string
		target     model.NoteVisibility
		visibility model.NoteVisibility
		want       model.NoteVisibility
	}{
		{"public target keeps public", model.NoteVisibilityPublic, model.NoteVisibilityPublic, model.NoteVisibilityPublic},
		{"public target keeps home", model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityHome},
		{"home target clamps public", model.NoteVisibilityHome, model.NoteVisibilityPublic, model.NoteVisibilityHome},
		{"home target keeps followers", model.NoteVisibilityHome, model.NoteVisibilityFollowers, model.NoteVisibilityFollowers},
		{"followers target clamps public", model.NoteVisibilityFollowers, model.NoteVisibilityPublic, model.NoteVisibilityFollowers},
		{"followers target clamps home", model.NoteVisibilityFollowers, model.NoteVisibilityHome, model.NoteVisibilityFollowers},
		{"followers target keeps specified", model.NoteVisibilityFollowers, model.NoteVisibilitySpecified, model.NoteVisibilitySpecified},
		{"specified target clamps public", model.NoteVisibilitySpecified, model.NoteVisibilityPublic, model.NoteVisibilitySpecified},
		{"specified target clamps followers", model.NoteVisibilitySpecified, model.NoteVisibilityFollowers, model.NoteVisibilitySpecified},
		{"specified target keeps specified", model.NoteVisibilitySpecified, model.NoteVisibilitySpecified, model.NoteVisibilitySpecified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, note.ClampVisibilityForReply(tt.target, tt.visibility))
		})
	}
}

// **エラー時に true を返す repo で見ること。** 既存の
// TestCanSeeNote_FollowersErrorTreatedAsHidden の stub は `(false, err)` を
// 返すので、`err == nil && ok` を `ok` に縮める変異を区別できない (#2752 で
// CanSeeNoteFunc へ切り出した際にここを畳んだ)。DB エラー時に followers note
// が漏れる形を固定する。
type trueWithErrorFollowingRepo struct{ repository.FollowingRepository }

func (trueWithErrorFollowingRepo) Exists(_, _ string) (bool, error) {
	return true, errors.New("db down")
}

func TestCanSeeNote_FollowLookupErrorFailsClosed(t *testing.T) {
	n := &model.Note{UserID: "author", Visibility: model.NoteVisibilityFollowers}
	assert.False(t, note.CanSeeNote(&model.User{ID: "viewer"}, n, trueWithErrorFollowingRepo{}),
		"follow 判定が失敗したら followers note は見せない")
}
