package transfer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/transfer"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- data-driven edge branches ---

// #1555 export-following の excludeMuting=true は muted followee を除外する。
func TestExport_Following_ExcludeMuting(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	fRepo := deps.FollowingRepo.(*testutil.MockFollowingRepository)
	fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: user.ID, FolloweeID: "bob"}
	fRepo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: user.ID, FolloweeID: "carol"}
	mRepo := deps.MutingRepo.(*testutil.MockMutingRepository)
	mRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: user.ID, MuteeID: "carol"}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFollowing,
		transfer.WithExcludeMuting(true))
	require.NoError(t, err)
	body := string(saver.uploads[0].Body)
	assert.Contains(t, body, "bob")
	assert.NotContains(t, body, "carol") // muted なので除外
}

// #1555 excludeInactive=true は updatedAt が 90日より古い followee を除外し、
// updatedAt が nil の followee は除外しない (upstream の `u.updatedAt &&` guard)。
func TestExport_Following_ExcludeInactive(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	userRepo := deps.UserRepo.(*testutil.MockUserRepository)
	old := time.Now().Add(-100 * 24 * time.Hour)
	recent := time.Now().Add(-1 * 24 * time.Hour)
	userRepo.Users["stale"] = &model.User{ID: "stale", Username: "stale", UsernameLower: "stale", UpdatedAt: &old}
	userRepo.Users["fresh"] = &model.User{ID: "fresh", Username: "fresh", UsernameLower: "fresh", UpdatedAt: &recent}
	// bob は updatedAt nil → inactive 扱いしない (除外されない)。
	fRepo := deps.FollowingRepo.(*testutil.MockFollowingRepository)
	fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: user.ID, FolloweeID: "stale"}
	fRepo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: user.ID, FolloweeID: "fresh"}
	fRepo.Followings["f3"] = &model.Following{ID: "f3", FollowerID: user.ID, FolloweeID: "bob"}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFollowing,
		transfer.WithExcludeInactive(true))
	require.NoError(t, err)
	body := string(saver.uploads[0].Body)
	assert.NotContains(t, body, "stale") // 90日以上 inactive → 除外
	assert.Contains(t, body, "fresh")
	assert.Contains(t, body, "bob") // updatedAt nil → 除外しない
}

// 存在しない followee ID を含む following 行を追加して、
// `u, err := UserRepo.FindByID; if err != nil { continue }` の経路をカバーする。
func TestExport_Following_SkipsMissingUser(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	fRepo := deps.FollowingRepo.(*testutil.MockFollowingRepository)
	fRepo.Followings["f1"] = &model.Following{
		ID: "f1", FollowerID: user.ID, FolloweeID: "ghost", // userRepo に居ない
	}
	fRepo.Followings["f2"] = &model.Following{
		ID: "f2", FollowerID: user.ID, FolloweeID: "bob",
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFollowing)
	require.NoError(t, err)

	body := string(saver.uploads[0].Body)
	// 存在しない ghost はスキップされ、bob だけ出力される
	assert.NotContains(t, body, "ghost")
	assert.Contains(t, body, "bob")
}

// 存在しない blockee ID を含む blocking 行を追加して、
// 該当ブランチ `continue` 経路をカバーする。
func TestExport_Blocking_SkipsMissingUser(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	bRepo := deps.BlockingRepo.(*testutil.MockBlockingRepository)
	bRepo.Blockings["b1"] = &model.Blocking{
		ID: "b1", BlockerID: user.ID, BlockeeID: "ghost",
	}
	bRepo.Blockings["b2"] = &model.Blocking{
		ID: "b2", BlockerID: user.ID, BlockeeID: "bob",
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportBlocking)
	require.NoError(t, err)

	body := string(saver.uploads[0].Body)
	assert.NotContains(t, body, "ghost")
	assert.Contains(t, body, "bob")
}

// ExpiresAt が設定された muting は永続でないのでエクスポートされない。
// またミュート対象ユーザーが見つからないケースも同時にカバーする。
func TestExport_Muting_SkipsExpiringAndMissingUser(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	mRepo := deps.MutingRepo.(*testutil.MockMutingRepository)
	expiry := time.Now().Add(24 * time.Hour)
	mRepo.Mutings["m1"] = &model.Muting{
		ID: "m1", MuterID: user.ID, MuteeID: "bob", ExpiresAt: &expiry,
	}
	// ghost: muting 対象が userRepo に不在 → スキップ
	mRepo.Mutings["m2"] = &model.Muting{
		ID: "m2", MuterID: user.ID, MuteeID: "ghost",
	}
	// bob (permanent): 出力される
	mRepo.Mutings["m3"] = &model.Muting{
		ID: "m3", MuterID: user.ID, MuteeID: "bob",
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportMuting)
	require.NoError(t, err)

	body := string(saver.uploads[0].Body)
	assert.NotContains(t, body, "ghost")
	assert.Contains(t, body, "bob")
}

// Favorites: Note == nil のケースをスキップする経路をカバーする。
func TestExport_Favorites_SkipsNilNote(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	favRepo := deps.NoteFavoriteRepo.(*testutil.MockNoteFavoriteRepository)
	// Note フィールドが nil の favorite → スキップ
	favRepo.Favorites["fav-nil"] = &model.NoteFavorite{
		ID: "fav-nil", UserID: user.ID, NoteID: "missing",
		// Note は明示的に nil
	}
	text := "hello"
	// Note 付きの favorite → 出力される
	favRepo.Favorites["fav-ok"] = &model.NoteFavorite{
		ID: "fav-ok", UserID: user.ID, NoteID: "n1",
		Note: &model.Note{ID: "n1", UserID: user.ID, Text: &text},
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFavorites)
	require.NoError(t, err)

	body := saver.uploads[0].Body
	assert.Contains(t, string(body), "fav-ok")
	// nil Note がスキップされたことを JSON パースで確認
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(body, &arr))
	require.Len(t, arr, 1)
}

// Antennas: userListId が設定されたケースを追加する。
func TestExport_Antennas_WithUserList(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	aRepo := deps.AntennaRepo.(*testutil.MockAntennaRepository)
	listID := "list1"
	aRepo.Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: user.ID, Name: "news",
		Src:             "list",
		UserListID:      &listID,
		Keywords:        []byte(`[]`),
		ExcludeKeywords: []byte(`[]`),
		Users:           model.StringArray{},
	}
	deps.UserListRepo.(*testutil.MockUserListRepository).Lists[listID] = &model.UserList{ID: listID, UserID: user.ID, Name: "mine"}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportAntennas)
	require.NoError(t, err)
	assert.Contains(t, string(saver.uploads[0].Body), "list1")
}

// Clips with notes: collectClipNotes 経路を exercise する。
func TestExport_Clips_WithNotes(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	clipRepo := deps.ClipRepo.(*testutil.MockClipRepository)
	clipNoteRepo := deps.ClipNoteRepo.(*testutil.MockClipNoteRepository)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)

	clipRepo.Clips["c1"] = &model.Clip{ID: "c1", UserID: user.ID, Name: "favs", IsPublic: true}
	clipNoteRepo.Entries["link1"] = &model.ClipNote{ID: "link1", ClipID: "c1", NoteID: "n1"}
	// 見つからない noteID も追加して FindByIDWithUser の失敗経路を exercise
	clipNoteRepo.Entries["link2"] = &model.ClipNote{ID: "link2", ClipID: "c1", NoteID: "missing"}

	text := "clipped"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: user.ID, Text: &text}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportClips)
	require.NoError(t, err)

	body := string(saver.uploads[0].Body)
	assert.Contains(t, body, "c1")
	assert.Contains(t, body, "clipped")
	assert.NotContains(t, body, "missing")
}

// TestExport_Clips_MultiPage は #1950 の export 経路回帰テスト。collectClipNotes は
// ListByClip を keyset でページネーションするが、#1950 で比較列が note.id になったため
// 次ページの cursor も note.id を渡さなければならない (clip_note.id を渡すと無関係 ULID
// で比較され 2 ページ目が空になり全件 export できない)。clip_note.id と note.id の prefix
// を変えて cross-column バグを顕在化させ、101 件超でも全件 export されることを検証する。
func TestExport_Clips_MultiPage(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	clipRepo := deps.ClipRepo.(*testutil.MockClipRepository)
	clipNoteRepo := deps.ClipNoteRepo.(*testutil.MockClipNoteRepository)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)

	clipRepo.Clips["c1"] = &model.Clip{ID: "c1", UserID: user.ID, Name: "big", IsPublic: true}

	const total = 150 // > 1 ページ (100)
	for i := 1; i <= total; i++ {
		noteID := fmt.Sprintf("n_%03d", i)
		// clip_note.id は note.id と prefix を変える (cross-column バグを露出させる)。
		clipNoteRepo.Entries[fmt.Sprintf("link_%03d", i)] = &model.ClipNote{
			ID: fmt.Sprintf("link_%03d", i), ClipID: "c1", NoteID: noteID,
		}
		text := "n" + noteID
		noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: user.ID, Text: &text}
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportClips)
	require.NoError(t, err)

	body := string(saver.uploads[0].Body)
	missing := 0
	for i := 1; i <= total; i++ {
		if !strings.Contains(body, fmt.Sprintf(`"%s"`, fmt.Sprintf("n_%03d", i))) {
			missing++
		}
	}
	assert.Equal(t, 0, missing, "all %d clipped notes must be exported across pages", total)
}

// UserLists: members without preloaded User trigger the UserRepo.FindByID fallback.
func TestExport_UserLists_FallsBackToUserRepo(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	listRepo := deps.UserListRepo.(*testutil.MockUserListRepository)
	listRepo.Lists["list1"] = &model.UserList{ID: "list1", UserID: user.ID, Name: "favs"}
	// Membership without User ptr → UserRepo.FindByID で解決
	listRepo.Members = append(listRepo.Members,
		&model.UserListMembership{ID: "m1", UserListID: "list1", UserID: "bob"},
		// 存在しないユーザー → スキップ
		&model.UserListMembership{ID: "m2", UserListID: "list1", UserID: "ghost"},
	)

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportUserLists)
	require.NoError(t, err)

	body := string(saver.uploads[0].Body)
	assert.Contains(t, body, "bob")
	assert.NotContains(t, body, "ghost")
	// CSV line count check
	lines := strings.Split(strings.TrimSpace(body), "\n")
	assert.Len(t, lines, 1)
}

// #2106 N24: list source antenna は member を acct 解決して userListAccts に出力し、
// excludeNotesInSensitiveChannel も含める (cross-instance import 互換)。
func TestExport_Antennas_UserListAccts(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	aRepo := deps.AntennaRepo.(*testutil.MockAntennaRepository)
	listRepo := deps.UserListRepo.(*testutil.MockUserListRepository)
	userRepo := deps.UserRepo.(*testutil.MockUserRepository)
	listID := "list1"
	aRepo.Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: user.ID, Name: "news", Src: "list", UserListID: &listID,
		Keywords: []byte(`[]`), ExcludeKeywords: []byte(`[]`), Users: model.StringArray{},
		ExcludeNotesInSensitiveChannel: true,
	}
	listRepo.Lists[listID] = &model.UserList{ID: listID, UserID: user.ID, Name: "src"}
	host := "remote.example"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}                    // local → "bob"
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol", Host: &host} // remote → "carol@remote.example"
	require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "m1", UserListID: listID, UserID: "bob"}))
	require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "m2", UserListID: listID, UserID: "carol"}))

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportAntennas)
	require.NoError(t, err)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &out))
	require.Len(t, out, 1)
	accts, ok := out[0]["userListAccts"].([]any)
	require.True(t, ok, "list source は userListAccts を配列で出す")
	assert.ElementsMatch(t, []any{"bob", "carol@remote.example"}, accts)
	assert.Equal(t, true, out[0]["excludeNotesInSensitiveChannel"])
}

// 1aac65d より前に保存された他人の userListId が残っていても、export はその
// list の ID もメンバーも出さない。
func TestExport_Antennas_OtherUsersListIsNotResolved(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	aRepo := deps.AntennaRepo.(*testutil.MockAntennaRepository)
	listRepo := deps.UserListRepo.(*testutil.MockUserListRepository)
	listID := "victimlist"
	aRepo.Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: user.ID, Name: "news", Src: "list", UserListID: &listID,
		Keywords: []byte(`[]`), ExcludeKeywords: []byte(`[]`), Users: model.StringArray{},
	}
	listRepo.Lists[listID] = &model.UserList{ID: listID, UserID: "bob", Name: "bob's secret list"}
	require.NoError(t, listRepo.AddMember(&model.UserListMembership{ID: "m1", UserListID: listID, UserID: "carol"}))

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportAntennas)
	require.NoError(t, err)
	body := string(saver.uploads[0].Body)
	assert.NotContains(t, body, listID)
	assert.NotContains(t, body, "carol")
	var out []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &out))
	require.Len(t, out, 1)
	assert.Nil(t, out[0]["userListAccts"])
}

type failingFindListRepo struct {
	*testutil.MockUserListRepository
}

func (failingFindListRepo) FindByID(string) (*model.UserList, error) {
	return nil, errors.New("db down")
}

// list の lookup 失敗は「他人の list」と取り違えず、エラーとして返す (#2792)。
func TestExport_Antennas_ListLookupErrorPropagates(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.UserListRepo = failingFindListRepo{MockUserListRepository: testutil.NewMockUserListRepository()}
	listID := "l1"
	deps.AntennaRepo.(*testutil.MockAntennaRepository).Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: user.ID, Name: "news", Src: "list", UserListID: &listID,
		Keywords: []byte(`[]`), ExcludeKeywords: []byte(`[]`), Users: model.StringArray{},
	}

	_, err := transfer.NewExporter(deps).Export(context.Background(), user.ID, transfer.ExportAntennas)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db down")
}
