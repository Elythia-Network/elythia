package testutil

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper: 文字列ポインタ
func sp(v string) *string { return &v }

// helper: bool ポインタ (TimelineDBFilter.WithRenotes に渡す)
func bp(v bool) *bool { return &v }

// helper: list 化された ID 集合を返す
func ids(notes []*model.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}

// MockNoteRepository.ListByUserList が real repo の SQL push-down と同
// semantics を持つことを確認する parity test (#1491 audit 指摘 1)。
// 旧 stub は (nil, nil) を返すだけで、real SQL の挙動 (#1452 visibility /
// #1496 withReplies / #1498 withRenotes / WithFiles) を全く模擬していなかった。

func TestMockListByUserList_NonMemberDropped(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{
		{UserListID: "l1", UserID: "alice"},
	}
	m.Notes["n_alice"] = &model.Note{ID: "n_alice", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	m.Notes["n_bob"] = &model.Note{ID: "n_bob", UserID: "bob", Visibility: model.NoteVisibilityPublic}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_alice"}, ids(got))
}

func TestMockListByUserList_ChannelExcluded(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	m.Notes["n_pub"] = &model.Note{ID: "n_pub", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	m.Notes["n_ch"] = &model.Note{ID: "n_ch", UserID: "alice", Visibility: model.NoteVisibilityPublic, ChannelID: sp("c1")}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_pub"}, ids(got))
}

func TestMockListByUserList_FollowersVisibility_NonFollowerDropped(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	// viewer は alice を follow していない (Following 未 seed)。
	m.Notes["n_fol"] = &model.Note{ID: "n_fol", UserID: "alice", Visibility: model.NoteVisibilityFollowers}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Empty(t, ids(got))
}

func TestMockListByUserList_FollowersVisibility_FollowerSees(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	m.Following["viewer"] = []string{"alice"}
	m.Notes["n_fol"] = &model.Note{ID: "n_fol", UserID: "alice", Visibility: model.NoteVisibilityFollowers}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_fol"}, ids(got))
}

func TestMockListByUserList_FollowersVisibility_SelfAuthor(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	// viewer = author = alice。Following 無くても本人は閲覧可。
	m.Notes["n_fol"] = &model.Note{ID: "n_fol", UserID: "alice", Visibility: model.NoteVisibilityFollowers}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "alice"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_fol"}, ids(got))
}

func TestMockListByUserList_FollowersVisibility_AnonymousDropped(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	m.Notes["n_fol"] = &model.Note{ID: "n_fol", UserID: "alice", Visibility: model.NoteVisibilityFollowers}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: ""})
	require.NoError(t, err)
	assert.Empty(t, ids(got))
}

func TestMockListByUserList_SpecifiedExcluded(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	// viewer は宛先本人。それでも list timeline は specified (DM) を出さない。
	m.Notes["n_dm"] = &model.Note{
		ID: "n_dm", UserID: "alice",
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: []string{"viewer"},
	}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Empty(t, ids(got))
}

func TestMockListByUserList_Reply_WithRepliesFalse_OnlySelfThreadVisible(t *testing.T) {
	m := NewMockNoteRepository()
	// alice は WithReplies=false で list に居る。
	m.UserListMembers["l1"] = []*model.UserListMembership{
		{UserListID: "l1", UserID: "alice", WithReplies: false},
	}
	m.Notes["n_top"] = &model.Note{ID: "n_top", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	// alice の self-thread reply は通る。
	m.Notes["n_self"] = &model.Note{
		ID: "n_self", UserID: "alice", Visibility: model.NoteVisibilityPublic,
		ReplyID: sp("n_top"), ReplyUserID: sp("alice"),
	}
	// alice → bob (= 第三者) reply は WithReplies=false なので drop。
	m.Notes["n_other"] = &model.Note{
		ID: "n_other", UserID: "alice", Visibility: model.NoteVisibilityPublic,
		ReplyID: sp("n_someone"), ReplyUserID: sp("bob"),
	}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	gotIDs := ids(got)
	assert.Contains(t, gotIDs, "n_top")
	assert.Contains(t, gotIDs, "n_self", "self-thread reply は WithReplies=false でも残る")
	assert.NotContains(t, gotIDs, "n_other", "第三者宛 reply は WithReplies=false で drop")
}

func TestMockListByUserList_Reply_ViewerTargetedSurvives(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{
		{UserListID: "l1", UserID: "alice", WithReplies: false},
	}
	// alice → viewer reply は viewer 宛なので WithReplies=false でも通る。
	m.Notes["n_to_me"] = &model.Note{
		ID: "n_to_me", UserID: "alice", Visibility: model.NoteVisibilityPublic,
		ReplyID: sp("p"), ReplyUserID: sp("viewer"),
	}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_to_me"}, ids(got))
}

func TestMockListByUserList_Reply_WithRepliesTrue_ThirdPartyVisible(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{
		{UserListID: "l1", UserID: "alice", WithReplies: true},
	}
	m.Notes["n_3p"] = &model.Note{
		ID: "n_3p", UserID: "alice", Visibility: model.NoteVisibilityPublic,
		ReplyID: sp("p"), ReplyUserID: sp("bob"),
	}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_3p"}, ids(got))
}

func TestMockListByUserList_WithFilesRequiresAttachment(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	m.Notes["n_text"] = &model.Note{ID: "n_text", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	m.Notes["n_file"] = &model.Note{ID: "n_file", UserID: "alice", Visibility: model.NoteVisibilityPublic, FileIDs: []string{"f1"}}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer", WithFiles: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_file"}, ids(got))
}

func TestMockListByUserList_WithRenotesFalseExcludesPureRenote(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	text := "quote text"
	// pure renote (text=nil, files=空): 除外。
	m.Notes["n_pure"] = &model.Note{
		ID: "n_pure", UserID: "alice", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig1"),
	}
	// quote renote (text あり): 通る。
	m.Notes["n_quote"] = &model.Note{
		ID: "n_quote", UserID: "alice", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig2"), Text: &text,
	}
	// plain note: 通る。
	m.Notes["n_plain"] = &model.Note{ID: "n_plain", UserID: "alice", Visibility: model.NoteVisibilityPublic}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{
		ViewerID:    "viewer",
		WithRenotes: bp(false),
	})
	require.NoError(t, err)
	gotIDs := ids(got)
	assert.NotContains(t, gotIDs, "n_pure", "pure renote は WithRenotes=false で除外")
	assert.Contains(t, gotIDs, "n_quote", "quote renote は WithRenotes=false でも残る")
	assert.Contains(t, gotIDs, "n_plain")
}

func TestMockListByUserList_PaginationUntilID(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	for _, id := range []string{"n1", "n2", "n3"} {
		m.Notes[id] = &model.Note{ID: id, UserID: "alice", Visibility: model.NoteVisibilityPublic}
	}

	got, err := m.ListByUserList("l1", 10, "", "n3", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n2", "n1"}, ids(got), "untilID=n3 で id DESC")
}

func TestMockListByUserList_PaginationSinceID(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	for _, id := range []string{"n1", "n2", "n3"} {
		m.Notes[id] = &model.Note{ID: id, UserID: "alice", Visibility: model.NoteVisibilityPublic}
	}

	got, err := m.ListByUserList("l1", 10, "n1", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Equal(t, []string{"n2", "n3"}, ids(got), "sinceID=n1 で id ASC")
}

func TestMockListByUserList_LimitDefault(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	for i := 0; i < 15; i++ {
		id := "n" + string(rune('a'+i))
		m.Notes[id] = &model.Note{ID: id, UserID: "alice", Visibility: model.NoteVisibilityPublic}
	}

	got, err := m.ListByUserList("l1", 0, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Len(t, got, 10, "limit<=0 のデフォルトは 10")
}

func TestMockListByUserList_EmptyList(t *testing.T) {
	m := NewMockNoteRepository()
	m.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}
	// UserListMembers["l1"] 未設定。

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{ViewerID: "viewer"})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// #1491 review high 指摘: real repo の applyTimelineFilter は
// IncludeMyRenotes / IncludeRenotedMyNotes / IncludeLocalRenotes を適用するが、
// mock 側の旧実装は WithFiles + WithRenotes のみで Include* 系を読まなかった。
// 以下 3 件で各 flag の pure-renote 分岐除外を fix する。これが無いと、handler
// が Include* を停止しても mock 経由のテストでは検出できない (= 旧 (nil, nil)
// stub と同じ silent regression を再導入してしまう)。

func TestMockListByUserList_IncludeMyRenotesFalse_DropsViewerOwnPureRenote(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{
		{UserListID: "l1", UserID: "viewer"},
		{UserListID: "l1", UserID: "other"},
	}
	// viewer の pure renote: 除外される。
	m.Notes["n_mine"] = &model.Note{
		ID: "n_mine", UserID: "viewer", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig1"),
	}
	// 他人の pure renote: 残る。
	m.Notes["n_other"] = &model.Note{
		ID: "n_other", UserID: "other", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig2"),
	}
	// viewer の plain note: 残る (pure renote ではない)。
	m.Notes["n_mine_plain"] = &model.Note{ID: "n_mine_plain", UserID: "viewer", Visibility: model.NoteVisibilityPublic}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{
		ViewerID:         "viewer",
		IncludeMyRenotes: bp(false),
	})
	require.NoError(t, err)
	gotIDs := ids(got)
	assert.NotContains(t, gotIDs, "n_mine", "IncludeMyRenotes=false で viewer 自身の pure renote が除外")
	assert.Contains(t, gotIDs, "n_other")
	assert.Contains(t, gotIDs, "n_mine_plain")
}

func TestMockListByUserList_IncludeRenotedMyNotesFalse_DropsRenoteOfViewer(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{
		{UserListID: "l1", UserID: "other"},
	}
	// other が viewer の note を pure renote: 除外される。
	m.Notes["n_renote_me"] = &model.Note{
		ID: "n_renote_me", UserID: "other", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig1"), RenoteUserID: sp("viewer"),
	}
	// other が誰か他人の note を pure renote: 残る。
	m.Notes["n_renote_3p"] = &model.Note{
		ID: "n_renote_3p", UserID: "other", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig2"), RenoteUserID: sp("bob"),
	}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{
		ViewerID:              "viewer",
		IncludeRenotedMyNotes: bp(false),
	})
	require.NoError(t, err)
	gotIDs := ids(got)
	assert.NotContains(t, gotIDs, "n_renote_me", "IncludeRenotedMyNotes=false で viewer の note の renote が除外")
	assert.Contains(t, gotIDs, "n_renote_3p")
}

func TestMockListByUserList_IncludeLocalRenotesFalse_DropsLocalUserRenote(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{
		{UserListID: "l1", UserID: "other"},
	}
	host := "remote.example"
	// local user (RenoteUserHost=nil) の note を pure renote: 除外される。
	m.Notes["n_local"] = &model.Note{
		ID: "n_local", UserID: "other", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig1"), RenoteUserID: sp("local"), RenoteUserHost: nil,
	}
	// remote user の note を pure renote: 残る。
	m.Notes["n_remote"] = &model.Note{
		ID: "n_remote", UserID: "other", Visibility: model.NoteVisibilityPublic,
		RenoteID: sp("orig2"), RenoteUserID: sp("remote"), RenoteUserHost: &host,
	}

	got, err := m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{
		ViewerID:            "viewer",
		IncludeLocalRenotes: bp(false),
	})
	require.NoError(t, err)
	gotIDs := ids(got)
	assert.NotContains(t, gotIDs, "n_local", "IncludeLocalRenotes=false で local user の pure renote が除外")
	assert.Contains(t, gotIDs, "n_remote")
}

// #1506: muting subquery 系 filter は mock 未実装。docstring の案内だけだと
// silent regression を招くため、これら 3 fields のいずれかが set されたら panic
// で loud-fail させる。下記 3 件で各 entry point の panic 経路を担保する。
// member seeding は最低限 (panic は filter check で前段に出るので member 構成
// は実質関係しないが、real path に近い形で 1 件だけ seed しておく)。

func TestMockListByUserList_PanicsOnUseMutingSubquery(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	m.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}

	require.PanicsWithValue(t,
		"testutil.MockNoteRepository.ListByUserList: muting subquery filter "+
			"(UseMutingSubquery / MutedUserIDs / MutedChannelIDs) is not implemented in this mock. "+
			"Escalate to a dedicated fake such as userListNotesRepo in internal/api/notes/handler_extra_test.go, "+
			"or exercise the real SQL push-down in internal/repository/note.go applyTimelineFilter via a DB-backed test (#1506).",
		func() {
			_, _ = m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{
				ViewerID:          "viewer",
				UseMutingSubquery: true,
			})
		},
	)
}

func TestMockListByUserList_PanicsOnMutedUserIDs(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	m.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}

	require.Panics(t, func() {
		_, _ = m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{
			ViewerID:     "viewer",
			MutedUserIDs: []string{"muted-author"},
		})
	})
}

func TestMockListByUserList_PanicsOnMutedChannelIDs(t *testing.T) {
	m := NewMockNoteRepository()
	m.UserListMembers["l1"] = []*model.UserListMembership{{UserListID: "l1", UserID: "alice"}}
	m.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}

	require.Panics(t, func() {
		_, _ = m.ListByUserList("l1", 10, "", "", model.TimelineDBFilter{
			ViewerID:        "viewer",
			MutedChannelIDs: []string{"ch-muted"},
		})
	})
}
