package maintenance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
)

func seedAvatarFile(t *testing.T, id, url string, webpublic *string) *model.DriveFile {
	t.Helper()
	f := &model.DriveFile{
		ID:           id,
		Type:         "image/jpeg",
		Name:         id + ".jpg",
		MD5:          id,
		Size:         1,
		URL:          url,
		WebpublicURL: webpublic,
		Comment:      nil,
		Properties:   datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, testDB.Create(f).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, id) })
	return f
}

func seedAvatarUser(t *testing.T, id string, avatarID, avatarURL, bannerID, bannerURL *string) {
	t.Helper()
	token := "tok_" + id
	require.NoError(t, testDB.Create(&model.User{
		ID: id, Username: id, UsernameLower: id, Token: &token,
		AvatarID: avatarID, AvatarURL: avatarURL,
		BannerID: bannerID, BannerURL: bannerURL,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
}

func userMediaURLs(t *testing.T, id string) (avatar, banner *string) {
	t.Helper()
	var u model.User
	require.NoError(t, testDB.Model(&model.User{}).Where(`"id" = ?`, id).First(&u).Error)
	return u.AvatarURL, u.BannerURL
}

func sptr(s string) *string { return &s }

// 既存行の avatarUrl / bannerUrl が原本を指していたら公開用へ寄せ直す。
// 原本は EXIF / XMP が載ったままなので、設定したときのまま公開され続ける。
func TestBackfillAvatarPublicURLBatch(t *testing.T) {
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "drive_file"`)

	webpublic := "https://cdn.example/wp-a.webp"
	seedAvatarFile(t, "bfa1", "https://cdn.example/orig-a.jpg", &webpublic)
	// webpublic を持たないファイル (メタデータが無く再エンコードされなかった)。
	seedAvatarFile(t, "bfa2", "https://cdn.example/orig-b.png", nil)

	// 1. 原本を指している行 → webpublic へ
	seedAvatarUser(t, "bfu1", sptr("bfa1"), sptr("https://cdn.example/orig-a.jpg"), nil, nil)
	// 2. banner も同じ扱い
	seedAvatarUser(t, "bfu2", nil, nil, sptr("bfa1"), sptr("https://cdn.example/orig-a.jpg"))
	// 3. webpublic を持たないファイル → 原本のままで変更なし
	seedAvatarUser(t, "bfu3", sptr("bfa2"), sptr("https://cdn.example/orig-b.png"), nil, nil)
	// 4. 既に公開用を指している行 → 変更なし (冪等)
	seedAvatarUser(t, "bfu4", sptr("bfa1"), &webpublic, nil, nil)
	// 5. avatarId も bannerId も無い行 → そもそも対象外 (リモート利用者の形)
	seedAvatarUser(t, "bfu5", nil, sptr("https://remote.example/avatar.png"), nil, nil)

	// --- dry-run は書かない ---
	res, err := BackfillAvatarPublicURLBatch(testDB, "", 100, true)
	require.NoError(t, err)
	assert.Equal(t, 4, res.Scanned, "avatarId か bannerId を持つ 4 行が対象")
	assert.Equal(t, 2, res.Updated)
	assert.Equal(t, "bfu4", res.LastID)

	got, _ := userMediaURLs(t, "bfu1")
	require.NotNil(t, got)
	assert.Equal(t, "https://cdn.example/orig-a.jpg", *got, "dry-run では書き換えない")

	// --- 本実行 ---
	res, err = BackfillAvatarPublicURLBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 4, res.Scanned)
	assert.Equal(t, 2, res.Updated)

	avatar, _ := userMediaURLs(t, "bfu1")
	require.NotNil(t, avatar)
	assert.Equal(t, webpublic, *avatar, "原本から公開用へ移っている")

	_, banner := userMediaURLs(t, "bfu2")
	require.NotNil(t, banner)
	assert.Equal(t, webpublic, *banner, "banner も同じ経路")

	avatar, _ = userMediaURLs(t, "bfu3")
	require.NotNil(t, avatar)
	assert.Equal(t, "https://cdn.example/orig-b.png", *avatar, "webpublic が無ければ原本のまま")

	avatar, _ = userMediaURLs(t, "bfu5")
	require.NotNil(t, avatar)
	assert.Equal(t, "https://remote.example/avatar.png", *avatar, "リモートの形は対象外")

	// **ファイルを消すと avatarId ごと NULL になる** (`FK_user_avatarId` は
	// `ON DELETE SET NULL`)。だから「参照先が消えた行」は対象集合に現れない。
	require.NoError(t, testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, "bfa2").Error)
	var orphanAvatarID *string
	require.NoError(t, testDB.Raw(`SELECT "avatarId" FROM "user" WHERE id = ?`, "bfu3").
		Scan(&orphanAvatarID).Error)
	assert.Nil(t, orphanAvatarID, "FK が avatarId を NULL にする")

	// --- 冪等 ---
	res, err = BackfillAvatarPublicURLBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Scanned, "bfu3 は FK で avatarId が NULL になり対象から外れた")
	assert.Equal(t, 0, res.Updated, "2 回目は書き換えるものが無い")
}

// keyset で再開できること。LastID を次の fromID に渡すと続きから進む。
func TestBackfillAvatarPublicURLBatch_Keyset(t *testing.T) {
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "drive_file"`)

	webpublic := "https://cdn.example/wp-k.webp"
	seedAvatarFile(t, "bfk1", "https://cdn.example/orig-k.jpg", &webpublic)
	for _, id := range []string{"bfka", "bfkb", "bfkc"} {
		seedAvatarUser(t, id, sptr("bfk1"), sptr("https://cdn.example/orig-k.jpg"), nil, nil)
	}

	res, err := BackfillAvatarPublicURLBatch(testDB, "", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Scanned)
	assert.Equal(t, 2, res.Updated)
	assert.Equal(t, "bfkb", res.LastID)

	res, err = BackfillAvatarPublicURLBatch(testDB, res.LastID, 2, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, 1, res.Updated)
	assert.Equal(t, "bfkc", res.LastID)

	res, err = BackfillAvatarPublicURLBatch(testDB, res.LastID, 2, false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Scanned)
	assert.Equal(t, "", res.LastID, "尽きたら LastID は空")

	for _, id := range []string{"bfka", "bfkb", "bfkc"} {
		avatar, _ := userMediaURLs(t, id)
		require.NotNil(t, avatar)
		assert.Equal(t, webpublic, *avatar, id)
	}
}

// **リモート利用者は対象外。** mk-go はリモートのアイコンを drive に保存しないが、
// upstream は保存して `avatarId` を書く。TS から引き継いだ DB にはその id が残って
// おり、mk-go の refreshActor は `avatarUrl` しか更新しないので id は古いまま残る。
// host で絞らないと、取り直した現在のリモート URL を TS 時代のキャッシュへ巻き戻す。
func TestBackfillAvatarPublicURLBatch_SkipsRemoteUsers(t *testing.T) {
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "drive_file"`)

	cached := "https://mk.example/files/ts-cached-webpublic"
	seedAvatarFile(t, "bfr1", "https://mk.example/files/ts-cached-original", &cached)

	// TS 由来の形: リモート利用者が avatarId を持ち、avatarUrl は mk-go が
	// actor から取り直した現在のリモート URL を指している。
	remoteHost := "remote.example"
	current := "https://remote.example/avatar-now.png"
	token := "tok_bfr"
	require.NoError(t, testDB.Create(&model.User{
		ID: "bfru1", Username: "bob", UsernameLower: "bob", Host: &remoteHost, Token: &token,
		AvatarID: sptr("bfr1"), AvatarURL: &current,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, "bfru1") })

	// 比較用にローカル利用者も 1 件置く。
	seedAvatarUser(t, "bfrl1", sptr("bfr1"), sptr("https://mk.example/files/ts-cached-original"), nil, nil)

	res, err := BackfillAvatarPublicURLBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned, "ローカルの 1 件だけが対象")
	assert.Equal(t, 1, res.Updated)

	avatar, _ := userMediaURLs(t, "bfru1")
	require.NotNil(t, avatar)
	assert.Equal(t, current, *avatar, "リモート利用者の avatarUrl を巻き戻してはいけない")

	avatar, _ = userMediaURLs(t, "bfrl1")
	require.NotNil(t, avatar)
	assert.Equal(t, cached, *avatar, "ローカルは従来どおり寄せ直す")
}

// 読んでから撃つまでの間にアイコンが変わっていたら書かない。書いてしまうと
// `avatarId` と `avatarUrl` が食い違ったまま次の更新まで残る。
func TestUpdateUserMediaURLs_GuardsAgainstConcurrentChange(t *testing.T) {
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "drive_file"`)

	seedAvatarFile(t, "bfg1", "https://cdn.example/g1.jpg", nil)
	seedAvatarUser(t, "bfgu1", sptr("bfg1"), sptr("https://cdn.example/g1.jpg"), nil, nil)

	// 旧値が一致する -> 書ける
	applied, err := updateUserMediaURLs(testDB, "bfgu1",
		map[string]any{"avatarId": "bfg1"},
		map[string]any{"avatarUrl": "https://cdn.example/new.webp"})
	require.NoError(t, err)
	assert.True(t, applied)
	avatar, _ := userMediaURLs(t, "bfgu1")
	require.NotNil(t, avatar)
	assert.Equal(t, "https://cdn.example/new.webp", *avatar)

	// 旧値が一致しない (= 間に変わった) -> 書かない
	applied, err = updateUserMediaURLs(testDB, "bfgu1",
		map[string]any{"avatarId": "stale"},
		map[string]any{"avatarUrl": "https://cdn.example/should-not-win.webp"})
	require.NoError(t, err)
	assert.False(t, applied, "旧値が変わっていたら書かない")
	avatar, _ = userMediaURLs(t, "bfgu1")
	require.NotNil(t, avatar)
	assert.Equal(t, "https://cdn.example/new.webp", *avatar, "上書きされていない")
}

// **アニメーションになりうる形式で公開用を持つ行は触らない。** 公開用は 1 コマの
// 静止画かもしれず、寄せ直すといま動いているアニメーションのアイコンをその瞬間に
// 静止画へ固定する。どちらかはバイト列を読まないと分からないので触らない側に倒す。
func TestBackfillAvatarPublicURLBatch_SkipsAnimatableTypes(t *testing.T) {
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "drive_file"`)

	wp := "https://cdn.example/flattened.webp"
	orig := "https://cdn.example/original"

	// 触らない形式。**`image/vnd.mozilla.apng` も入れる** — mk-go の MIME 検出は
	// APNG をこの綴りで報告する (`drive_service.go`) ので、旧い行はこちらを持つ。
	for i, mime := range []string{"image/gif", "image/apng", "image/vnd.mozilla.apng", "image/webp", "image/avif"} {
		id := "bfanim" + string(rune('a'+i))
		f := &model.DriveFile{
			ID: id, Type: mime, Name: id, MD5: id, Size: 1,
			URL: orig, WebpublicURL: &wp, Properties: datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, testDB.Create(f).Error)
		t.Cleanup(func() { testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, id) })
		seedAvatarUser(t, "bfu"+id, sptr(id), sptr(orig), nil, nil)
	}
	// 比較用: アニメーションになりえない形式は従来どおり寄せ直す
	// `seedAvatarFile` は `image/jpeg` を入れる (= アニメーションになりえない形式)。
	seedAvatarFile(t, "bfstill", orig, &wp)
	seedAvatarUser(t, "bfustill", sptr("bfstill"), sptr(orig), nil, nil)

	res, err := BackfillAvatarPublicURLBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 6, res.Scanned)
	assert.Equal(t, 1, res.Updated, "静止画形式の 1 件だけが対象")

	for i := range []int{0, 1, 2, 3, 4} {
		id := "bfubfanim" + string(rune('a'+i))
		avatar, _ := userMediaURLs(t, id)
		require.NotNil(t, avatar)
		assert.Equal(t, orig, *avatar, id+" は触らない")
	}
	avatar, _ := userMediaURLs(t, "bfustill")
	require.NotNil(t, avatar)
	assert.Equal(t, wp, *avatar, "静止画形式は寄せ直す")
}

// **バッチが組み立てる guard を見る。** `updateUserMediaURLs` を直接呼ぶテストだけ
// だと、呼び出し側が guard を渡さなくなっても緑のままになる。読み終えた直後に
// `avatarId` を差し替えて、バッチ自身の UPDATE が空振りすることを確かめる。
func TestBackfillAvatarPublicURLBatch_GuardIsWiredFromTheBatch(t *testing.T) {
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "drive_file"`)

	wp := "https://cdn.example/wp-wire.webp"
	seedAvatarFile(t, "bfw1", "https://cdn.example/orig-wire.jpg", &wp)
	seedAvatarUser(t, "bfwu1", sptr("bfw1"), sptr("https://cdn.example/orig-wire.jpg"), nil, nil)

	// user を読み終えた直後に avatarId を差し替える = 読んでから撃つまでの間に
	// 利用者がアイコンを変えた状態を作る。
	// **callback レジストリは Session を跨いで共有される** (`gorm.Config` の
	// フィールドなので `Session()` の値コピーでもポインタが同じ)。外さないと
	// この後のテストの `user` への query すべてで発火する。
	sess := testDB.Session(&gorm.Session{NewDB: true})
	t.Cleanup(func() {
		_ = sess.Callback().Query().Remove("test_swap_avatar")
	})
	require.NoError(t, sess.Callback().Query().After("gorm:query").
		Register("test_swap_avatar", func(tx *gorm.DB) {
			if tx.Statement == nil || tx.Statement.Table != "user" {
				return
			}
			tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
				Exec(`UPDATE "user" SET "avatarId" = NULL WHERE id = ?`, "bfwu1")
		}))

	res, err := BackfillAvatarPublicURLBatch(sess, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	// `Updated` は**「書けた件数」ではなく「書く必要があった件数」**。CAS が
	// 空振りしても減らさない (dry-run は衝突を知りようがないので、減らすと
	// dry-run と本実行で意味の違う数が並ぶ)。
	assert.Equal(t, 1, res.Updated, "衝突しても Updated は減らさない")

	avatar, _ := userMediaURLs(t, "bfwu1")
	require.NotNil(t, avatar)
	assert.Equal(t, "https://cdn.example/orig-wire.jpg", *avatar,
		"読んだときの avatarId を条件にしていれば、差し替わった行は書けない")
}

func TestClampBatchSize(t *testing.T) {
	// `IN ?` のプレースホルダは 1 バッチで最大 batchSize*2。PostgreSQL の
	// 上限 65535 を越えないところで頭を打つ。
	assert.Equal(t, 1000, clampBatchSize(0), "0 は既定へ")
	assert.Equal(t, 1000, clampBatchSize(-1), "負値も既定へ")
	assert.Equal(t, 1, clampBatchSize(1))
	assert.Equal(t, 10000, clampBatchSize(10000), "上限ちょうどは通す")
	assert.Equal(t, 10000, clampBatchSize(33000), "上限を越えたら頭打ち")
	assert.LessOrEqual(t, clampBatchSize(1<<30)*2, 65535, "プレースホルダ上限に収まる")
}

// URL が空の行は触らない。空を書くとアイコンが identicon に化けるので、
// 「寄せ直す先が無い」ときは現状維持にする。
func TestBackfillAvatarPublicURLBatch_SkipsEmptyURL(t *testing.T) {
	testDB.Exec(`DELETE FROM "user"`)
	testDB.Exec(`DELETE FROM "drive_file"`)

	f := &model.DriveFile{
		ID: "bfempty", Type: "image/jpeg", Name: "bfempty", MD5: "bfempty", Size: 1,
		URL: "", Properties: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, testDB.Create(f).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, "bfempty") })
	seedAvatarUser(t, "bfuempty", sptr("bfempty"), sptr("https://cdn.example/keep.jpg"), nil, nil)

	res, err := BackfillAvatarPublicURLBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, 0, res.Updated, "寄せ直す先が無いので触らない")

	avatar, _ := userMediaURLs(t, "bfuempty")
	require.NotNil(t, avatar)
	assert.Equal(t, "https://cdn.example/keep.jpg", *avatar)
}
