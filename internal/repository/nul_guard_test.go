package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorable(t *testing.T) {
	assert.True(t, storable("9abcdef0123456"))
	assert.True(t, storable(""), "空文字は列に入る (呼び出し側の既存の扱いを変えない)")
	assert.True(t, storable(strings.Repeat("x", 1000)), "長さは見ない")
	assert.False(t, storable("a\x00b"))
	assert.False(t, storable("\x00"))
}

// **要素ごとに落とす。** `IN (...)` は 1 つでも NUL があればクエリごと落ちるが、
// 残りの id は普通に引けるので、全体を諦めると結果が変わる。
func TestStorableIDs(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{"全部入る", []string{"a", "b"}, []string{"a", "b"}},
		{"nil", nil, nil},
		{"空", []string{}, []string{}},
		{"先頭が NUL", []string{"a\x00", "b", "c"}, []string{"b", "c"}},
		{"途中が NUL", []string{"a", "b\x00", "c"}, []string{"a", "c"}},
		{"末尾が NUL", []string{"a", "b", "c\x00"}, []string{"a", "b"}},
		{"複数が NUL", []string{"a\x00", "b", "c\x00", "d"}, []string{"b", "d"}},
		{"全部 NUL", []string{"a\x00", "b\x00"}, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := storableIDs(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

// 落とす要素が無いときは元の slice をそのまま返す (hot path で確保を増やさない)。
func TestStorableIDsReusesInputWhenNothingDropped(t *testing.T) {
	in := []string{"a", "b", "c"}
	got := storableIDs(in)
	require.Len(t, got, 3)
	got[0] = "z"
	assert.Equal(t, "z", in[0], "確保し直している (落とす要素が無いのに copy している)")
}

// **実 DB で「引く前に弾いている」ことを固定する。** mock では NUL を渡しても
// 落ちないので、guard を外しても mock だけのテストは緑のまま通る。ここは実際に
// PostgreSQL を触るので、guard が無ければ 08P01 / 22021 が返って `IsNotFound` が
// false になり落ちる (#3025)。
func TestFindByIDRejectsUnstorableIDBeforeQuerying(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	const unstorable = "a\x00b"

	t.Run("user", func(t *testing.T) {
		_, err := NewUserRepository(db).FindByID(unstorable)
		require.Error(t, err)
		assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
	})
	t.Run("note", func(t *testing.T) {
		_, err := NewNoteRepository(db).FindByID(unstorable)
		require.Error(t, err)
		assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
	})
	t.Run("drive file", func(t *testing.T) {
		_, err := NewDriveFileRepository(db).FindByID(unstorable)
		require.Error(t, err)
		assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
	})
}

// 複数 id の経路も実 DB で固定する。**入る id は引けたまま**であることまで見る —
// 「NUL が 1 つでもあれば全体を空にする」で塞ぐと、この assert が落ちる。
func TestFindManyByIDsDropsOnlyTheUnstorableID(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	repo := NewUserRepository(db)
	u := &model.User{ID: "storableids1", Username: "storableids1", UsernameLower: "storableids1"}
	require.NoError(t, db.Create(u).Error)
	t.Cleanup(func() { db.Delete(&model.User{}, `"id" = ?`, u.ID) })

	users, err := repo.FindManyByIDs([]string{u.ID, "a\x00b"})
	require.NoError(t, err, "IN に NUL を載せてしまっている")
	require.Len(t, users, 1, "入る id まで落としている")
	assert.Equal(t, u.ID, users[0].ID)
}

// **AND で畳む語は 1 つでも一致しえなければ全体が空。** OR で畳む集合に使うと
// 結果が変わるので、`storableIDs` と使い分ける。
func TestAllStorable(t *testing.T) {
	assert.True(t, allStorable(nil))
	assert.True(t, allStorable([]string{}))
	assert.True(t, allStorable([]string{"a", "b"}))
	assert.False(t, allStorable([]string{"a", "b\x00"}))
	assert.False(t, allStorable([]string{"\x00"}))
}

// emoji v2 の検索語は **OR** で畳むので、NUL を含む語だけを落とす。
// 全部落ちたら空になり、呼び出し側が `1 = 0` に倒す。
func TestMultipleWordsToQueryDropsOnlyUnstorableWords(t *testing.T) {
	assert.Equal(t, []string{"%a%", "%c%"}, multipleWordsToQuery("a b\x00 c"),
		"一致しうる語まで落としている (OR なので他の語の一致は残るべき)")
	assert.Empty(t, multipleWordsToQuery("a\x00 b\x00"),
		"全部落ちたら空にする (呼び出し側が 1 = 0 に倒す)")
	assert.Equal(t, []string{"%a%", "%b%"}, multipleWordsToQuery("a b"))
}

// **実 DB で「引く前に弾いている」ことを固定する。** guard が無ければ LIKE の
// パターンに NUL が乗り、08P01 / 22021 でエラーになる (= handler が 500 に倒す)。
// 検索は「一致しえない」が正しい答えなので、空の結果を返す (#3025)。
func TestSearchRejectsUnmatchableQueryBeforeQuerying(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	const unmatchable = "a\x00b"

	t.Run("users", func(t *testing.T) {
		users, err := NewUserRepository(db).SearchByUsernameAndHost(unmatchable, nil, true, 10)
		require.NoError(t, err, "LIKE のパターンに載せてしまっている")
		assert.Empty(t, users)
	})
	t.Run("instances", func(t *testing.T) {
		rows, err := NewInstanceRepository(db).List(model.InstanceListFilter{Host: unmatchable, Limit: 10})
		require.NoError(t, err, "LIKE のパターンに載せてしまっている")
		assert.Empty(t, rows)
	})
	t.Run("channels", func(t *testing.T) {
		rows, err := NewChannelRepository(db).List(model.ChannelListFilter{Query: unmatchable, Limit: 10})
		require.NoError(t, err, "LIKE のパターンに載せてしまっている")
		assert.Empty(t, rows)
	})
	t.Run("moderation logs", func(t *testing.T) {
		rows, err := NewModerationLogRepository(db).List(model.ModerationLogFilter{Search: unmatchable, Limit: 10})
		require.NoError(t, err, "LIKE のパターンに載せてしまっている")
		assert.Empty(t, rows)
	})
}

// **普通の検索語は通る。** guard を広げすぎて「検索が常に空」になっていないかを
// 見る (空を返すだけの実装でも上のテストは通ってしまう)。
func TestSearchStillMatchesOrdinaryQuery(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	repo := NewUserRepository(db)
	u := &model.User{ID: "nulguardsearch1", Username: "nulguardsearch1", UsernameLower: "nulguardsearch1"}
	require.NoError(t, db.Create(u).Error)
	t.Cleanup(func() { db.Delete(&model.User{}, `"id" = ?`, u.ID) })

	users, err := repo.SearchByUsernameAndHost("nulguardsearch", nil, true, 10)
	require.NoError(t, err)
	require.Len(t, users, 1, "普通の検索語まで空にしている")
	assert.Equal(t, u.ID, users[0].ID)
}

// **単一行の lookup は id 以外の値でも引く前に弾く (#3025 のレビュー H4)。**
// `users/show` と `signin` は `username` を完全一致で引くので、`*ByID*` だけを
// 守っていると**未認証で 500 を起こせる**経路が残る。
func TestSingleRowLookupsRejectUnstorableValues(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	const unstorable = "a\x00b"

	t.Run("username", func(t *testing.T) {
		_, err := NewUserRepository(db).FindByUsernameLower(unstorable, nil)
		require.Error(t, err)
		assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
	})
	t.Run("uri", func(t *testing.T) {
		_, err := NewUserRepository(db).FindByURI(unstorable)
		require.Error(t, err)
		assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
	})
	t.Run("host", func(t *testing.T) {
		_, err := NewInstanceRepository(db).FindByHost(unstorable)
		require.Error(t, err)
		assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
	})
	t.Run("hashtag name", func(t *testing.T) {
		_, err := NewHashtagRepository(db).FindByName(unstorable)
		require.Error(t, err)
		assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
	})
}

// **一覧系にも実際に届く経路がある (#3025 のレビュー H1)。** ここは gate の
// 射程外 (単一行 lookup と LIKE しか見ない) なので、テストが唯一の防波堤になる。
// いずれも `= ?` の等価比較で、`federation/*` は**未認証で叩ける**。
func TestListFiltersRejectUnstorableHost(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	const unstorable = "a\x00b"

	t.Run("federation/followers", func(t *testing.T) {
		rows, err := NewFollowingRepository(db).ListFollowersByHostCursor(unstorable, "", "", 10, model.FollowListViewer{})
		require.NoError(t, err, "host を SELECT に載せてしまっている")
		assert.Empty(t, rows)
	})
	t.Run("federation/following", func(t *testing.T) {
		rows, err := NewFollowingRepository(db).ListFollowingByHostCursor(unstorable, "", "", 10, model.FollowListViewer{})
		require.NoError(t, err, "host を SELECT に載せてしまっている")
		assert.Empty(t, rows)
	})
	t.Run("federation/users", func(t *testing.T) {
		rows, err := NewUserRepository(db).ListUsers(model.UserListFilter{Hostname: unstorable, Limit: 10})
		require.NoError(t, err, "hostname を SELECT に載せてしまっている")
		assert.Empty(t, rows)
	})
}

// **role id は overlap (`&&`) = OR なので要素ごとに落とす (#3025 のレビュー M1)。**
// 全体を空にすると、一緒に指定した他の role の一致まで消える。
func TestEmojiV2RoleIDsDropOnlyTheUnstorableElement(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	repo := NewEmojiRepository(db)
	e := &model.Emoji{
		ID:                                      "nulguardemoji1",
		Name:                                    "nulguardemoji1",
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"nulguardrole1"},
	}
	require.NoError(t, db.Create(e).Error)
	t.Cleanup(func() { db.Delete(&model.Emoji{}, `"id" = ?`, e.ID) })

	filter := func(ids []string) model.EmojiV2Filter {
		return model.EmojiV2Filter{Query: &model.EmojiV2Query{RoleIDs: ids}, Limit: 10}
	}
	got, err := repo.ListV2(filter([]string{"nulguardrole1"}))
	require.NoError(t, err)
	require.Len(t, got, 1, "前提が崩れている (role で引けていない)")

	got, err = repo.ListV2(filter([]string{"nulguardrole1", "a\x00b"}))
	require.NoError(t, err, "role id を ARRAY に載せてしまっている")
	assert.Len(t, got, 1, "OR なのに他の role の一致まで消している")

	got, err = repo.ListV2(filter([]string{"a\x00b"}))
	require.NoError(t, err)
	assert.Empty(t, got, "指定が全部落ちたのにフィルタごと素通ししている (絞ったつもりで全件出る)")

	// **同じ filter で 2 回引く経路まで見る (#3025 のレビュー H1)。** handler は
	// `ListV2` の直後に `CountV2` を同じ filter で呼ぶ。`filter.Query` は
	// ポインタなので、1 回目が `RoleIDs` を書き換えると 2 回目は guard も後段の
	// フィルタも素通りし、**絞ったつもりで全件を数える**。
	f := filter([]string{"a\x00b"})
	got, err = repo.ListV2(f)
	require.NoError(t, err)
	require.Empty(t, got)
	n, err := repo.CountV2(f)
	require.NoError(t, err)
	assert.Zero(t, n, "1 回目の呼び出しが filter を書き換えて、2 回目が絞れていない")
}

// **gate の射程外だが実際に届く経路 (#3025 のレビュー H1 / M1 / M2 / M4 / L1)。**
// 単一行 lookup でも LIKE でもないので gate は見ていない。ここが唯一の防波堤。
func TestReachableListPathsRejectUnstorableValues(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	const bad = "a\x00b"

	t.Run("notes/reactions の type (未認証)", func(t *testing.T) {
		rows, err := NewNoteReactionRepository(db).ListByNoteID("n1", "", "", 10, []string{bad})
		require.NoError(t, err, "reaction を SELECT に載せている")
		assert.Empty(t, rows)
	})
	t.Run("users/clips の userId (未認証)", func(t *testing.T) {
		rows, err := NewClipRepository(db).ListPublicByUser(bad, "", "", 10, 0)
		require.NoError(t, err, "userId を SELECT に載せている")
		assert.Empty(t, rows)
	})
	t.Run("users/flashs の userId (未認証)", func(t *testing.T) {
		rows, err := NewFlashRepository(db).ListPublicByUser(bad, "", "", 10, 0)
		require.NoError(t, err, "userId を SELECT に載せている")
		assert.Empty(t, rows)
	})
	t.Run("users/gallery/posts の userId (未認証)", func(t *testing.T) {
		rows, err := NewGalleryRepository(db).ListByUser(bad, "", "", 10, 0)
		require.NoError(t, err, "userId を SELECT に載せている")
		assert.Empty(t, rows)
	})
	t.Run("users/pages の userId (未認証)", func(t *testing.T) {
		rows, err := NewPageRepository(db).ListPublicByUser(bad, "", "", 10, 0)
		require.NoError(t, err, "userId を SELECT に載せている")
		assert.Empty(t, rows)
	})
	t.Run("drive/files/find-by-hash の md5", func(t *testing.T) {
		rows, err := NewDriveFileRepository(db).FindAllByMD5("u1", bad)
		require.NoError(t, err, "md5 を SELECT に載せている")
		assert.Empty(t, rows)
	})
	t.Run("i/registry の domain", func(t *testing.T) {
		repo := NewRegistryRepository(db)
		domain := bad
		items, err := repo.GetAll("u1", []string{"s"}, &domain)
		require.NoError(t, err, "domain を SELECT に載せている")
		assert.Empty(t, items)
		keys, err := repo.KeysWithType("u1", []string{"s"}, &domain)
		require.NoError(t, err, "domain を SELECT に載せている")
		assert.Empty(t, keys)
		require.NoError(t, repo.Remove("u1", "k", []string{"s"}, &domain), "domain を DELETE に載せている")
	})
	t.Run("admin/emoji の bulk", func(t *testing.T) {
		repo := NewEmojiRepository(db)
		require.NoError(t, repo.UpdateFieldsMany([]string{bad}, map[string]any{"category": "x"}),
			"id を UPDATE に載せている")
		require.NoError(t, repo.DeleteMany([]string{bad}), "id を DELETE に載せている")
	})
	t.Run("admin/federation の host", func(t *testing.T) {
		rows, err := NewFollowingRepository(db).ListFollowingByHost(bad, 10, 0)
		require.NoError(t, err, "host を SELECT に載せている")
		assert.Empty(t, rows)
	})
}

// **「無い」を `(nil, nil)` で表す lookup は、guard もそれに合わせる
// (#3025 のレビュー H2)。** ここだけ `ErrNotFound` を返すと呼び出し側が err として
// 扱い、`roles/assignment-show` が 500 に倒れる。
func TestNilContractLookupsKeepReturningNilNil(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	const bad = "a\x00b"

	got, err := NewRoleAssignmentRepository(db).FindActive(bad, bad, time.Now())
	require.NoError(t, err, "契約 (nil, nil) を破って error を返している")
	assert.Nil(t, got)

	game, err := NewReversiRepository(db).FindPendingInvitation(bad, bad)
	require.NoError(t, err, "契約 (nil, nil) を破って error を返している")
	assert.Nil(t, game)
}

// **guard を 1 つ残らず通す (#3025)。** gate は「guard が書いてあるか」を静的に
// 見るだけなので、**その guard が実際に効くか**はここでしか分からない。実 DB を
// 使うので、弾けていなければ SQLSTATE 08P01 が返って `IsNotFound` でも `nil` でも
// なくなる。件数が多いのは意図的で、1 つずつ書かないと**新しい lookup に guard を
// 足し忘れたときに、カバレッジが落ちるだけで誰も気付かない**。
func TestGuardedLookupsRejectUnstorableValues(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	bad := "a\x00b"

	for _, tt := range []struct {
		name string
		call func() error
	}{
		{"NewAbuseReportRepository.FindByID", func() error { _, err := NewAbuseReportRepository(db).FindByID(bad); return err }},
		{"NewAbuseReportRepository.FindStatesByIDs", func() error { _, err := NewAbuseReportRepository(db).FindStatesByIDs([]string{bad}); return err }},
		{"NewAbuseReportNotificationRecipientRepository.FindByID", func() error { _, err := NewAbuseReportNotificationRecipientRepository(db).FindByID(bad); return err }},
		{"NewAccessTokenRepository.FindByHash", func() error { _, err := NewAccessTokenRepository(db).FindByHash(bad); return err }},
		{"NewAccessTokenRepository.FindByHashOrToken", func() error { _, err := NewAccessTokenRepository(db).FindByHashOrToken(bad, bad); return err }},
		{"NewAccessTokenRepository.FindByID", func() error { _, err := NewAccessTokenRepository(db).FindByID(bad); return err }},
		{"NewAccessTokenRepository.DeleteByID", func() error { return NewAccessTokenRepository(db).DeleteByID(bad) }},
		{"NewAdRepository.FindByID", func() error { _, err := NewAdRepository(db).FindByID(bad); return err }},
		{"NewAnnouncementRepository.FindByID", func() error { _, err := NewAnnouncementRepository(db).FindByID(bad); return err }},
		{"NewAntennaRepository.FindByID", func() error { _, err := NewAntennaRepository(db).FindByID(bad); return err }},
		{"NewAuthSessionRepository.FindAppBySecret", func() error { _, err := NewAuthSessionRepository(db).FindAppBySecret(bad); return err }},
		{"NewAuthSessionRepository.FindSessionByToken", func() error { _, err := NewAuthSessionRepository(db).FindSessionByToken(bad); return err }},
		{"NewAuthSessionRepository.FindSessionByTokenAndAppID", func() error { _, err := NewAuthSessionRepository(db).FindSessionByTokenAndAppID(bad, bad); return err }},
		{"NewAuthSessionRepository.FindAccessTokenByAppAndUser", func() error { _, err := NewAuthSessionRepository(db).FindAccessTokenByAppAndUser(bad, bad); return err }},
		{"NewAuthSessionRepository.FindAccessTokenBySession", func() error { _, err := NewAuthSessionRepository(db).FindAccessTokenBySession(bad); return err }},
		{"NewAuthSessionRepository.FindAppByID", func() error { _, err := NewAuthSessionRepository(db).FindAppByID(bad); return err }},
		{"NewAvatarDecorationRepository.FindByID", func() error { _, err := NewAvatarDecorationRepository(db).FindByID(bad); return err }},
		{"NewBlockingRepository.FindByPair", func() error { _, err := NewBlockingRepository(db).FindByPair(bad, bad); return err }},
		{"NewChannelRepository.FindByID", func() error { _, err := NewChannelRepository(db).FindByID(bad); return err }},
		{"NewChannelRepository.FindByIDs", func() error { _, err := NewChannelRepository(db).FindByIDs([]string{bad}); return err }},
		{"NewChannelFollowingRepository.FindByPair", func() error { _, err := NewChannelFollowingRepository(db).FindByPair(bad, bad); return err }},
		{"NewChatRepository.FindRoomByID", func() error { _, err := NewChatRepository(db).FindRoomByID(bad); return err }},
		{"NewChatRepository.FindRoomByURI", func() error { _, err := NewChatRepository(db).FindRoomByURI(bad); return err }},
		{"NewChatRepository.FindMessageByID", func() error { _, err := NewChatRepository(db).FindMessageByID(bad); return err }},
		{"NewChatRepository.FindMessageByURI", func() error { _, err := NewChatRepository(db).FindMessageByURI(bad); return err }},
		{"NewChatRepository.FindMembership", func() error { _, err := NewChatRepository(db).FindMembership(bad, bad); return err }},
		{"NewChatRepository.FindInvitation", func() error { _, err := NewChatRepository(db).FindInvitation(bad, bad); return err }},
		{"NewChatRepository.FindInvitationByID", func() error { _, err := NewChatRepository(db).FindInvitationByID(bad); return err }},
		{"NewChunkedUploadSessionRepository.FindByID", func() error { _, err := NewChunkedUploadSessionRepository(db).FindByID(bad); return err }},
		{"NewClipRepository.FindByID", func() error { _, err := NewClipRepository(db).FindByID(bad); return err }},
		{"NewClipRepository.ListPublicByIDs", func() error { _, err := NewClipRepository(db).ListPublicByIDs([]string{bad}); return err }},
		{"NewClipNoteRepository.FindByPair", func() error { _, err := NewClipNoteRepository(db).FindByPair(bad, bad); return err }},
		{"NewDriveFileRepository.FindByID", func() error { _, err := NewDriveFileRepository(db).FindByID(bad); return err }},
		{"NewDriveFileRepository.FindByIDs", func() error { _, err := NewDriveFileRepository(db).FindByIDs([]string{bad}); return err }},
		{"NewDriveFileRepository.FindByMD5", func() error { _, err := NewDriveFileRepository(db).FindByMD5(bad, bad); return err }},
		{"NewDriveFileRepository.FindByAnyURL", func() error { _, err := NewDriveFileRepository(db).FindByAnyURL(bad); return err }},
		{"NewDriveFileRepository.FindByURI", func() error { _, err := NewDriveFileRepository(db).FindByURI(bad); return err }},
		{"NewDriveFileRepository.FindByAccessKey", func() error { _, err := NewDriveFileRepository(db).FindByAccessKey(bad); return err }},
		{"NewDriveFileRepository.FindByAnyAccessKey", func() error { _, err := NewDriveFileRepository(db).FindByAnyAccessKey(bad); return err }},
		{"NewDriveFileRepository.DeleteByIDs", func() error { _, err := NewDriveFileRepository(db).DeleteByIDs([]string{bad}); return err }},
		{"NewDriveFolderRepository.FindByID", func() error { _, err := NewDriveFolderRepository(db).FindByID(bad); return err }},
		{"NewDriveFolderRepository.FindByIDs", func() error { _, err := NewDriveFolderRepository(db).FindByIDs([]string{bad}); return err }},
		{"NewEmojiRepository.FindByID", func() error { _, err := NewEmojiRepository(db).FindByID(bad); return err }},
		{"NewEmojiRepository.FindManyByIDs", func() error { _, err := NewEmojiRepository(db).FindManyByIDs([]string{bad}); return err }},
		{"NewEmojiRepository.FindByNameAndHost", func() error { _, err := NewEmojiRepository(db).FindByNameAndHost(bad, &bad); return err }},
		{"NewEmojiApplicationRepository.FindByID", func() error { _, err := NewEmojiApplicationRepository(db).FindByID(bad); return err }},
		{"NewFlashRepository.FindByID", func() error { _, err := NewFlashRepository(db).FindByID(bad); return err }},
		{"NewFlashLikeRepository.FindByPair", func() error { _, err := NewFlashLikeRepository(db).FindByPair(bad, bad); return err }},
		{"NewFollowRequestRepository.FindByPair", func() error { _, err := NewFollowRequestRepository(db).FindByPair(bad, bad); return err }},
		{"NewFollowingRepository.FindByPair", func() error { _, err := NewFollowingRepository(db).FindByPair(bad, bad); return err }},
		{"NewGalleryRepository.FindPostsByIDs", func() error { _, err := NewGalleryRepository(db).FindPostsByIDs([]string{bad}); return err }},
		{"NewHashtagRepository.FindByName", func() error { _, err := NewHashtagRepository(db).FindByName(bad); return err }},
		{"NewInstanceRepository.FindByHost", func() error { _, err := NewInstanceRepository(db).FindByHost(bad); return err }},
		{"NewInstanceSignatureCapabilityRepository.FindByHost", func() error { _, err := NewInstanceSignatureCapabilityRepository(db).FindByHost(bad); return err }},
		{"NewMutingRepository.FindByPair", func() error { _, err := NewMutingRepository(db).FindByPair(bad, bad); return err }},
		{"NewRenoteMutingRepository.FindByPair", func() error { _, err := NewRenoteMutingRepository(db).FindByPair(bad, bad); return err }},
		{"NewNoteRepository.FindByID", func() error { _, err := NewNoteRepository(db).FindByID(bad); return err }},
		{"NewNoteRepository.FindByIDWithUser", func() error { _, err := NewNoteRepository(db).FindByIDWithUser(bad); return err }},
		{"NewNoteRepository.FindByIDWithRelations", func() error { _, err := NewNoteRepository(db).FindByIDWithRelations(bad); return err }},
		{"NewNoteRepository.FindByURI", func() error { _, err := NewNoteRepository(db).FindByURI(bad); return err }},
		{"NewNoteRepository.FindManyByIDsWithUser", func() error { _, err := NewNoteRepository(db).FindManyByIDsWithUser([]string{bad}); return err }},
		{"NewNoteRepository.ListRenotesByUser", func() error { _, err := NewNoteRepository(db).ListRenotesByUser(bad, bad); return err }},
		{"NewNoteDraftRepository.FindByIDAndUser", func() error { _, err := NewNoteDraftRepository(db).FindByIDAndUser(bad, bad); return err }},
		{"NewNoteDraftRepository.FindByID", func() error { _, err := NewNoteDraftRepository(db).FindByID(bad); return err }},
		{"NewNoteReactionRepository.FindByPair", func() error { _, err := NewNoteReactionRepository(db).FindByPair(bad, bad); return err }},
		{"NewPageRepository.FindByID", func() error { _, err := NewPageRepository(db).FindByID(bad); return err }},
		{"NewPageRepository.FindManyByIDs", func() error { _, err := NewPageRepository(db).FindManyByIDs([]string{bad}); return err }},
		{"NewPageRepository.FindByUserAndName", func() error { _, err := NewPageRepository(db).FindByUserAndName(bad, bad); return err }},
		{"NewPageLikeRepository.FindByPair", func() error { _, err := NewPageLikeRepository(db).FindByPair(bad, bad); return err }},
		{"NewPasswordResetRequestRepository.FindByToken", func() error { _, err := NewPasswordResetRequestRepository(db).FindByToken(bad); return err }},
		{"NewPollRepository.FindByNoteID", func() error { _, err := NewPollRepository(db).FindByNoteID(bad); return err }},
		{"NewPollVoteRepository.FindByUserAndChoice", func() error { _, err := NewPollVoteRepository(db).FindByUserAndChoice(bad, bad, 10); return err }},
		{"NewRegistrationTicketRepository.FindByCode", func() error { _, err := NewRegistrationTicketRepository(db).FindByCode(bad); return err }},
		{"NewRegistrationTicketRepository.FindByIDForUpdateTx", func() error { _, err := NewRegistrationTicketRepository(db).FindByIDForUpdateTx(db, bad); return err }},
		{"NewRegistrationTicketRepository.FindByID", func() error { _, err := NewRegistrationTicketRepository(db).FindByID(bad); return err }},
		{"NewRegistryRepository.Get", func() error { _, err := NewRegistryRepository(db).Get(bad, bad, []string{bad}, &bad); return err }},
		{"NewRelayRepository.FindByID", func() error { _, err := NewRelayRepository(db).FindByID(bad); return err }},
		{"NewRetentionAggregationRepository.FindByDateKey", func() error { _, err := NewRetentionAggregationRepository(db).FindByDateKey(bad); return err }},
		{"NewReversiRepository.FindByID", func() error { _, err := NewReversiRepository(db).FindByID(bad); return err }},
		{"NewReversiRepository.FindPendingInvitation", func() error { _, err := NewReversiRepository(db).FindPendingInvitation(bad, bad); return err }},
		{"NewRoleRepository.FindByID", func() error { _, err := NewRoleRepository(db).FindByID(bad); return err }},
		{"NewRoleAssignmentRepository.FindActive", func() error { _, err := NewRoleAssignmentRepository(db).FindActive(bad, bad, time.Now()); return err }},
		{"NewSignupApplicationRepository.FindByID", func() error { _, err := NewSignupApplicationRepository(db).FindByID(bad); return err }},
		{"NewSignupApplicationRepository.FindByClaimCodeHash", func() error { _, err := NewSignupApplicationRepository(db).FindByClaimCodeHash(bad); return err }},
		{"NewSignupApplicationRepository.FindByIDForUpdateTx", func() error { _, err := NewSignupApplicationRepository(db).FindByIDForUpdateTx(db, bad); return err }},
		{"NewSwSubscriptionRepository.FindByUserAndEndpoint", func() error { _, err := NewSwSubscriptionRepository(db).FindByUserAndEndpoint(bad, bad); return err }},
		{"NewSwSubscriptionRepository.FindByUserEndpointAuthKey", func() error {
			_, err := NewSwSubscriptionRepository(db).FindByUserEndpointAuthKey(bad, bad, bad, bad)
			return err
		}},
		{"NewSystemAccountRepository.FindByType", func() error { _, err := NewSystemAccountRepository(db).FindByType(bad); return err }},
		{"NewSystemWebhookRepository.FindByID", func() error { _, err := NewSystemWebhookRepository(db).FindByID(bad); return err }},
		{"NewUserRepository.FindByID", func() error { _, err := NewUserRepository(db).FindByID(bad); return err }},
		{"NewUserRepository.FindByURI", func() error { _, err := NewUserRepository(db).FindByURI(bad); return err }},
		{"NewUserRepository.FindByToken", func() error { _, err := NewUserRepository(db).FindByToken(bad); return err }},
		{"NewUserRepository.FindByUsernameLower", func() error { _, err := NewUserRepository(db).FindByUsernameLower(bad, &bad); return err }},
		{"NewUserRepository.FindProfileByUserID", func() error { _, err := NewUserRepository(db).FindProfileByUserID(bad); return err }},
		{"NewUserRepository.FindManyByIDs", func() error { _, err := NewUserRepository(db).FindManyByIDs([]string{bad}); return err }},
		{"NewUserRepository.FindProfileByVerifyCode", func() error { _, err := NewUserRepository(db).FindProfileByVerifyCode(bad); return err }},
		{"NewUserRepository.FindProfileByEmail", func() error { _, err := NewUserRepository(db).FindProfileByEmail(bad); return err }},
		{"NewUserKeypairRepository.FindByUserID", func() error { _, err := NewUserKeypairRepository(db).FindByUserID(bad); return err }},
		{"NewUserKeypairExtraRepository.FindByUserID", func() error { _, err := NewUserKeypairExtraRepository(db).FindByUserID(bad); return err }},
		{"NewUserListRepository.FindByID", func() error { _, err := NewUserListRepository(db).FindByID(bad); return err }},
		{"NewUserMemoRepository.FindByPair", func() error { _, err := NewUserMemoRepository(db).FindByPair(bad, bad); return err }},
		{"NewUserNotePiningRepository.FindByPair", func() error { _, err := NewUserNotePiningRepository(db).FindByPair(bad, bad); return err }},
		{"NewUserPendingRepository.FindByCode", func() error { _, err := NewUserPendingRepository(db).FindByCode(bad); return err }},
		{"NewUserPublickeyRepository.FindByUserID", func() error { _, err := NewUserPublickeyRepository(db).FindByUserID(bad); return err }},
		{"NewUserPublickeyRepository.FindByKeyID", func() error { _, err := NewUserPublickeyRepository(db).FindByKeyID(bad); return err }},
		{"NewUserPublickeyExtraRepository.FindByUserAndKeyID", func() error { _, err := NewUserPublickeyExtraRepository(db).FindByUserAndKeyID(bad, bad); return err }},
		{"NewUserSecurityKeyRepository.FindByID", func() error { _, err := NewUserSecurityKeyRepository(db).FindByID(bad); return err }},
		{"NewWebhookRepository.FindByID", func() error { _, err := NewWebhookRepository(db).FindByID(bad); return err }},
		{"NewWebhookRepository.FindByIDAndUserID", func() error { _, err := NewWebhookRepository(db).FindByIDAndUserID(bad, bad); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			// 「無い」の表し方は lookup ごとに違う (`ErrNotFound` / `(nil, nil)`)。
			// **どちらでもよいが、DB のエラーが返ってきたら弾けていない。**
			if err != nil {
				assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
			}
		})
	}
}

// **不正な UTF-8 も NUL と同じく列に入らない** (SQLSTATE 22021
// `invalid byte sequence for encoding "UTF8"`)。以前は `colfit.Storable` が NUL
// しか見ていなかったので、この 3 形はどれも guard を素通りして 500 になっていた。
//
// **クエリパラメータ経由でしか来ない形**なので、JSON body だけを試すテストでは
// 再現しない (Go の json decoder が U+FFFD へ矯正する)。ここは repository に
// 直接渡して、引く前に弾いていることを実 DB で固定する。
func TestLookupsRejectInvalidUTF8BeforeQuerying(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	for _, tt := range []struct {
		name string
		in   string
	}{
		{"孤立した継続バイト", "a\x80b"},
		{"UTF-8 で符号化した孤立サロゲート", "a\xed\xa0\x80b"},
		{"0xFF", "\xff"},
		{"切れた multibyte", "\xe3\x81"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewUserRepository(db).FindByID(tt.in)
			require.Error(t, err)
			assert.True(t, IsNotFound(err), "SELECT に載せてしまっている: %v", err)
		})
	}
}

// **入る値は引けたまま**であることまで見る。「不正な UTF-8 かどうか」ではなく
// 「非 ASCII かどうか」で弾く実装にすると、ここで落ちる。
func TestLookupsStillMatchValidNonASCII(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	repo := NewUserRepository(db)
	u := &model.User{ID: "utf8ok1", Username: "utf8ok1", UsernameLower: "utf8ok1", Name: strPtr2("絵文字\U0001F600")}
	require.NoError(t, db.Create(u).Error)
	t.Cleanup(func() { db.Delete(&model.User{}, `"id" = ?`, u.ID) })

	got, err := repo.FindByID(u.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Name)
	assert.Equal(t, "絵文字\U0001F600", *got.Name)
}
