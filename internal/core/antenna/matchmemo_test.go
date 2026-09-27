package antenna

import (
	"testing"

	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// 同じ note を全アンテナで評価するので、owner ごとの follow 判定は 1 回だけ
// 引いて使い回す。アンテナ数に比例して DB を叩くと fan-out が note 作成に
// 対して遅れ、直後の antennas/notes が取りこぼす。
func TestMatchMemo_OwnerFollowsIsCachedPerNote(t *testing.T) {
	svc, _ := newSvc(t)
	followingRepo := testutil.NewMockFollowingRepository()
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "owner", FolloweeID: "author"}
	svc.SetFollowingRepo(followingRepo)

	memo := newMatchMemo()
	assert.True(t, memo.ownerFollows(svc, "owner", "author"))
	assert.True(t, memo.ownerFollows(svc, "owner", "author"), "2 回目はキャッシュから返す")
	assert.False(t, memo.ownerFollows(svc, "stranger", "author"), "owner ごとに独立して判定する")
}

// memo が nil でも (単体テスト等の直接呼び出し) 素通しで動く。
func TestMatchMemo_NilFallsBackToDirectLookup(t *testing.T) {
	svc, _ := newSvc(t)
	followingRepo := testutil.NewMockFollowingRepository()
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "owner", FolloweeID: "author"}
	svc.SetFollowingRepo(followingRepo)

	var memo *matchMemo
	assert.True(t, memo.ownerFollows(svc, "owner", "author"))
	assert.False(t, memo.listContains(svc, "owner", "missing-list", "author"))
}

// list メンバーシップも note 単位でキャッシュする。
func TestMatchMemo_ListMembership(t *testing.T) {
	svc, _ := newSvc(t)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "owner", Name: "l"}
	listRepo.Members = []*model.UserListMembership{{ID: "m1", UserListID: "l1", UserID: "author"}}
	svc.SetUserListRepo(listRepo)

	memo := newMatchMemo()
	assert.True(t, memo.listContains(svc, "owner", "l1", "author"))
	assert.True(t, memo.listContains(svc, "owner", "l1", "author"), "2 回目はキャッシュから返す")
	assert.False(t, memo.listContains(svc, "owner", "l1", "stranger"))
}

// #2752: followers 可視の note では、**可視性 gate の follow 判定も** memo を
// 通すこと。通していないと matchNote 冒頭の CanSeeNote がアンテナ 1 件ごとに
// `Exists` を逐次で飛ばす (memo が効くのは matchSource 側だけだった)。
func TestMatchNote_FollowLookupIsMemoizedAcrossAntennas(t *testing.T) {
	svc, repo := newSvc(t)
	follows := newCountingFollowingRepo()
	follows.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "owner", FolloweeID: "author"}
	svc.SetFollowingRepo(follows)

	// 同じ owner の active antenna を 5 件用意する。
	for i := 0; i < 5; i++ {
		a := &model.Antenna{
			ID: string(rune('a'+i)) + "1", UserID: "owner", Src: model.AntennaSourceAll,
			IsActive: true, Keywords: datatypes.JSON([]byte(`[["猫"]]`)),
		}
		repo.Antennas[a.ID] = a
	}

	text := "猫がかわいい"
	n := &model.Note{
		ID: "n1", UserID: "author", Text: &text,
		Visibility: model.NoteVisibilityFollowers,
	}
	svc.OnNoteCreated(n, &model.User{ID: "author"})

	assert.Equal(t, 1, follows.existsCalls,
		"アンテナ数によらず follow 判定は note 1 件あたり 1 回")
}

// followingRepo 未配線での fail-closed は
// TestMatchNote_FollowersVisibility_NilFollowingRepoFailClosed
// (antenna_service_test.go) が既に固定しているのでここには足さない。

// #2752: memo のキーは **(owner, author) の対**であること。
//
// 従来は ownerID だけで足りていた。唯一の呼び出し元 (matchSource) が常に
// `author.ID` を渡していたため。可視性 gate が `n.UserID` を渡すようになり、
// **同じキーに 2 種類の質問が載る**形になった。今はどの呼び出し元でも
// `author.ID == n.UserID` だが、そこは型で守られていないので、キーの側で閉じる。
func TestMatchMemo_KeyIncludesAuthor(t *testing.T) {
	svc, _ := newSvc(t)
	followingRepo := testutil.NewMockFollowingRepository()
	// owner は "a1" をフォローしているが "a2" はしていない。
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "owner", FolloweeID: "a1"}
	svc.SetFollowingRepo(followingRepo)

	memo := newMatchMemo()
	assert.True(t, memo.ownerFollows(svc, "owner", "a1"))
	assert.False(t, memo.ownerFollows(svc, "owner", "a2"),
		"owner が同じでも author が違えば別の答えを返すこと")
	// 逆順でも同じ (先に false を入れてから true を引く)。
	memo2 := newMatchMemo()
	assert.False(t, memo2.ownerFollows(svc, "owner", "a2"))
	assert.True(t, memo2.ownerFollows(svc, "owner", "a1"))
}
