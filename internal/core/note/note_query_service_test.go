package note_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newQueryService(t *testing.T) (*note.QueryService, *testutil.MockNoteRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	svc := note.NewQueryService(noteRepo, followingRepo)
	return svc, noteRepo, followingRepo
}

func TestQueryService_Show_NotFound(t *testing.T) {
	svc, _, _ := newQueryService(t)
	_, err := svc.Show(nil, "missing")
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

func TestQueryService_Show_Hidden(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers}
	_, err := svc.Show(&model.User{ID: "viewer"}, "n1")
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

func TestQueryService_Show_Visible(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityPublic}
	got, err := svc.Show(nil, "n1")
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

// ShowForAPI は upstream Misskey TS の notes/show 互換挙動 (#799)。
// visibility 違反でも note を返す (= ID 指定の lookup は公開する設計)。
func TestQueryService_ShowForAPI_ReturnsFollowersNoteToStranger(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers}
	got, err := svc.ShowForAPI("n1")
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

func TestQueryService_ShowForAPI_ReturnsSpecifiedNoteToStranger(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilitySpecified}
	got, err := svc.ShowForAPI("n1")
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

// 不存在は ShowForAPI でも ErrNoteNotFound (= 404 path)。
func TestQueryService_ShowForAPI_NotFound(t *testing.T) {
	svc, _, _ := newQueryService(t)
	_, err := svc.ShowForAPI("missing")
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

func TestQueryService_ShowForAPIOnPrimary_ReturnsStoredNote(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilitySpecified}
	got, err := svc.ShowForAPIOnPrimary("n1")
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

// RequireVisible は #1443 で追加した public wrapper。favorites/create 等の
// mutation endpoint が「存在 + 閲覧可」を 1 ステップで確認するために使う。
func TestQueryService_RequireVisible_NotFound(t *testing.T) {
	svc, _, _ := newQueryService(t)
	_, err := svc.RequireVisible(&model.User{ID: "viewer"}, "missing")
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

func TestQueryService_RequireVisible_HiddenFromStranger(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers}
	_, err := svc.RequireVisible(&model.User{ID: "viewer"}, "n1")
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

func TestQueryService_RequireVisible_VisibleToFollower(t *testing.T) {
	svc, noteRepo, followingRepo := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers}
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "viewer", FolloweeID: "author"}
	got, err := svc.RequireVisible(&model.User{ID: "viewer"}, "n1")
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

func TestQueryService_RequireVisible_PublicVisibleToAnonymous(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityPublic}
	got, err := svc.RequireVisible(nil, "n1")
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

func TestQueryService_ListRenotes(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "a", Visibility: model.NoteVisibilityPublic}
	parentID := "parent"
	noteRepo.Notes["r1"] = &model.Note{ID: "r1", UserID: "b", Visibility: model.NoteVisibilityPublic, RenoteID: &parentID}

	out, err := svc.ListRenotes(nil, "parent", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestQueryService_ListRenotes_ParentMissing(t *testing.T) {
	svc, _, _ := newQueryService(t)
	_, err := svc.ListRenotes(nil, "ghost", "", "", 10)
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

func TestQueryService_ListReplies(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "a", Visibility: model.NoteVisibilityPublic}
	parentID := "parent"
	noteRepo.Notes["r1"] = &model.Note{ID: "r1", UserID: "b", Visibility: model.NoteVisibilityPublic, ReplyID: &parentID}

	out, err := svc.ListReplies(nil, "parent", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

// #1929: 親 note が存在しない/不可視でも 404 ではなく空リストを返す
// (upstream replies.ts は親チェックを行わない)。
func TestQueryService_ListReplies_ParentMissing(t *testing.T) {
	svc, _, _ := newQueryService(t)
	out, err := svc.ListReplies(nil, "ghost", "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestQueryService_ListChildren(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "a", Visibility: model.NoteVisibilityPublic}
	parentID := "parent"
	quoteText := "quote"
	noteRepo.Notes["c1"] = &model.Note{ID: "c1", UserID: "b", Visibility: model.NoteVisibilityPublic, ReplyID: &parentID}
	// quote renote (text あり) は child として返る。
	noteRepo.Notes["c2"] = &model.Note{ID: "c2", UserID: "c", Visibility: model.NoteVisibilityPublic, RenoteID: &parentID, Text: &quoteText}

	out, err := svc.ListChildren(nil, "parent", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

// TestQueryService_ListChildren_PureRenoteExcluded pins that a pure renote
// (no text/file/poll) of the parent is excluded from children, matching
// upstream notes/children (children.ts:53-67). Regression guard for #1554.
func TestQueryService_ListChildren_PureRenoteExcluded(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "a", Visibility: model.NoteVisibilityPublic}
	parentID := "parent"
	quoteText := "quote"
	noteRepo.Notes["reply"] = &model.Note{ID: "reply", UserID: "b", Visibility: model.NoteVisibilityPublic, ReplyID: &parentID}
	noteRepo.Notes["pure"] = &model.Note{ID: "pure", UserID: "c", Visibility: model.NoteVisibilityPublic, RenoteID: &parentID}
	noteRepo.Notes["quote"] = &model.Note{ID: "quote", UserID: "d", Visibility: model.NoteVisibilityPublic, RenoteID: &parentID, Text: &quoteText}

	out, err := svc.ListChildren(nil, "parent", "", "", 10)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, n := range out {
		got[n.ID] = true
	}
	assert.Equal(t, map[string]bool{"reply": true, "quote": true}, got)
	assert.NotContains(t, got, "pure")
}

// #1929: children も親不在/不可視で 404 ではなく空リスト (upstream children.ts)。
func TestQueryService_ListChildren_ParentMissing(t *testing.T) {
	svc, _, _ := newQueryService(t)
	out, err := svc.ListChildren(nil, "ghost", "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// #1929: 親が viewer に不可視 (followers-only) でも 404 にせず、reply 自体の
// 可視性で filter した結果を返す (upstream は親チェックを行わない)。public reply は
// 返り、followers-only reply は匿名 viewer から filter される (= leak しない)。
func TestQueryService_ListReplies_InvisibleParentStillFiltersReplies(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	// 親 P は followers-only (匿名 viewer には不可視)。
	noteRepo.Notes["P"] = &model.Note{ID: "P", UserID: "a", Visibility: model.NoteVisibilityFollowers}
	pid := "P"
	noteRepo.Notes["pub"] = &model.Note{ID: "pub", UserID: "b", Visibility: model.NoteVisibilityPublic, ReplyID: &pid}
	noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "a", Visibility: model.NoteVisibilityFollowers, ReplyID: &pid}

	// 匿名 viewer: 親不可視でも 404 にならず、public reply のみ返る (followers reply は filter)。
	out, err := svc.ListReplies(nil, "P", "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "pub", out[0].ID, "不可視親でも public reply は返る / followers reply は leak しない")
}

// #1500: visibility は repository の SQL push-down に委譲される。QueryService は
// viewer の ID を repo に正しく渡すだけ (post-fetch filterVisible は撤去)。
// public parent への followers reply は、author を follow していない viewer には
// 返らず、follow すると返ることで viewerID 伝播 + push-down の委譲を固定する。
func TestQueryService_ListReplies_VisibilityDelegatedToRepo(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "a", Visibility: model.NoteVisibilityPublic}
	parentID := "parent"
	noteRepo.Notes["pub"] = &model.Note{ID: "pub", UserID: "a", Visibility: model.NoteVisibilityPublic, ReplyID: &parentID}
	noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "a", Visibility: model.NoteVisibilityFollowers, ReplyID: &parentID}

	viewer := &model.User{ID: "v"}
	// 非フォロワー: followers reply は見えない (push-down が効いている)。
	out, err := svc.ListReplies(viewer, "parent", "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "pub", out[0].ID)

	// follow すると followers reply も返る。
	noteRepo.Following = map[string][]string{"v": {"a"}}
	out, err = svc.ListReplies(viewer, "parent", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

// #1500 audit follow-up: ListRenotes も同じ doctrine (push-down delegation +
// viewerID 伝播) なので symmetric な regression test を置く (#1491 audit 指摘 3)。
//
// 注意: 本 test が直接捕捉できるのは「QueryService が viewerID を repo に
// 渡し忘れて空文字で叩いた」regression のみ。「post-fetch FilterVisible が
// 復活した」regression は、mock がすでに同じ条件で pre-filter している関係上
// 同じ結果セットになって検出できない (#1504 adversarial review 指摘)。
// それでも viewerID 伝播の固定として意味があるので残す。
func TestQueryService_ListRenotes_VisibilityDelegatedToRepo(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "a", Visibility: model.NoteVisibilityPublic}
	parentID := "parent"
	noteRepo.Notes["pub"] = &model.Note{ID: "pub", UserID: "a", Visibility: model.NoteVisibilityPublic, RenoteID: &parentID}
	noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "a", Visibility: model.NoteVisibilityFollowers, RenoteID: &parentID}

	viewer := &model.User{ID: "v"}
	out, err := svc.ListRenotes(viewer, "parent", "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "pub", out[0].ID, "非フォロワーには followers renote が見えない (push-down 委譲)")

	noteRepo.Following = map[string][]string{"v": {"a"}}
	out, err = svc.ListRenotes(viewer, "parent", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2, "follow すると followers renote も返る (viewerID 伝播)")
}

// ListChildren も同じ doctrine。children は reply + quote renote の合算なので
// followers reply / followers renote 両方の visibility delegation を覆う。
func TestQueryService_ListChildren_VisibilityDelegatedToRepo(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "a", Visibility: model.NoteVisibilityPublic}
	parentID := "parent"
	quoteText := "quote"
	noteRepo.Notes["pub_reply"] = &model.Note{ID: "pub_reply", UserID: "a", Visibility: model.NoteVisibilityPublic, ReplyID: &parentID}
	noteRepo.Notes["fol_reply"] = &model.Note{ID: "fol_reply", UserID: "a", Visibility: model.NoteVisibilityFollowers, ReplyID: &parentID}
	// quote renote (text あり) として pure renote 除外 (#1554) に引っかからないようにする。
	noteRepo.Notes["fol_quote"] = &model.Note{ID: "fol_quote", UserID: "a", Visibility: model.NoteVisibilityFollowers, RenoteID: &parentID, Text: &quoteText}

	viewer := &model.User{ID: "v"}
	out, err := svc.ListChildren(viewer, "parent", "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 1, "非フォロワーには followers reply/quote が見えない")
	assert.Equal(t, "pub_reply", out[0].ID)

	noteRepo.Following = map[string][]string{"v": {"a"}}
	out, err = svc.ListChildren(viewer, "parent", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 3, "follow すると followers reply + followers quote も返る")
}

func TestQueryService_Conversation_Empty(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["root"] = &model.Note{ID: "root", UserID: "a", Visibility: model.NoteVisibilityPublic}

	out, err := svc.Conversation(nil, "root", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestQueryService_Conversation_Walks(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["g"] = &model.Note{ID: "g", UserID: "a", Visibility: model.NoteVisibilityPublic}
	gID := "g"
	noteRepo.Notes["p"] = &model.Note{ID: "p", UserID: "a", Visibility: model.NoteVisibilityPublic, ReplyID: &gID}
	pID := "p"
	noteRepo.Notes["c"] = &model.Note{ID: "c", UserID: "a", Visibility: model.NoteVisibilityPublic, ReplyID: &pID}

	out, err := svc.Conversation(nil, "c", 10, 0)
	require.NoError(t, err)
	require.Len(t, out, 2)
	// nearest-first (直接の親が先頭、upstream conversation.ts、#1948-17)
	assert.Equal(t, "p", out[0].ID)
	assert.Equal(t, "g", out[1].ID)
}

func TestQueryService_Conversation_DefaultLimit(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["root"] = &model.Note{ID: "root", UserID: "a", Visibility: model.NoteVisibilityPublic}
	rootID := "root"
	noteRepo.Notes["c"] = &model.Note{ID: "c", UserID: "a", Visibility: model.NoteVisibilityPublic, ReplyID: &rootID}

	out, err := svc.Conversation(nil, "c", 0, 0)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestQueryService_Conversation_LimitRespected(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	// チェーン: a -> b -> c -> d (cはbに、dはcに、bはaに返信)
	noteRepo.Notes["a"] = &model.Note{ID: "a", UserID: "u", Visibility: model.NoteVisibilityPublic}
	aID := "a"
	noteRepo.Notes["b"] = &model.Note{ID: "b", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &aID}
	bID := "b"
	noteRepo.Notes["c"] = &model.Note{ID: "c", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &bID}
	cID := "c"
	noteRepo.Notes["d"] = &model.Note{ID: "d", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &cID}

	out, err := svc.Conversation(nil, "d", 2, 0)
	require.NoError(t, err)
	require.Len(t, out, 2)
	// 直近の親2件: c, b。nearest-first なので c, b (#1948-17)
	assert.Equal(t, "c", out[0].ID)
	assert.Equal(t, "b", out[1].ID)
}

// #1554 offset は note に近い親を skip する (upstream conversation.ts:72-74)。
func TestQueryService_Conversation_Offset(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	// d -> c -> b -> a (d が leaf)。親チェーンは c(最近), b, a(最古)。
	noteRepo.Notes["a"] = &model.Note{ID: "a", UserID: "u", Visibility: model.NoteVisibilityPublic}
	aID := "a"
	noteRepo.Notes["b"] = &model.Note{ID: "b", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &aID}
	bID := "b"
	noteRepo.Notes["c"] = &model.Note{ID: "c", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &bID}
	cID := "c"
	noteRepo.Notes["d"] = &model.Note{ID: "d", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &cID}

	// offset=1: 最近の親 c を skip → b, a を収集。nearest-first で b, a (#1948-17)。
	out, err := svc.Conversation(nil, "d", 10, 1)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "b", out[0].ID)
	assert.Equal(t, "a", out[1].ID)

	// offset=1 + limit=1: c skip → b のみ。
	out, err = svc.Conversation(nil, "d", 1, 1)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "b", out[0].ID)
}

// #1948-17: 閲覧不可な祖先は walk を打ち切らず raw に含める (stub 化は handler の
// 責務)。以前は CanSeeNote で break して可視な祖先まで truncate していた。
func TestQueryService_Conversation_HiddenAncestorIncludedRaw(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	// チェーン: leaf(public) -> secret(followers, 不可視) -> gp(public)。
	gpID := "gp"
	noteRepo.Notes["gp"] = &model.Note{ID: "gp", UserID: "author", Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["secret"] = &model.Note{ID: "secret", UserID: "author", Visibility: model.NoteVisibilityFollowers, ReplyID: &gpID}
	secretID := "secret"
	noteRepo.Notes["leaf"] = &model.Note{ID: "leaf", UserID: "viewer", Visibility: model.NoteVisibilityPublic, ReplyID: &secretID}

	out, err := svc.Conversation(&model.User{ID: "viewer"}, "leaf", 10, 0)
	require.NoError(t, err)
	// 不可視 secret で打ち切らず、その先の可視 gp も返る (nearest-first)。
	require.Len(t, out, 2)
	assert.Equal(t, "secret", out[0].ID, "不可視祖先も raw に含む (handler が stub する、#1948-17)")
	assert.Equal(t, "gp", out[1].ID, "不可視祖先の先の可視 note も truncate しない (#1948-17)")
}

func TestQueryService_Conversation_StartMissing(t *testing.T) {
	svc, _, _ := newQueryService(t)
	_, err := svc.Conversation(nil, "ghost", 10, 0)
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

func TestQueryService_Conversation_CycleSafe(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	// 自己ループ: aがa自身に返信(無効データだが防御する)
	aID := "a"
	noteRepo.Notes["a"] = &model.Note{ID: "a", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &aID}

	out, err := svc.Conversation(nil, "a", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestQueryService_Conversation_ParentMissingTerminates(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	// "leaf"はghost親への返信。 親が見つからないのでチェーンが切れる
	ghostID := "ghost"
	noteRepo.Notes["leaf"] = &model.Note{ID: "leaf", UserID: "u", Visibility: model.NoteVisibilityPublic, ReplyID: &ghostID}

	out, err := svc.Conversation(nil, "leaf", 10, 0)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestQueryService_State_OK(t *testing.T) {
	svc, noteRepo, _ := newQueryService(t)
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "a", Visibility: model.NoteVisibilityPublic}

	st, err := svc.State(nil, "n1")
	require.NoError(t, err)
	assert.False(t, st.IsFavorited)
	assert.False(t, st.IsMutedThread)
}

func TestQueryService_State_NotFound(t *testing.T) {
	svc, _, _ := newQueryService(t)
	_, err := svc.State(nil, "ghost")
	require.ErrorIs(t, err, note.ErrNoteNotFound)
}

// failingRepoForList stubs out a single list method to return an error so
// QueryServiceの伝搬パスを検証できる。
type failingRepoForList struct {
	*testutil.MockNoteRepository
	mode string
}

func (f *failingRepoForList) ListRenotesOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	if f.mode == "renotes" {
		return nil, errors.New("boom")
	}
	return f.MockNoteRepository.ListRenotesOf("", "", "", "", 0)
}

func (f *failingRepoForList) ListRepliesOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	if f.mode == "replies" {
		return nil, errors.New("boom")
	}
	return f.MockNoteRepository.ListRepliesOf("", "", "", "", 0)
}

func (f *failingRepoForList) ListChildrenOf(_, _, _, _ string, _ int) ([]*model.Note, error) {
	if f.mode == "children" {
		return nil, errors.New("boom")
	}
	return f.MockNoteRepository.ListChildrenOf("", "", "", "", 0)
}

func TestQueryService_ListRenotes_RepoError(t *testing.T) {
	mock := testutil.NewMockNoteRepository()
	mock.Notes["p"] = &model.Note{ID: "p", UserID: "a", Visibility: model.NoteVisibilityPublic}
	repo := &failingRepoForList{MockNoteRepository: mock, mode: "renotes"}
	svc := note.NewQueryService(repo, nil)
	_, err := svc.ListRenotes(nil, "p", "", "", 10)
	require.Error(t, err)
}

func TestQueryService_ListReplies_RepoError(t *testing.T) {
	mock := testutil.NewMockNoteRepository()
	mock.Notes["p"] = &model.Note{ID: "p", UserID: "a", Visibility: model.NoteVisibilityPublic}
	repo := &failingRepoForList{MockNoteRepository: mock, mode: "replies"}
	svc := note.NewQueryService(repo, nil)
	_, err := svc.ListReplies(nil, "p", "", "", 10)
	require.Error(t, err)
}

func TestQueryService_ListChildren_RepoError(t *testing.T) {
	mock := testutil.NewMockNoteRepository()
	mock.Notes["p"] = &model.Note{ID: "p", UserID: "a", Visibility: model.NoteVisibilityPublic}
	repo := &failingRepoForList{MockNoteRepository: mock, mode: "children"}
	svc := note.NewQueryService(repo, nil)
	_, err := svc.ListChildren(nil, "p", "", "", 10)
	require.Error(t, err)
}

func TestQueryService_State_AnonymousIsAllFalse(t *testing.T) {
	mock := testutil.NewMockNoteRepository()
	mock.Notes["n1"] = &model.Note{ID: "n1", UserID: "a", Visibility: model.NoteVisibilityPublic}
	svc := note.NewQueryService(mock, nil)
	svc.SetFavoriteRepo(testutil.NewMockNoteFavoriteRepository())
	svc.SetThreadMutingRepo(testutil.NewMockNoteThreadMutingRepository())

	state, err := svc.State(nil, "n1")
	require.NoError(t, err)
	assert.False(t, state.IsFavorited)
	assert.False(t, state.IsMutedThread)
}

func TestQueryService_State_FavoritedTrue(t *testing.T) {
	mock := testutil.NewMockNoteRepository()
	mock.Notes["n1"] = &model.Note{ID: "n1", UserID: "a", Visibility: model.NoteVisibilityPublic}
	svc := note.NewQueryService(mock, nil)
	favRepo := testutil.NewMockNoteFavoriteRepository()
	require.NoError(t, favRepo.Create(&model.NoteFavorite{UserID: "viewer", NoteID: "n1"}))
	svc.SetFavoriteRepo(favRepo)

	state, err := svc.State(&model.User{ID: "viewer"}, "n1")
	require.NoError(t, err)
	assert.True(t, state.IsFavorited)
	assert.False(t, state.IsMutedThread)
}

func TestQueryService_State_MutedThreadTrue(t *testing.T) {
	mock := testutil.NewMockNoteRepository()
	rootID := "thread-root"
	mock.Notes["child"] = &model.Note{ID: "child", UserID: "a", ThreadID: &rootID, Visibility: model.NoteVisibilityPublic}
	svc := note.NewQueryService(mock, nil)
	mutes := testutil.NewMockNoteThreadMutingRepository()
	require.NoError(t, mutes.Create(&model.NoteThreadMuting{UserID: "viewer", ThreadID: rootID}))
	svc.SetThreadMutingRepo(mutes)

	state, err := svc.State(&model.User{ID: "viewer"}, "child")
	require.NoError(t, err)
	assert.True(t, state.IsMutedThread)
}

func TestQueryService_State_NoteNotFound(t *testing.T) {
	svc := note.NewQueryService(testutil.NewMockNoteRepository(), nil)
	_, err := svc.State(&model.User{ID: "viewer"}, "missing")
	require.Error(t, err)
}
