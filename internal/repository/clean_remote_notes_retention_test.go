package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestNoteRepository_DeleteExpiredRemoteNotes_Retention covers the retention
// guards ported from upstream CleanRemoteNotesProcessorService (#2329).
//
// 期限切れのリモートノートでも、ローカルユーザーが手を加えたものは消さない。
// これが無いと、クリップに入れた・ピン留めした・お気に入りにした投稿が本人の
// 操作と無関係に消える。
func TestNoteRepository_DeleteExpiredRemoteNotes_Retention(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "retention.example"

	remoteUser := &model.User{
		ID: "rtn_remote", Username: "rtn_remote", UsernameLower: "rtn_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)

	localUser := &model.User{
		ID: "rtn_local", Username: "rtn_local", UsernameLower: "rtn_local",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(localUser).Error)
	defer cleanupUser(t, localUser.ID)

	// aidx の先頭 8 文字が時刻。"00000000" は 2000-01-01 なので必ず期限切れ。
	newOldNote := func(t *testing.T, suffix string) *model.Note {
		t.Helper()
		n := &model.Note{
			ID: "00000000" + suffix, UserID: remoteUser.ID, UserHost: &host,
			Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, nr.Create(n))
		t.Cleanup(func() { cleanupNote(t, n.ID) })
		return n
	}

	cases := []struct {
		name string
		// protect attaches the row that must keep the note alive. nil = 対照群。
		protect  func(t *testing.T, noteID string)
		survives bool
	}{
		{
			name:     "何も付いていない期限切れノートは削除される",
			protect:  nil,
			survives: false,
		},
		{
			// upstream は clippedCount で判定する。mk-go も #1768 から維持しているが、
			// それより前にクリップした行は 0 のままなので、clip_note を直接見ないと
			// クリップを保護できない。
			name: "クリップに入っているノートは残る",
			protect: func(t *testing.T, noteID string) {
				clip := &model.Clip{ID: "rtn_clip_" + noteID[8:], UserID: localUser.ID, Name: "c"}
				require.NoError(t, testDB.Create(clip).Error)
				t.Cleanup(func() { testDB.Delete(&model.Clip{}, "id = ?", clip.ID) })
				cn := &model.ClipNote{ID: "rtn_cn_" + noteID[8:], NoteID: noteID, ClipID: clip.ID}
				require.NoError(t, testDB.Create(cn).Error)
				t.Cleanup(func() { testDB.Delete(&model.ClipNote{}, "id = ?", cn.ID) })
			},
			survives: true,
		},
		{
			name: "プロフィールにピン留めされたノートは残る",
			protect: func(t *testing.T, noteID string) {
				p := &model.UserNotePining{ID: "rtn_pin_" + noteID[8:], UserID: localUser.ID, NoteID: noteID}
				require.NoError(t, testDB.Create(p).Error)
				t.Cleanup(func() { testDB.Delete(&model.UserNotePining{}, "id = ?", p.ID) })
			},
			survives: true,
		},
		{
			name: "お気に入りに入っているノートは残る",
			protect: func(t *testing.T, noteID string) {
				f := &model.NoteFavorite{ID: "rtn_fav_" + noteID[8:], UserID: localUser.ID, NoteID: noteID}
				require.NoError(t, testDB.Create(f).Error)
				t.Cleanup(func() { testDB.Delete(&model.NoteFavorite{}, "id = ?", f.ID) })
			},
			survives: true,
		},
		{
			name: "ローカルユーザーがリアクションしたノートは残る",
			protect: func(t *testing.T, noteID string) {
				r := &model.NoteReaction{ID: "rtn_rx_" + noteID[8:], UserID: localUser.ID, NoteID: noteID, Reaction: "👍"}
				require.NoError(t, testDB.Create(r).Error)
				t.Cleanup(func() { testDB.Delete(&model.NoteReaction{}, "id = ?", r.ID) })
			},
			survives: true,
		},
		{
			// ページの作成で pageCount が増える (#3293)。ページの content にしか
			// 参照が無いので、カウンタを維持していないと保護されない。
			name: "ローカルのページに埋め込まれたノートは残る",
			protect: func(t *testing.T, noteID string) {
				pg := newTestPage("rtn_pg_"+noteID[8:], localUser.ID, "rtn"+noteID[9:])
				pg.Content = datatypes.JSON([]byte(`[{"type":"note","note":"` + noteID + `"}]`))
				require.NoError(t, NewPageRepository(testDB).Create(pg))
				t.Cleanup(func() { cleanupPage(t, pg.ID) })
			},
			survives: true,
		},
		{
			// リモートユーザーのリアクションは保護しない (upstream の
			// `"user"."host" IS NULL` 条件)。連合先の反応で無限に溜まるため。
			name: "リモートユーザーのリアクションだけなら削除される",
			protect: func(t *testing.T, noteID string) {
				r := &model.NoteReaction{ID: "rtn_rrx_" + noteID[8:], UserID: remoteUser.ID, NoteID: noteID, Reaction: "👍"}
				require.NoError(t, testDB.Create(r).Error)
				t.Cleanup(func() { testDB.Delete(&model.NoteReaction{}, "id = ?", r.ID) })
			},
			survives: false,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := newOldNote(t, "_rtn_"+string(rune('a'+i)))
			if tc.protect != nil {
				tc.protect(t, note.ID)
			}

			_, err := nr.DeleteExpiredRemoteNotes(1, 100)
			require.NoError(t, err)

			_, ferr := nr.FindByID(note.ID)
			if tc.survives {
				assert.NoError(t, ferr, "保護されたノートが削除された")
			} else {
				assert.Error(t, ferr, "保護対象でないノートが残っている")
			}
		})
	}
}

// TestNoteRepository_DeleteExpiredRemoteNotes_CountersBlockDeletion covers the
// upstream `clippedCount = 0` / `pageCount = 0` conditions. mk-go は
// clippedCount を #1768 から、pageCount を #3293 から維持している。
func TestNoteRepository_DeleteExpiredRemoteNotes_CountersBlockDeletion(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "counters.example"

	u := &model.User{
		ID: "ctr_remote", Username: "ctr_remote", UsernameLower: "ctr_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(u).Error)
	defer cleanupUser(t, u.ID)

	for _, tc := range []struct {
		name   string
		column string
	}{
		{"clippedCount が立っているノートは残る", "clippedCount"},
		{"pageCount が立っているノートは残る", "pageCount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &model.Note{
				ID: "00000000_ctr_" + tc.column[:3], UserID: u.ID, UserHost: &host,
				Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
			}
			require.NoError(t, nr.Create(n))
			defer cleanupNote(t, n.ID)
			require.NoError(t, testDB.Exec(`UPDATE "note" SET "`+tc.column+`" = 1 WHERE id = ?`, n.ID).Error)

			_, err := nr.DeleteExpiredRemoteNotes(1, 100)
			require.NoError(t, err)

			_, ferr := nr.FindByID(n.ID)
			assert.NoError(t, ferr, "カウンタが立っているノートが削除された")
		})
	}
}

// **ツリー単位で守ること。**
//
// 単体の条件だけで消すと、ローカル利用者が返信・引用・リノートしたリモート
// ノートが、その利用者の投稿を残したまま消える。返信先の無い返信や「削除された
// ノート」のリノートがタイムラインに残り、不可逆 (相手サーバーから取り直す
// 経路は無い)。upstream は起点をルートに絞り、再帰 CTE でツリー全体を作り、
// 1 件でも削除不可があればツリーごと除外する。
func TestNoteRepository_DeleteExpiredRemoteNotes_KeepsTreeWithProtectedDescendant(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "tree.example"

	remoteUser := &model.User{
		ID: "tree_remote", Username: "tree_remote", UsernameLower: "tree_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)

	localUser := &model.User{
		ID: "tree_local", Username: "tree_local", UsernameLower: "tree_local",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(localUser).Error)
	defer cleanupUser(t, localUser.ID)

	// リモートのルートノート (期限切れ)。
	root := &model.Note{
		ID: "00000000treeroot", UserID: remoteUser.ID, UserHost: &host,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(root))
	defer cleanupNote(t, root.ID)

	// **ローカル利用者がぶら下げた返信。** これがある限りツリーごと守る。
	reply := &model.Note{
		ID: "00000000treerepl", UserID: localUser.ID, ReplyID: &root.ID,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(reply))
	defer cleanupNote(t, reply.ID)

	_, err := nr.DeleteExpiredRemoteNotes(1, 100)
	require.NoError(t, err)

	var count int64
	require.NoError(t, testDB.Model(&model.Note{}).Where("id = ?", root.ID).Count(&count).Error)
	require.EqualValues(t, 1, count,
		"ローカル利用者の返信がぶら下がっているルートは消さないこと")
}

// 誰もぶら下がっていないリモートのツリーは従来どおり消えること。
func TestNoteRepository_DeleteExpiredRemoteNotes_RemovesFullyRemoteTree(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "tree2.example"

	remoteUser := &model.User{
		ID: "tree2_remote", Username: "tree2_remote", UsernameLower: "tree2_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)

	root := &model.Note{
		ID: "00000000tre2root", UserID: remoteUser.ID, UserHost: &host,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(root))
	defer cleanupNote(t, root.ID)

	child := &model.Note{
		ID: "00000000tre2chld", UserID: remoteUser.ID, UserHost: &host, ReplyID: &root.ID,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(child))
	defer cleanupNote(t, child.ID)

	_, err := nr.DeleteExpiredRemoteNotes(1, 100)
	require.NoError(t, err)

	var count int64
	require.NoError(t, testDB.Model(&model.Note{}).
		Where("id IN ?", []string{root.ID, child.ID}).Count(&count).Error)
	require.EqualValues(t, 0, count, "全部リモートのツリーは従来どおり消えること")
}

// --- 敵対的レビューで見つかった 3 つの欠陥 (#04541671 の追補) ---

// **引用リプライの連鎖で `tree` が爆発しないこと。**
//
// `replyId` と `renoteId` が同じツリーの別ノートを指す形 (= 引用リプライ。
// Misskey の通常機能) だと、1 ノードが 2 行から派生する。`UNION ALL` では
// Fibonacci 的に増殖し、実測では長さ 26 の連鎖で `tree` が 317,810 行
// (0.90 秒)、31 で 2 分 38 秒を超えた。**連合相手が投稿を積むだけで誘発
// できる。**
func TestNoteRepository_DeleteExpiredRemoteNotes_QuoteReplyChainDoesNotExplode(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "chain.example"

	remoteUser := &model.User{
		ID: "chain_remote", Username: "chain_remote", UsernameLower: "chain_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)

	// n_i は n_{i-1} への返信かつ n_{i-2} の引用。
	const chainLen = 26
	ids := make([]string, 0, chainLen)
	for i := 0; i < chainLen; i++ {
		n := &model.Note{
			ID: fmt.Sprintf("00000000chain%03d", i), UserID: remoteUser.ID, UserHost: &host,
			Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
		}
		if i >= 1 {
			n.ReplyID = &ids[i-1]
		}
		if i >= 2 {
			n.RenoteID = &ids[i-2]
		}
		require.NoError(t, nr.Create(n))
		ids = append(ids, n.ID)
		defer cleanupNote(t, n.ID)
	}

	// **時間で見る。** 行数は実装の内側なので、外から観測できる「終わるか」で
	// 判定する。`UNION ALL` 版は同じ長さで 1.27 秒かかった (develop は 4.6ms)。
	start := time.Now()
	_, err := nr.DeleteExpiredRemoteNotes(1, 100)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"引用リプライの連鎖で再帰 CTE が増殖している (UNION ALL になっていないか)")
}

// **期限内 (新しい) のノートを消さないこと。**
//
// cutoff を `roots` にしか掛けないと、たった今届いた返信がぶら下がっている
// だけのツリーが丸ごと消える。upstream は `removalCriteria` に
// `note."id" < :newestLimit` を含め、帰納ステップの判定にも使う。
func TestNoteRepository_DeleteExpiredRemoteNotes_KeepsRecentDescendant(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "recent.example"

	remoteUser := &model.User{
		ID: "recent_remote", Username: "recent_remote", UsernameLower: "recent_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)

	root := &model.Note{
		ID: "00000000recentrt", UserID: remoteUser.ID, UserHost: &host,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(root))
	defer cleanupNote(t, root.ID)

	// **たった今**届いたリモートの返信 (aidx の先頭 8 文字が現在時刻)。
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	freshID := gen.Generate(time.Now())
	fresh := &model.Note{
		ID: freshID, UserID: remoteUser.ID, UserHost: &host, ReplyID: &root.ID,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(fresh))
	defer cleanupNote(t, fresh.ID)

	_, err = nr.DeleteExpiredRemoteNotes(1, 100)
	require.NoError(t, err)

	var count int64
	require.NoError(t, testDB.Model(&model.Note{}).Where("id = ?", fresh.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count, "期限内のノートを消さないこと")
	require.NoError(t, testDB.Model(&model.Note{}).Where("id = ?", root.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count, "期限内の子孫を持つツリーは丸ごと残すこと")
}

// **保護されたルートが LIMIT の枠を食わないこと。**
//
// `roots` が removability を見ずに LIMIT すると、削除不可のルートが枠を占め、
// `deleted < batchSize` で break する processor が前へ進めなくなる。
func TestNoteRepository_DeleteExpiredRemoteNotes_ProtectedRootDoesNotBlockBatch(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "hol.example"

	remoteUser := &model.User{
		ID: "hol_remote", Username: "hol_remote", UsernameLower: "hol_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)

	localUser := &model.User{
		ID: "hol_local", Username: "hol_local", UsernameLower: "hol_local",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(localUser).Error)
	defer cleanupUser(t, localUser.ID)

	// id 昇順で先に来る、ローカルのお気に入りが付いた (= 削除不可の) ルート。
	protected := &model.Note{
		ID: "00000000hol_aaaa", UserID: remoteUser.ID, UserHost: &host,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(protected))
	defer cleanupNote(t, protected.ID)
	require.NoError(t, testDB.Exec(
		`INSERT INTO "note_favorite" (id, "createdAt", "noteId", "userId") VALUES (?, NOW(), ?, ?)`,
		"hol_fav", protected.ID, localUser.ID).Error)
	defer func() { testDB.Exec(`DELETE FROM "note_favorite" WHERE id = ?`, "hol_fav") }()

	deletable := &model.Note{
		ID: "00000000hol_bbbb", UserID: remoteUser.ID, UserHost: &host,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(deletable))
	defer cleanupNote(t, deletable.ID)

	// batchSize=1。保護されたルートが枠を食うと 0 件になり、掃除が止まる。
	n, err := nr.DeleteExpiredRemoteNotes(1, 1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "削除不可のルートが LIMIT の枠を食っている")

	var count int64
	require.NoError(t, testDB.Model(&model.Note{}).Where("id = ?", protected.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count, "保護されたルートは残ること")
}

// **消せないツリーが先頭に並んでいても、カーソルで後ろへ進める (#17957)。**
// 根の選び方に順序も位置も無かった頃は、配下にローカルの返信を持つ根が
// 毎回同じ LIMIT の枠を食い、その後ろの消せる根に一度も届かなかった。
func TestNoteRepository_DeleteExpiredRemoteNotesAfter_Cursor(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "cursor.example"
	remoteUser := &model.User{
		ID: "cur_remote", Username: "cur_remote", UsernameLower: "cur_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)
	localUser := &model.User{
		ID: "cur_local", Username: "cur_local", UsernameLower: "cur_local",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(localUser).Error)
	defer cleanupUser(t, localUser.ID)

	newNote := func(id string, user *model.User, h *string, replyID *string) {
		t.Helper()
		n := &model.Note{
			ID: id, UserID: user.ID, UserHost: h, ReplyID: replyID,
			Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, nr.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
	}
	// 先頭 150 個: 期限切れのリモートの根に、ローカルの返信がぶら下がる (消せない)。
	for i := range 150 {
		root := fmt.Sprintf("00000000_cur_p%03d", i)
		newNote(root, remoteUser, &host, nil)
		newNote(fmt.Sprintf("00000000_cur_q%03d", i), localUser, nil, &root)
	}
	// その後ろに消せる根が 5 個。
	var deletable []string
	for i := range 5 {
		id := fmt.Sprintf("00000000_cur_z%03d", i)
		newNote(id, remoteUser, &host, nil)
		deletable = append(deletable, id)
	}
	remaining := func() int {
		var n int64
		require.NoError(t, testDB.Model(&model.Note{}).Where("id IN ?", deletable).Count(&n).Error)
		return int(n)
	}

	// カーソル無しで何度回しても先頭の 100 個しか見ないので届かない。
	for range 3 {
		_, err := nr.DeleteExpiredRemoteNotes(1, 100)
		require.NoError(t, err)
	}
	require.Equal(t, 5, remaining(), "前提: カーソル無しでは後ろの根に届かない")

	// カーソルで進めると末尾まで届く。
	cursor := ""
	for range 10 {
		_, scanned, last, err := nr.DeleteExpiredRemoteNotesAfter(1, 100, cursor)
		require.NoError(t, err)
		if scanned < 100 {
			break
		}
		require.Greater(t, last, cursor, "カーソルは前にしか進まない")
		cursor = last
	}
	assert.Zero(t, remaining(), "後ろの消せる根が消える")
	var kept int64
	require.NoError(t, testDB.Model(&model.Note{}).Where("id LIKE ?", "00000000_cur_p%").Count(&kept).Error)
	assert.EqualValues(t, 150, kept, "消せないツリーは残る")

	// 期限より新しいカーソル (期限を延ばした後など) は先頭からにする。
	_, scanned, _, err := nr.DeleteExpiredRemoteNotesAfter(1, 100, "zzzzzzzzzz")
	require.NoError(t, err)
	assert.Equal(t, 100, scanned, "cutoff 以上のカーソルは無視して先頭から")
}

// **根は id 順に見る。** 順序が無いと「見た根の最大 id」をカーソルにしたとき、
// 見ていない若い id を飛び越える。挿入順 (heap の順) と id の順を逆にして、
// 若い id の消せる根が最初のページで拾われることを見る。
func TestNoteRepository_DeleteExpiredRemoteNotesAfter_Ordered(t *testing.T) {
	nr := NewNoteRepository(testDB)
	host := "order.example"
	remoteUser := &model.User{
		ID: "ord_remote", Username: "ord_remote", UsernameLower: "ord_remote",
		Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteUser).Error)
	defer cleanupUser(t, remoteUser.ID)
	localUser := &model.User{
		ID: "ord_local", Username: "ord_local", UsernameLower: "ord_local",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(localUser).Error)
	defer cleanupUser(t, localUser.ID)

	newNote := func(id string, user *model.User, h *string, replyID *string) {
		t.Helper()
		n := &model.Note{
			ID: id, UserID: user.ID, UserHost: h, ReplyID: replyID,
			Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, nr.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
	}
	// 先に大きい id の消せないツリーを 150 個入れ、後から小さい id の消せる根を入れる。
	for i := range 150 {
		root := fmt.Sprintf("00000000_ord_p%03d", i)
		newNote(root, remoteUser, &host, nil)
		newNote(fmt.Sprintf("00000000_ord_q%03d", i), localUser, nil, &root)
	}
	newNote("00000000_ord_a000", remoteUser, &host, nil)

	_, _, _, err := nr.DeleteExpiredRemoteNotesAfter(1, 100, "")
	require.NoError(t, err)
	_, ferr := nr.FindByID("00000000_ord_a000")
	assert.Error(t, ferr, "最初のページで最も若い id の根を見ている")
}
