package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func insertFollowing(t *testing.T, id, followerID, followeeID string) *model.Following {
	t.Helper()
	f := &model.Following{
		ID:         id,
		FollowerID: followerID,
		FolloweeID: followeeID,
	}
	require.NoError(t, testDB.Create(f).Error)
	return f
}

func TestFollowingRepository_Create_FindByPair(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	follower := insertTestUser(t, "u_fl_1", "follower1")
	defer cleanupUser(t, follower.ID)
	followee := insertTestUser(t, "u_fl_2", "followee1")
	defer cleanupUser(t, followee.ID)

	f := &model.Following{
		ID:         "fl_1",
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	}
	require.NoError(t, repo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)

	found, err := repo.FindByPair(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.Equal(t, f.ID, found.ID)
}

// ListFolloweeIDs はページングせず全件返す。HTL の「返信先が followers 限定の
// 投稿」ガードで集合として使うため、limit で切れると判定が壊れる。
func TestFollowingRepository_ListFolloweeIDs(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	follower := insertTestUser(t, "u_lfi_1", "lfiFollower")
	defer cleanupUser(t, follower.ID)
	a := insertTestUser(t, "u_lfi_a", "lfiA")
	defer cleanupUser(t, a.ID)
	b := insertTestUser(t, "u_lfi_b", "lfiB")
	defer cleanupUser(t, b.ID)
	other := insertTestUser(t, "u_lfi_o", "lfiO")
	defer cleanupUser(t, other.ID)

	for i, pair := range [][2]string{{follower.ID, a.ID}, {follower.ID, b.ID}, {other.ID, a.ID}} {
		id := fmt.Sprintf("lfi_%d", i)
		require.NoError(t, repo.Create(&model.Following{ID: id, FollowerID: pair[0], FolloweeID: pair[1]}))
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, id)
	}

	ids, err := repo.ListFolloweeIDs(follower.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{a.ID, b.ID}, ids, "他人のフォローは含めない")

	// フォロー 0 件なら空。
	ids, err = repo.ListFolloweeIDs(a.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)

	// 空 ID は早期 return。
	ids, err = repo.ListFolloweeIDs("")
	require.NoError(t, err)
	assert.Nil(t, ids)
}

func TestFollowingRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	_, err := repo.FindByPair("ghost1", "ghost2")
	assert.Error(t, err)
}

// ListFollowersByHostCursor / ListFollowingByHostCursor は federation の
// followers/following を id cursor でページングする (#1732)。
func TestFollowingRepository_ListByHostCursor(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	// ページングだけを見るテストなので、持ち主の公開範囲による絞り込みは
	// moderator として外す (絞り込みは TestFollowingRepository_HostListVisibility)。
	modBypass := model.FollowListViewer{Moderator: true}
	// following は (followerId, followeeId) が unique なので各行を別 pair にする。
	la := insertTestUser(t, "u_hc_la", "hcla")
	lb := insertTestUser(t, "u_hc_lb", "hclb")
	lc := insertTestUser(t, "u_hc_lc", "hclc")
	ru := insertTestUser(t, "u_hc_ru", "hcru")
	defer cleanupUser(t, la.ID)
	defer cleanupUser(t, lb.ID)
	defer cleanupUser(t, lc.ID)
	defer cleanupUser(t, ru.ID)

	host := "hostcursor.example"
	// followeeHost=host の 3 行 (federation/followers 対象)。id は昇順 a<b<c、
	// pair は (la→ru), (lb→ru), (lc→ru) で distinct。
	followers := []struct{ id, follower string }{
		{"hc_fee_a", la.ID}, {"hc_fee_b", lb.ID}, {"hc_fee_c", lc.ID},
	}
	for _, r := range followers {
		row := &model.Following{ID: r.id, FollowerID: r.follower, FolloweeID: ru.ID, FolloweeHost: &host}
		require.NoError(t, repo.Create(row))
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, r.id)
	}

	// untilID=hc_fee_c → a, b のみ DESC で [b, a]。
	rows, err := repo.ListFollowersByHostCursor(host, "", "hc_fee_c", 10, modBypass)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "hc_fee_b", rows[0].ID)
	assert.Equal(t, "hc_fee_a", rows[1].ID)

	// sinceID=hc_fee_a → b, c のみ ASC で [b, c]。
	rows, err = repo.ListFollowersByHostCursor(host, "hc_fee_a", "", 10, modBypass)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "hc_fee_b", rows[0].ID)
	assert.Equal(t, "hc_fee_c", rows[1].ID)

	// followerHost 版 (federation/following) も動くこと (default id DESC)。
	// pair は (ru→la), (ru→lb) で distinct。
	host2 := "hostcursor2.example"
	following := []struct{ id, followee string }{
		{"hc_fer_a", la.ID}, {"hc_fer_b", lb.ID},
	}
	for _, r := range following {
		row := &model.Following{ID: r.id, FollowerID: ru.ID, FollowerHost: &host2, FolloweeID: r.followee}
		require.NoError(t, repo.Create(row))
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, r.id)
	}
	rows, err = repo.ListFollowingByHostCursor(host2, "", "", 10, modBypass)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "hc_fer_b", rows[0].ID, "default は id DESC")

	// following 版の sinceID / untilID 分岐 + limit<=0 のデフォルト適用も通す。
	rows, err = repo.ListFollowingByHostCursor(host2, "hc_fer_a", "", 0, modBypass)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "hc_fer_b", rows[0].ID, "sinceID 指定 + limit<=0 デフォルト")

	rows, err = repo.ListFollowingByHostCursor(host2, "", "hc_fer_b", 10, modBypass)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "hc_fer_a", rows[0].ID, "untilID 指定")

	// followers 版も limit<=0 デフォルト適用を通す (全 3 行返る)。
	rows, err = repo.ListFollowersByHostCursor(host, "", "", 0, modBypass)
	require.NoError(t, err)
	assert.Len(t, rows, 3)
}

// CountRemoteFollowees / CountRemoteFollowers は federation/stats の
// allSubCount / allPubCount に対応 (#1544)。グローバル count なので baseline
// からの delta を検証する。
func TestFollowingRepository_CountRemoteFollows(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	a := insertTestUser(t, "u_crf_a", "crfa")
	b := insertTestUser(t, "u_crf_b", "crfb")
	cc := insertTestUser(t, "u_crf_c", "crfc")
	d := insertTestUser(t, "u_crf_d", "crfd")
	defer cleanupUser(t, a.ID)
	defer cleanupUser(t, b.ID)
	defer cleanupUser(t, cc.ID)
	defer cleanupUser(t, d.ID)

	baseSub, err := repo.CountRemoteFollowees()
	require.NoError(t, err)
	basePub, err := repo.CountRemoteFollowers()
	require.NoError(t, err)

	remote := "remote.example"
	// a→b: followeeHost set (remote followee = allSubCount)
	subRow := &model.Following{ID: "crf_sub", FollowerID: a.ID, FolloweeID: b.ID, FolloweeHost: &remote}
	// c→d: followerHost set (remote follower = allPubCount)
	pubRow := &model.Following{ID: "crf_pub", FollowerID: cc.ID, FolloweeID: d.ID, FollowerHost: &remote}
	require.NoError(t, repo.Create(subRow))
	require.NoError(t, repo.Create(pubRow))
	defer testDB.Exec(`DELETE FROM "following" WHERE id IN (?, ?)`, subRow.ID, pubRow.ID)

	sub, err := repo.CountRemoteFollowees()
	require.NoError(t, err)
	assert.Equal(t, baseSub+1, sub, "followeeHost not null が 1 件増える")
	pub, err := repo.CountRemoteFollowers()
	require.NoError(t, err)
	assert.Equal(t, basePub+1, pub, "followerHost not null が 1 件増える")
}

func TestFollowingRepository_Exists(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	follower := insertTestUser(t, "u_fl_3", "follower3")
	defer cleanupUser(t, follower.ID)
	followee := insertTestUser(t, "u_fl_4", "followee4")
	defer cleanupUser(t, followee.ID)

	exists, err := repo.Exists(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	insertFollowing(t, "fl_2", follower.ID, followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_2")

	exists, err = repo.Exists(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.True(t, exists)
}

// TestFollowingRepository_FilterFollowingsFromAnchor + ToAnchor cover the
// batch lookup added for users/{followers,following} relation flag
// population (#1144). 空 input は短絡で nil 返却、通常 input は anchor が
// follow している (or anchor を follow している) candidate のみ返る。
// cancelled ctx の error path も別 test で cover。
func TestFollowingRepository_FilterFollowingsFromAnchor(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	anchor := insertTestUser(t, "u_ff_a", "anchorff")
	defer cleanupUser(t, anchor.ID)
	t1 := insertTestUser(t, "u_ff_t1", "targetff1")
	defer cleanupUser(t, t1.ID)
	t2 := insertTestUser(t, "u_ff_t2", "targetff2")
	defer cleanupUser(t, t2.ID)

	insertFollowing(t, "ff_1", anchor.ID, t1.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "ff_1")

	// 空入力は repo を叩かずに nil 返却。
	out, err := repo.FilterFollowingsFromAnchor("", []string{t1.ID})
	require.NoError(t, err)
	assert.Nil(t, out)
	out, err = repo.FilterFollowingsFromAnchor(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	// 通常入力: anchor は t1 のみ follow → t1 だけ返る (t2 は除外、ghost も miss)。
	out, err = repo.FilterFollowingsFromAnchor(anchor.ID, []string{t1.ID, t2.ID, "ghost"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{t1.ID}, out)
}

func TestFollowingRepository_FilterFollowingsToAnchor(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	anchor := insertTestUser(t, "u_ft_a", "anchorft")
	defer cleanupUser(t, anchor.ID)
	c1 := insertTestUser(t, "u_ft_c1", "candft1")
	defer cleanupUser(t, c1.ID)
	c2 := insertTestUser(t, "u_ft_c2", "candft2")
	defer cleanupUser(t, c2.ID)

	insertFollowing(t, "ft_1", c1.ID, anchor.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "ft_1")

	out, err := repo.FilterFollowingsToAnchor("", []string{c1.ID})
	require.NoError(t, err)
	assert.Nil(t, out)
	out, err = repo.FilterFollowingsToAnchor(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = repo.FilterFollowingsToAnchor(anchor.ID, []string{c1.ID, c2.ID, "ghost"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c1.ID}, out)
}

// TestFollowingRepository_FilterFollowings_QueryError covers the DB error
// branch of both Filter helpers (cancelled ctx → driver error → bubble up).
func TestFollowingRepository_FilterFollowings_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFollowingRepository(db)
	_, err := repo.FilterFollowingsFromAnchor("u", []string{"v"})
	assert.Error(t, err)
	_, err = repo.FilterFollowingsToAnchor("u", []string{"v"})
	assert.Error(t, err)
}

func TestFollowingRepository_Delete(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	follower := insertTestUser(t, "u_fl_5", "follower5")
	defer cleanupUser(t, follower.ID)
	followee := insertTestUser(t, "u_fl_6", "followee6")
	defer cleanupUser(t, followee.ID)

	f := insertFollowing(t, "fl_3", follower.ID, followee.ID)
	require.NoError(t, repo.Delete(f))

	exists, err := repo.Exists(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestFollowingRepository_ListFollowers_ListFollowing(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	a := insertTestUser(t, "u_fl_7", "user7")
	defer cleanupUser(t, a.ID)
	b := insertTestUser(t, "u_fl_8", "user8")
	defer cleanupUser(t, b.ID)
	c := insertTestUser(t, "u_fl_9", "user9")
	defer cleanupUser(t, c.ID)

	insertFollowing(t, "fl_4", a.ID, b.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_4")
	insertFollowing(t, "fl_5", c.ID, b.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_5")
	insertFollowing(t, "fl_6", a.ID, c.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_6")

	followers, err := repo.ListFollowers(b.ID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, followers, 2)

	followingsOfA, err := repo.ListFollowing(a.ID, 10, 0)
	require.NoError(t, err)
	assert.Len(t, followingsOfA, 2)

	// **offset 版に clamp を掛けないこと。** cursor 版と body を共有しているので、
	// 共有側へ clamp を移すと fanout (200) / CSV export (500) / stream snapshot
	// (200) が 100 に切り詰められ、いずれも `len(rows) < pageSize` でループを
	// 抜ける形なので**静かに取りこぼす** (#2712 review round 2 MEDIUM-1)。
	// limit=0 が判別子になる (clamp 無し = LIMIT 0 = 0 件 / clamp 有り = 既定 10)。
	zero, err := repo.ListFollowers(b.ID, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, zero, "offset 版は limit をそのまま渡す (LIMIT 0)")
	zeroSent, err := repo.ListFollowing(a.ID, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, zeroSent)
}

// ListFollowersWithCursor / ListFollowingWithCursor は sinceId / untilId を
// **SQL 側**で掛ける (#2711)。LIMIT のあとに捨てると 2 ページ目が空になる。
func TestFollowingRepository_ListWithCursor(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	target := insertTestUser(t, "u_cur_t", "curtarget")
	defer cleanupUser(t, target.ID)
	// **呼び出し側に id をリテラルで書く。** CI の重複 fixture 検査は
	// 挿入ヘルパの直後にダブルクォートが来る形しか grep しないので、変数や
	// fmt.Sprintf を挟むと検査から漏れる。id を別スライスへ出すだけでは
	// 足りない (#2712 review LOW-4 / round 2 MEDIUM-2)。
	followers := []*model.User{
		insertTestUser(t, "u_cur_0", "curuser0"),
		insertTestUser(t, "u_cur_1", "curuser1"),
		insertTestUser(t, "u_cur_2", "curuser2"),
		insertTestUser(t, "u_cur_3", "curuser3"),
		insertTestUser(t, "u_cur_4", "curuser4"),
	}
	ids := []string{"cur_1", "cur_2", "cur_3", "cur_4", "cur_5"}
	for i, fid := range ids {
		u := followers[i]
		defer cleanupUser(t, u.ID)
		// followers 側と following 側の両方を同じ id で作る。
		insertFollowing(t, fid, u.ID, target.ID)
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, fid)
		sent := fid + "_s"
		insertFollowing(t, sent, target.ID, u.ID)
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, sent)
	}

	// untilId: 新しい順に 2 件ずつ、重複なく最後まで辿れること。
	page1, err := repo.ListFollowersWithCursor(target.ID, "", "", 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	assert.Equal(t, []string{"cur_5", "cur_4"}, []string{page1[0].ID, page1[1].ID})

	page2, err := repo.ListFollowersWithCursor(target.ID, "", page1[1].ID, 2, 0)
	require.NoError(t, err)
	require.Len(t, page2, 2, "2 ページ目が空: cursor が SQL に渡っていない")
	assert.Equal(t, []string{"cur_3", "cur_2"}, []string{page2[0].ID, page2[1].ID})

	// sinceId: 昇順で、指定より新しい側を古い方から返す。
	asc, err := repo.ListFollowersWithCursor(target.ID, "cur_2", "", 2, 0)
	require.NoError(t, err)
	require.Len(t, asc, 2)
	assert.Equal(t, []string{"cur_3", "cur_4"}, []string{asc[0].ID, asc[1].ID})

	// following 側も同じ形。**両方向を見る** — 片側だけだと、#2711 と同じ形が
	// 反対側で再発しても検出できない (#2712 review MEDIUM-1)。
	sentPage, err := repo.ListFollowingWithCursor(target.ID, "", "cur_4_s", 2, 0)
	require.NoError(t, err)
	require.Len(t, sentPage, 2)
	assert.Equal(t, []string{"cur_3_s", "cur_2_s"}, []string{sentPage[0].ID, sentPage[1].ID})

	sentAsc, err := repo.ListFollowingWithCursor(target.ID, "cur_2_s", "", 2, 0)
	require.NoError(t, err)
	require.Len(t, sentAsc, 2)
	assert.Equal(t, []string{"cur_3_s", "cur_4_s"}, []string{sentAsc[0].ID, sentAsc[1].ID})

	// sinceId と untilId の同時指定は両方 exclusive で DESC (upstream
	// makePaginationQuery の第 1 分岐、#2712 review LOW-5)。
	both, err := repo.ListFollowersWithCursor(target.ID, "cur_1", "cur_5", 10, 0)
	require.NoError(t, err)
	require.Len(t, both, 3)
	assert.Equal(t, []string{"cur_4", "cur_3", "cur_2"},
		[]string{both[0].ID, both[1].ID, both[2].ID})

	// **cursor 指定時は offset を無視する** (本家 makePaginationQuery と同じ。
	// この package の他 10 ファイルと揃える、#2712 review MEDIUM-3)。
	withOffset, err := repo.ListFollowersWithCursor(target.ID, "", "cur_5", 2, 1)
	require.NoError(t, err)
	require.Len(t, withOffset, 2)
	assert.Equal(t, []string{"cur_4", "cur_3"}, []string{withOffset[0].ID, withOffset[1].ID},
		"cursor と併用した offset が効いている")

	// cursor 未指定なら offset は効く (offset 版の呼び出し元が依存している)。
	offsetOnly, err := repo.ListFollowersWithCursor(target.ID, "", "", 2, 1)
	require.NoError(t, err)
	require.Len(t, offsetOnly, 2)
	assert.Equal(t, []string{"cur_4", "cur_3"}, []string{offsetOnly[0].ID, offsetOnly[1].ID})

	// limit の clamp は cursor 版だけ。0 で 0 件・負値で全件にならないこと。
	clamped, err := repo.ListFollowersWithCursor(target.ID, "", "", 0, 0)
	require.NoError(t, err)
	assert.Len(t, clamped, 5, "limit=0 が LIMIT 0 になっている")
	clamped, err = repo.ListFollowersWithCursor(target.ID, "", "", -1, 0)
	require.NoError(t, err)
	assert.Len(t, clamped, 5)
}

// ListFollowersBefore / ListFollowingBefore は id DESC で cursor (id <) ページング
// する (AP followers/following collection の page 用, #1877)。
func TestFollowingRepository_ListFollowersBefore_ListFollowingBefore(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	target := insertTestUser(t, "u_fb_t", "fbtarget")
	defer cleanupUser(t, target.ID)
	f1 := insertTestUser(t, "u_fb_1", "fb1")
	defer cleanupUser(t, f1.ID)
	f2 := insertTestUser(t, "u_fb_2", "fb2")
	defer cleanupUser(t, f2.ID)
	f3 := insertTestUser(t, "u_fb_3", "fb3")
	defer cleanupUser(t, f3.ID)
	// f1,f2,f3 follow target → target の followers。row id 昇順 fb_a < fb_b < fb_c。
	insertFollowing(t, "fb_a", f1.ID, target.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fb_a")
	insertFollowing(t, "fb_b", f2.ID, target.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fb_b")
	insertFollowing(t, "fb_c", f3.ID, target.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fb_c")

	// cursor 無し → 全件 id DESC。
	all, err := repo.ListFollowersBefore(target.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, "fb_c", all[0].ID)
	assert.Equal(t, "fb_a", all[2].ID)

	// limit。
	lim, err := repo.ListFollowersBefore(target.ID, "", 2)
	require.NoError(t, err)
	assert.Len(t, lim, 2)

	// cursor: id < "fb_c" → fb_b, fb_a。
	page, err := repo.ListFollowersBefore(target.ID, "fb_c", 10)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "fb_b", page[0].ID)

	// following 側: target が g1 を follow。
	g1 := insertTestUser(t, "u_fb_g", "fbg")
	defer cleanupUser(t, g1.ID)
	insertFollowing(t, "fb_g", target.ID, g1.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fb_g")
	flw, err := repo.ListFollowingBefore(target.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, flw, 1)
	assert.Equal(t, "fb_g", flw[0].ID)
}

// ListFollowersToNotify は notify='normal' の follower row だけを返す (#1559)。
func TestFollowingRepository_ListFollowersToNotify(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	followee := insertTestUser(t, "u_ntf_fe", "ntffe")
	defer cleanupUser(t, followee.ID)
	notifyFollower := insertTestUser(t, "u_ntf_on", "ntfon")
	defer cleanupUser(t, notifyFollower.ID)
	silentFollower := insertTestUser(t, "u_ntf_off", "ntfoff")
	defer cleanupUser(t, silentFollower.ID)

	// notify='normal' の follow edge
	normal := "normal"
	on := &model.Following{ID: "ntf_on", FollowerID: notifyFollower.ID, FolloweeID: followee.ID, Notify: &normal}
	require.NoError(t, testDB.Create(on).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, on.ID)
	// notify 無し (null) の follow edge は対象外
	insertFollowing(t, "ntf_off", silentFollower.ID, followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "ntf_off")

	rows, err := repo.ListFollowersToNotify(followee.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, notifyFollower.ID, rows[0].FollowerID)
}

// ListLocalFollowerIDs は followerHost IS NULL のフォロワーだけを全件返す
// (#2418 アカウント移行のフォロワー引き継ぎ用)。リモートフォロワーと、別ユーザーの
// フォロワーを取り込まないことを固定する。
func TestFollowingRepository_ListLocalFollowerIDs(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	followee := insertTestUser(t, "u_llf_fe", "llffe")
	defer cleanupUser(t, followee.ID)
	other := insertTestUser(t, "u_llf_other", "llfother")
	defer cleanupUser(t, other.ID)
	localA := insertTestUser(t, "u_llf_a", "llfa")
	defer cleanupUser(t, localA.ID)
	localB := insertTestUser(t, "u_llf_b", "llfb")
	defer cleanupUser(t, localB.ID)
	remote := insertTestUser(t, "u_llf_r", "llfr")
	defer cleanupUser(t, remote.ID)

	insertFollowing(t, "llf_a", localA.ID, followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "llf_a")
	insertFollowing(t, "llf_b", localB.ID, followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "llf_b")
	// followerHost 有り = リモートフォロワーは対象外。
	host := "remote.example"
	remoteRow := &model.Following{
		ID: "llf_r", FollowerID: remote.ID, FolloweeID: followee.ID, FollowerHost: &host,
	}
	require.NoError(t, testDB.Create(remoteRow).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, remoteRow.ID)
	// 別ユーザーのフォロワーも対象外。
	insertFollowing(t, "llf_other", localA.ID, other.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "llf_other")

	ids, err := repo.ListLocalFollowerIDs(followee.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{localA.ID, localB.ID}, ids)

	// フォロワーが居なければ空。
	empty, err := repo.ListLocalFollowerIDs(other.ID + "-nobody")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestFollowingRepository_QueryErrors(t *testing.T) {
	// cancelledなcontextを使ってgorm経由でerrorを発生させる
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFollowingRepository(db)

	_, err := repo.Exists("a", "b")
	assert.Error(t, err)

	_, err = repo.ListFollowers("a", 10, 0)
	assert.Error(t, err)

	_, err = repo.ListFollowersToNotify("a")
	assert.Error(t, err)

	_, err = repo.ListFollowing("a", 10, 0)
	assert.Error(t, err)

	_, err = repo.ListRemoteFollowerInboxes("a")
	assert.Error(t, err)
}

func TestFollowingRepository_ListRemoteFollowerInboxes(t *testing.T) {
	repo := NewFollowingRepository(testDB)

	// Local followee (id を短く保つ — token は char(16) なので "tok_"+id<=16)
	followee := insertTestUser(t, "u_inb_fe", "inbfe")
	defer cleanupUser(t, followee.ID)

	// Remote follower with sharedInbox
	host := "remote.example"
	sharedInbox1 := "https://remote.example/inbox"
	inbox1 := "https://remote.example/users/r1/inbox"
	uri1 := "https://remote.example/users/r1"
	require.NoError(t, testDB.Exec(`INSERT INTO "user" (id, username, "usernameLower", host, uri, inbox, "sharedInbox", "avatarDecorations") VALUES (?, ?, ?, ?, ?, ?, ?, '[]')`,
		"u_inb_r1", "r1", "r1", host, uri1, inbox1, sharedInbox1).Error)
	defer cleanupUser(t, "u_inb_r1")

	// Remote follower with same sharedInbox (集約対象)
	inbox2 := "https://remote.example/users/r2/inbox"
	uri2 := "https://remote.example/users/r2"
	require.NoError(t, testDB.Exec(`INSERT INTO "user" (id, username, "usernameLower", host, uri, inbox, "sharedInbox", "avatarDecorations") VALUES (?, ?, ?, ?, ?, ?, ?, '[]')`,
		"u_inb_r2", "r2", "r2", host, uri2, inbox2, sharedInbox1).Error)
	defer cleanupUser(t, "u_inb_r2")

	// Remote follower without sharedInbox (個別inboxを返す)
	host2 := "other.example"
	inbox3 := "https://other.example/users/r3/inbox"
	uri3 := "https://other.example/users/r3"
	require.NoError(t, testDB.Exec(`INSERT INTO "user" (id, username, "usernameLower", host, uri, inbox, "avatarDecorations") VALUES (?, ?, ?, ?, ?, ?, '[]')`,
		"u_inb_r3", "r3", "r3", host2, uri3, inbox3).Error)
	defer cleanupUser(t, "u_inb_r3")

	// Local follower (除外対象)
	local := insertTestUser(t, "u_inb_lo", "inblo")
	defer cleanupUser(t, local.ID)

	insertFollowing(t, "fl_inb_1", "u_inb_r1", followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_inb_1")
	insertFollowing(t, "fl_inb_2", "u_inb_r2", followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_inb_2")
	insertFollowing(t, "fl_inb_3", "u_inb_r3", followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_inb_3")
	insertFollowing(t, "fl_inb_4", local.ID, followee.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_inb_4")

	inboxes, err := repo.ListRemoteFollowerInboxes(followee.ID)
	require.NoError(t, err)
	// #1811: sharedInbox 集約は Shared=true、個別 inbox は Shared=false。
	assert.ElementsMatch(t, []model.RemoteInbox{
		{Inbox: sharedInbox1, Shared: true},
		{Inbox: inbox3, Shared: false},
	}, inboxes)
}

func TestUserRepository_IncrementFollowingCount(t *testing.T) {
	repo := NewUserRepository(testDB)
	user := insertTestUser(t, "u_f_inc_1", "fincuser1")
	defer cleanupUser(t, user.ID)

	require.NoError(t, repo.IncrementFollowingCount(user.ID, 1))
	found, _ := repo.FindByID(user.ID)
	assert.Equal(t, 1, found.FollowingCount)

	require.NoError(t, repo.IncrementFollowingCount(user.ID, -1))
	found, _ = repo.FindByID(user.ID)
	assert.Equal(t, 0, found.FollowingCount)
}

// DeleteAllByUser は target が follower か followee のどちらに入っていても
// 消え、target と無関係な行には触らないことを確認する。
func TestFollowingRepository_DeleteAllByUser(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	target := insertTestUser(t, "u_dab_target", "dabtarget")
	other := insertTestUser(t, "u_dab_other", "dabother")
	bystander := insertTestUser(t, "u_dab_by", "dabby")
	defer cleanupUser(t, target.ID)
	defer cleanupUser(t, other.ID)
	defer cleanupUser(t, bystander.ID)

	insertFollowing(t, "fl_dab_1", target.ID, other.ID)
	insertFollowing(t, "fl_dab_2", other.ID, target.ID)
	insertFollowing(t, "fl_dab_3", other.ID, bystander.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id IN (?, ?, ?)`, "fl_dab_1", "fl_dab_2", "fl_dab_3")

	n, err := repo.DeleteAllByUser(target.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	// target 絡みの 2 件は消え、無関係は残る
	_, err = repo.FindByPair(target.ID, other.ID)
	assert.Error(t, err)
	_, err = repo.FindByPair(other.ID, target.ID)
	assert.Error(t, err)
	found, err := repo.FindByPair(other.ID, bystander.ID)
	require.NoError(t, err)
	assert.Equal(t, "fl_dab_3", found.ID)
}

func TestUserRepository_IncrementFollowersCount(t *testing.T) {
	repo := NewUserRepository(testDB)
	user := insertTestUser(t, "u_inc_2", "incuser2")
	defer cleanupUser(t, user.ID)

	require.NoError(t, repo.IncrementFollowersCount(user.ID, 1))
	found, _ := repo.FindByID(user.ID)
	assert.Equal(t, 1, found.FollowersCount)

	require.NoError(t, repo.IncrementFollowersCount(user.ID, -1))
	found, _ = repo.FindByID(user.ID)
	assert.Equal(t, 0, found.FollowersCount)
}

// insertBirthdayProfile は user_profile 行を birthday 付きで直接 INSERT する。
// 他のテストで既存プロフィールが作られている場合は UPSERT になる。
func insertBirthdayProfile(t *testing.T, userID, birthday string) {
	t.Helper()
	require.NoError(t, testDB.Exec(
		`INSERT INTO "user_profile" ("userId", birthday) VALUES (?, ?)
         ON CONFLICT ("userId") DO UPDATE SET birthday = EXCLUDED.birthday`,
		userID, birthday).Error)
}

func TestFollowingRepository_ListFollowingByBirthday(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	me := insertTestUser(t, "u_bd_me", "bdme")
	defer cleanupUser(t, me.ID)
	fe1 := insertTestUser(t, "u_bd_fe1", "bdfe1")
	defer cleanupUser(t, fe1.ID)
	fe2 := insertTestUser(t, "u_bd_fe2", "bdfe2")
	defer cleanupUser(t, fe2.ID)
	fe3 := insertTestUser(t, "u_bd_fe3", "bdfe3")
	defer cleanupUser(t, fe3.ID)

	insertBirthdayProfile(t, fe1.ID, "1990-05-10")
	insertBirthdayProfile(t, fe2.ID, "1991-12-30")
	insertBirthdayProfile(t, fe3.ID, "1992-01-03")

	insertFollowing(t, "fl_bd_1", me.ID, fe1.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_bd_1")
	insertFollowing(t, "fl_bd_2", me.ID, fe2.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_bd_2")
	insertFollowing(t, "fl_bd_3", me.ID, fe3.ID)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "fl_bd_3")

	// 単発: 5/10 のみ。
	rows, err := repo.ListFollowingByBirthday(me.ID, 510, 510, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, fe1.ID, rows[0].FolloweeID)

	// 範囲 (年跨ぎ): 12/25..1/5 → fe2, fe3 (mmdd 昇順なので 103, 1230)。
	rows2, err := repo.ListFollowingByBirthday(me.ID, 1225, 105, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows2, 2)
	// mmdd ASC なので fe3 (0103) が先、fe2 (1230) が後。
	assert.Equal(t, fe3.ID, rows2[0].FolloweeID)
	assert.Equal(t, fe2.ID, rows2[1].FolloweeID)

	// 範囲 (正常): 1/1..6/30 → fe1 (05-10), fe3 (01-03)。
	rows3, err := repo.ListFollowingByBirthday(me.ID, 101, 630, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows3, 2)
}

func TestFollowingRepository_ListFollowingByBirthday_LimitDefaultsAndOffset(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	// limit <= 0 / > 100 のパスを通す。呼び出しが成功すれば良い。
	_, err := repo.ListFollowingByBirthday("nobody", 1, 1231, 0, 0)
	require.NoError(t, err)
	_, err = repo.ListFollowingByBirthday("nobody", 1, 1231, 500, 0)
	require.NoError(t, err)
}

func TestFollowingRepository_ListFollowingByBirthday_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewFollowingRepository(testDB.WithContext(ctx))
	_, err := repo.ListFollowingByBirthday("me", 1, 1231, 10, 0)
	assert.Error(t, err)
}

// --- 追加テスト (#260 repository coverage: 0%関数) ---

// TestFollowingRepository_ListFollowersByHost guards upstream parity: the
// federation/followers query filters by followeeHost (= followed users belong
// to the given remote host), matching 本家 followers.ts. A row whose
// followerHost (but not followeeHost) matches must NOT be returned.
func TestFollowingRepository_ListFollowersByHost(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	localr := insertTestUser(t, "flh_l1", "localfollower")
	defer cleanupUser(t, localr.ID)

	host := "remote.example"
	remoteFollowee := &model.User{
		ID:                "flh_e1",
		Username:          "remote_followee",
		UsernameLower:     "remote_followee",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteFollowee).Error)
	defer cleanupUser(t, remoteFollowee.ID)

	// followeeHost=host のフォロー (local が remote をフォロー) が対象。
	match := &model.Following{
		ID:           "flh_match",
		FollowerID:   localr.ID,
		FolloweeID:   remoteFollowee.ID,
		FolloweeHost: &host,
	}
	require.NoError(t, repo.Create(match))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, match.ID)

	// followerHost=host のみのフォローは federation/followers では対象外。
	remoteFollower := &model.User{
		ID:                "flh_r1",
		Username:          "remote_follower",
		UsernameLower:     "remote_follower",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteFollower).Error)
	defer cleanupUser(t, remoteFollower.ID)
	miss := &model.Following{
		ID:           "flh_miss",
		FollowerID:   remoteFollower.ID,
		FolloweeID:   localr.ID,
		FollowerHost: &host,
	}
	require.NoError(t, repo.Create(miss))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, miss.ID)

	rows, err := repo.ListFollowersByHost(host, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, match.ID, rows[0].ID)

	// limit のデフォルト (0 → 30) と cap (>100 → 100) の分岐を通す
	_, err = repo.ListFollowersByHost(host, 0, 0)
	require.NoError(t, err)
	_, err = repo.ListFollowersByHost(host, 999, 0)
	require.NoError(t, err)
}

// TestFollowingRepository_ListFollowingByHost guards upstream parity: the
// federation/following query filters by followerHost (= followers belong to
// the given remote host), matching 本家 following.ts. A row whose followeeHost
// (but not followerHost) matches must NOT be returned.
func TestFollowingRepository_ListFollowingByHost(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	localee := insertTestUser(t, "flg_l1", "localfollowee")
	defer cleanupUser(t, localee.ID)

	host := "remote.example"
	remoteFollower := &model.User{
		ID:                "flg_r1",
		Username:          "remote_follower",
		UsernameLower:     "remote_follower",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteFollower).Error)
	defer cleanupUser(t, remoteFollower.ID)

	// followerHost=host のフォロー (remote が local をフォロー) が対象。
	match := &model.Following{
		ID:           "flg_match",
		FollowerID:   remoteFollower.ID,
		FolloweeID:   localee.ID,
		FollowerHost: &host,
	}
	require.NoError(t, repo.Create(match))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, match.ID)

	// followeeHost=host のみのフォローは federation/following では対象外。
	remoteFollowee := &model.User{
		ID:                "flg_e1",
		Username:          "remote_followee",
		UsernameLower:     "remote_followee",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(remoteFollowee).Error)
	defer cleanupUser(t, remoteFollowee.ID)
	miss := &model.Following{
		ID:           "flg_miss",
		FollowerID:   localee.ID,
		FolloweeID:   remoteFollowee.ID,
		FolloweeHost: &host,
	}
	require.NoError(t, repo.Create(miss))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, miss.ID)

	rows, err := repo.ListFollowingByHost(host, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, match.ID, rows[0].ID)

	_, err = repo.ListFollowingByHost(host, 0, 0)
	require.NoError(t, err)
	_, err = repo.ListFollowingByHost(host, 999, 0)
	require.NoError(t, err)
}

func TestFollowingRepository_UpdateRelation(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	u1 := insertTestUser(t, "fur_u1", "fur_u1")
	u2 := insertTestUser(t, "fur_u2", "fur_u2")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)

	f := &model.Following{ID: "fur_1", FollowerID: u1.ID, FolloweeID: u2.ID}
	require.NoError(t, repo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)

	// 空fieldsはno-op
	require.NoError(t, repo.UpdateRelation(u1.ID, u2.ID, map[string]any{}))

	// followerHostを更新
	newHost := "updated.example"
	require.NoError(t, repo.UpdateRelation(u1.ID, u2.ID, map[string]any{"followerHost": newHost}))

	found, err := repo.FindByPair(u1.ID, u2.ID)
	require.NoError(t, err)
	require.NotNil(t, found.FollowerHost)
	assert.Equal(t, newHost, *found.FollowerHost)
}

func TestFollowingRepository_UpdateAllByFollower(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	u1 := insertTestUser(t, "uab_u1", "uab_u1")
	u2 := insertTestUser(t, "uab_u2", "uab_u2")
	u3 := insertTestUser(t, "uab_u3", "uab_u3")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	defer cleanupUser(t, u3.ID)

	for _, id := range []string{"uab_1", "uab_2"} {
		f := &model.Following{ID: id, FollowerID: u1.ID, FolloweeID: u2.ID}
		if id == "uab_2" {
			f.FolloweeID = u3.ID
		}
		require.NoError(t, repo.Create(f))
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, id)
	}

	// 空fieldsはno-op
	require.NoError(t, repo.UpdateAllByFollower(u1.ID, map[string]any{}))

	// 一括更新
	inbox := "https://remote.example/inbox"
	require.NoError(t, repo.UpdateAllByFollower(u1.ID, map[string]any{"followerInbox": inbox}))
}

var _ = context.Background // import guard (used elsewhere in file)

// clampRelationLimit は cursor 版の public method 用の境界。
//
// **offset 版には掛けない** — fanout (200) / CSV export (500) が切り詰められる。
// いまの唯一の呼び出し元は pagination.ResolveLimit が 1..100 を保証しているが、
// interface に生えているので limit=0 で 0 件・負値で LIMIT 句ごと消えて全件、
// という壊れ方を塞ぐ (#2712 review LOW-3)。
func TestClampRelationLimit(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"zero falls back to the default", 0, 10},
		{"negative falls back to the default", -1, 10},
		{"one is kept", 1, 1},
		{"upper bound is kept", 100, 100},
		{"above the upper bound is capped", 101, 100},
		{"far above the upper bound is capped", 100000, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, clampRelationLimit(tc.limit))
		})
	}
}
