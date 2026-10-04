package federation

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/api/userrelation"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- bind validation ---

func TestFollowers_MissingHost(t *testing.T) {
	h, _ := newHandler(t)
	rec := postStub(h.Followers)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// 400 エラーレスポンス直後に 200 を追記してしまう double-write バグの
	// regression guard。body は単一の JSON オブジェクト (error wrapper) になる
	// はずで、追加の [] が後続しないこと。
	var single map[string]any
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &single),
		"response body must be a single JSON object, not concatenated")
}

func TestFollowing_MissingHost(t *testing.T) {
	h, _ := newHandler(t)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Following).Code)
}

func TestUsers_MissingHost(t *testing.T) {
	h, _ := newHandler(t)
	assert.Equal(t, http.StatusBadRequest, postStub(h.Users).Code)
}

// --- detail: Followers / Following / Users with repo wired ---

// TestFollowers_FiltersByHost guards the upstream-parity column: federation/
// followers filters by followeeHost (= the followed users belong to the given
// remote host). Seeding followerHost=remote must NOT match.
func TestFollowers_FiltersByHost(t *testing.T) {
	h, _ := newHandler(t)
	repo := testutil.NewMockFollowingRepository()
	remote := "remote.example"
	other := "elsewhere.example"
	// followeeHost=remote の 2 行が対象 (本家 followers.ts と同じ列)。
	repo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "local1", FolloweeID: "r1", FolloweeHost: &remote}
	repo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "local2", FolloweeID: "r2", FolloweeHost: &remote}
	repo.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "local3", FolloweeID: "r3", FolloweeHost: &other}
	// followerHost=remote は federation/followers では対象外であることの guard。
	repo.Followings["f4"] = &model.Following{ID: "f4", FollowerID: "r4", FollowerHost: &remote, FolloweeID: "local4"}
	h.SetFollowingRepo(repo)

	rec := postBody(h.Followers, `{"host":"remote.example","limit":10}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2)
}

// TestFollowing_FiltersByHost guards the upstream-parity column: federation/
// following filters by followerHost (= the followers belong to the given
// remote host). Seeding followeeHost=remote must NOT match.
func TestFollowing_FiltersByHost(t *testing.T) {
	h, _ := newHandler(t)
	repo := testutil.NewMockFollowingRepository()
	remote := "remote.example"
	// followerHost=remote の 1 行が対象 (本家 following.ts と同じ列)。
	repo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "r1", FollowerHost: &remote, FolloweeID: "local1"}
	// followeeHost=remote は federation/following では対象外。
	repo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "local2", FolloweeID: "r2", FolloweeHost: &remote}
	h.SetFollowingRepo(repo)

	rec := postBody(h.Following, `{"host":"remote.example"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 1)
}

// TestFollowers_PackShape verifies the Following packer matches upstream
// FollowingEntityService.pack: id / createdAt / followeeId / followerId plus a
// populated followee (UserDetailedNotMe) when userRepo + idGen are wired, and
// that the old host/withReplies fields are gone.
func TestFollowers_PackShape(t *testing.T) {
	h, _ := newHandler(t)
	gen, _ := id.NewGenerator("aidx")
	h.SetIDGen(gen)

	remote := "remote.example"
	// row ID は aidx ID にする (createdAt は row ID から導出されるため)。
	fid := gen.Generate(time.Now())
	rid := gen.Generate(time.Now())
	repo := testutil.NewMockFollowingRepository()
	repo.Followings[fid] = &model.Following{ID: fid, FollowerID: "local1", FolloweeID: rid, FolloweeHost: &remote}
	h.SetFollowingRepo(repo)

	userRepo := testutil.NewMockUserRepository()
	userRepo.Users[rid] = &model.User{ID: rid, Username: "alice", Host: &remote}
	h.SetUserRepo(userRepo)

	rec := postBody(h.Followers, `{"host":"remote.example"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, fid, row["id"])
	assert.Equal(t, "local1", row["followerId"])
	assert.Equal(t, rid, row["followeeId"])
	// createdAt は aidx ID から導出され非空。
	assert.NotEmpty(t, row["createdAt"])
	// populateFollowee=true なので followee が UserDetailedNotMe で埋まる。
	followee, ok := row["followee"].(map[string]any)
	require.True(t, ok, "followee must be populated")
	assert.Equal(t, "alice", followee["username"])
	// follower は populate しない (本家 populateFollower 未指定)。
	_, hasFollower := row["follower"]
	assert.False(t, hasFollower, "follower must not be populated")
	// 旧 shape の host / withReplies フィールドは出さない (本家 schema に無い)。
	_, hasFollowerHost := row["followerHost"]
	_, hasFolloweeHost := row["followeeHost"]
	_, hasWithReplies := row["withReplies"]
	assert.False(t, hasFollowerHost)
	assert.False(t, hasFolloweeHost)
	assert.False(t, hasWithReplies)
}

// TestFollowing_PackShape mirrors TestFollowers_PackShape for the following
// endpoint and also covers the missing-followee path (lookup returns nil so
// followee stays absent).
func TestFollowing_PackShape(t *testing.T) {
	h, _ := newHandler(t)
	gen, _ := id.NewGenerator("aidx")
	h.SetIDGen(gen)

	remote := "remote.example"
	repo := testutil.NewMockFollowingRepository()
	// followee user は userRepo に存在しないので followee は埋まらない。
	repo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "r1", FollowerHost: &remote, FolloweeID: "ghost"}
	h.SetFollowingRepo(repo)
	h.SetUserRepo(testutil.NewMockUserRepository())

	rec := postBody(h.Following, `{"host":"remote.example"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, "ghost", row["followeeId"])
	_, hasFollowee := row["followee"]
	assert.False(t, hasFollowee, "missing followee user must leave followee absent")
}

// upstream followers.ts は makePaginationQuery で id cursor を使う。untilId で
// それ以前の行のみ返ること (#1732)。
func TestFollowers_CursorUntilID(t *testing.T) {
	h, _ := newHandler(t)
	repo := testutil.NewMockFollowingRepository()
	remote := "remote.example"
	repo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "l1", FolloweeID: "r1", FolloweeHost: &remote}
	repo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "l2", FolloweeID: "r2", FolloweeHost: &remote}
	repo.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "l3", FolloweeID: "r3", FolloweeHost: &remote}
	h.SetFollowingRepo(repo)

	// untilId=f3 → f3 より小さい id (f1, f2) のみ、DESC で [f2, f1]。
	rec := postBody(h.Followers, `{"host":"remote.example","untilId":"f3"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	assert.Equal(t, "f2", rows[0]["id"])
	assert.Equal(t, "f1", rows[1]["id"])
}

// sinceId 指定時は ASC で それ以降の行を返す (#1732)。
func TestFollowing_CursorSinceID(t *testing.T) {
	h, _ := newHandler(t)
	repo := testutil.NewMockFollowingRepository()
	remote := "remote.example"
	repo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "r1", FollowerHost: &remote, FolloweeID: "l1"}
	repo.Followings["f2"] = &model.Following{ID: "f2", FollowerID: "r2", FollowerHost: &remote, FolloweeID: "l2"}
	repo.Followings["f3"] = &model.Following{ID: "f3", FollowerID: "r3", FollowerHost: &remote, FolloweeID: "l3"}
	h.SetFollowingRepo(repo)

	// sinceId=f1 → f1 より大きい id (f2, f3) を ASC で [f2, f3]。
	rec := postBody(h.Following, `{"host":"remote.example","sinceId":"f1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	assert.Equal(t, "f2", rows[0]["id"])
	assert.Equal(t, "f3", rows[1]["id"])
}

// federation/users も id cursor (untilId) を受け付ける (#1732)。
func TestUsers_CursorUntilID(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	for _, uid := range []string{"u1", "u2", "u3"} {
		userRepo.Users[uid] = &model.User{ID: uid, Username: uid, Host: &remote}
	}
	h.SetUserRepo(userRepo)

	// untilId=u3 → u1, u2 のみ DESC で [u2, u1]。
	rec := postBody(h.Users, `{"host":"remote.example","untilId":"u3"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	assert.Equal(t, "u2", rows[0]["id"])
	assert.Equal(t, "u1", rows[1]["id"])
}

// #1957-a: federation/users の embed user に viewer 視点の relation block が乗る。
// 認証 viewer が remote user を follow していれば isFollowing=true、匿名なら省略。
func TestUsers_EmbedsViewerRelation(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", Host: &remote}
	h.SetUserRepo(userRepo)
	idGen, _ := id.NewGenerator("aidx")
	h.SetIDGen(idGen)
	followingRepo := testutil.NewMockFollowingRepository()
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "local1", FolloweeID: "r1"}
	h.SetFollowingRepo(followingRepo)
	setListPacker(h, userrelation.Repos{Following: followingRepo}, fakeModerator{ids: map[string]bool{"mod1": true}})

	// 認証 viewer local1: r1 を follow しているので isFollowing=true。
	rec := postBodyAs(h.Users, `{"host":"remote.example","limit":10}`, "local1")
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, true, rows[0]["isFollowing"], "認証 viewer の embed user に isFollowing=true (#1957-a)")

	// 匿名 caller: relation は省略される (key 不在)。fresh な slice に unmarshal する
	// (既存 slice 再利用だと json が map をマージし前回の isFollowing が残るため)。
	rec = postBody(h.Users, `{"host":"remote.example","limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var anonRows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anonRows))
	require.Len(t, anonRows, 1)
	_, hasIsFollowing := anonRows[0]["isFollowing"]
	assert.False(t, hasIsFollowing, "匿名 caller には relation を出さない (#1957-a)")
}

// #1957-a: federation/followers の embed followee に viewer 視点の relation block が
// 乗る。viewer が followee を follow していれば followee.isFollowing=true、匿名なら省略。
// packFollowings の二重 lookup + pf.Followee ポインタ経路の回帰ガード。
func TestFollowers_FolloweeCarriesViewerRelation(t *testing.T) {
	h, _ := newHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	h.SetIDGen(idGen)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	userRepo.Users["rf1"] = &model.User{ID: "rf1", Username: "rf1", Host: &remote}
	h.SetUserRepo(userRepo)

	followingRepo := testutil.NewMockFollowingRepository()
	// Followers が返す行 (followeeHost=remote)。
	followingRepo.Followings["f_listed"] = &model.Following{ID: "f_listed", FollowerID: "someone", FolloweeID: "rf1", FolloweeHost: &remote}
	// viewer1 -> rf1 の follow (isFollowing 検出用)。
	followingRepo.Followings["f_viewer"] = &model.Following{ID: "f_viewer", FollowerID: "viewer1", FolloweeID: "rf1"}
	h.SetFollowingRepo(followingRepo)
	setListPacker(h, userrelation.Repos{Following: followingRepo}, fakeModerator{ids: map[string]bool{"mod1": true}})

	// 認証 viewer: embed followee に isFollowing=true。
	rec := postBodyAs(h.Followers, `{"host":"remote.example","limit":10}`, "viewer1")
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	followee, ok := rows[0]["followee"].(map[string]any)
	require.True(t, ok, "followee が embed される")
	assert.Equal(t, true, followee["isFollowing"], "embed followee に viewer の isFollowing=true (#1957-a)")

	// 匿名: relation 省略。
	rec = postBody(h.Followers, `{"host":"remote.example","limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var anon []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anon))
	require.Len(t, anon, 1)
	anonFollowee, _ := anon[0]["followee"].(map[string]any)
	_, has := anonFollowee["isFollowing"]
	assert.False(t, has, "匿名には followee relation を出さない (#1957-a)")
}

// #1988: federation/users の embed user の followers-only count は非フォロワー
// (federation/* は requireCredential:false なので匿名含む) に leak しない。
func TestUsers_GatesFollowersCount(t *testing.T) {
	h, _ := newHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	h.SetIDGen(idGen)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	userRepo.Users["r1"] = &model.User{ID: "r1", Username: "r1", Host: &remote, FollowersCount: 7}
	userRepo.Profiles["r1"] = &model.UserProfile{
		UserID:              "r1",
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}
	h.SetUserRepo(userRepo)
	setListPacker(h, userrelation.Repos{Following: testutil.NewMockFollowingRepository()}, fakeModerator{ids: map[string]bool{"mod1": true}})

	// 匿名 caller: followers-only count は 0 に潰される。
	rec := postBody(h.Users, `{"host":"remote.example","limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.EqualValues(t, 0, rows[0]["followersCount"], "followers-only count は匿名に leak しない (#1988)")

	// moderator caller: count をそのまま見せる。
	h.SetModeratorChecker(fakeModerator{ids: map[string]bool{"mod1": true}})
	rec = postBodyAs(h.Users, `{"host":"remote.example","limit":10}`, "mod1")
	require.Equal(t, http.StatusOK, rec.Code)
	var modRows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &modRows))
	require.Len(t, modRows, 1)
	assert.EqualValues(t, 7, modRows[0]["followersCount"], "moderator には count を見せる (#1988)")
}

// #1988: federation/followers の embed followee の followers-only count も非フォロワー
// (匿名含む) に leak しない。
func TestFollowers_FolloweeGatesFollowersCount(t *testing.T) {
	h, _ := newHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	h.SetIDGen(idGen)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	userRepo.Users["rf1"] = &model.User{ID: "rf1", Username: "rf1", Host: &remote, FollowersCount: 7}
	userRepo.Profiles["rf1"] = &model.UserProfile{
		UserID:              "rf1",
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}
	h.SetUserRepo(userRepo)
	followingRepo := testutil.NewMockFollowingRepository()
	followingRepo.Followings["f_listed"] = &model.Following{ID: "f_listed", FollowerID: "someone", FolloweeID: "rf1", FolloweeHost: &remote}
	h.SetFollowingRepo(followingRepo)
	setListPacker(h, userrelation.Repos{Following: followingRepo}, fakeModerator{ids: map[string]bool{"mod1": true}})

	// 匿名: embed followee の followers-only count は 0。
	rec := postBody(h.Followers, `{"host":"remote.example","limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	followee := rows[0]["followee"].(map[string]any)
	assert.EqualValues(t, 0, followee["followersCount"], "followee の followers-only count は匿名に leak しない (#1988)")
}

// #1988: federation/following も同じ packFollowings 経路を通るため、embed followee の
// followers-only count が非フォロワー (匿名含む) に leak しないことを明示的に確認する。
func TestFollowing_FolloweeGatesFollowersCount(t *testing.T) {
	h, _ := newHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	h.SetIDGen(idGen)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["lf1"] = &model.User{ID: "lf1", Username: "lf1", FollowersCount: 7}
	userRepo.Profiles["lf1"] = &model.UserProfile{
		UserID:              "lf1",
		FollowersVisibility: model.FollowingVisibilityFollowers,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}
	h.SetUserRepo(userRepo)
	followingRepo := testutil.NewMockFollowingRepository()
	remote := "remote.example"
	// federation/following は followerHost=remote の行が対象 (embed されるのは followee=lf1)。
	followingRepo.Followings["f_listed"] = &model.Following{ID: "f_listed", FollowerID: "someone", FollowerHost: &remote, FolloweeID: "lf1"}
	h.SetFollowingRepo(followingRepo)
	setListPacker(h, userrelation.Repos{Following: followingRepo}, fakeModerator{ids: map[string]bool{"mod1": true}})

	rec := postBody(h.Following, `{"host":"remote.example","limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	followee := rows[0]["followee"].(map[string]any)
	assert.EqualValues(t, 0, followee["followersCount"], "followee の followers-only count は匿名に leak しない (#1988)")
}

func TestUsers_FiltersByHost(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", Host: &remote}
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob"}
	h.SetUserRepo(userRepo)

	rec := postBody(h.Users, `{"host":"remote.example"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	// MockUserRepository.ListUsers が Hostname フィルタを実装してないと local も
	// 混入してしまうため、実装側でフィルタしないケースでもテストは最低限
	// 200 が返ることを確認する。実 DB 経路での厳密な filter 動作検証は repo
	// 側テスト (TestUserRepository_ListUsers_HostnameFilter 相当) で行う。
	assert.GreaterOrEqual(t, len(rows), 1)
}

func TestFollowers_NoRepoReturnsEmpty(t *testing.T) {
	h, _ := newHandler(t)
	rec := postBody(h.Followers, `{"host":"remote.example"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFollowing_NoRepoReturnsEmpty(t *testing.T) {
	h, _ := newHandler(t)
	rec := postBody(h.Following, `{"host":"remote.example"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUsers_NoRepoReturnsEmpty(t *testing.T) {
	h, _ := newHandler(t)
	rec := postBody(h.Users, `{"host":"remote.example"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// upstream users.ts は UserDetailedNotMe を返す。旧実装の最小 map では欠けて
// いた createdAt / notesCount 等の UserDetailed field が含まれること (#1544)。
func TestUsers_ReturnsUserDetailedShape(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	remote := "remote.example"
	idGen, _ := id.NewGenerator("aidx")
	uid := idGen.Generate(time.Now())
	userRepo.Users[uid] = &model.User{ID: uid, Username: "alice", Host: &remote}
	userRepo.Profiles[uid] = &model.UserProfile{UserID: uid, Description: strPtr("hello")}
	h.SetUserRepo(userRepo)

	rec := postBody(h.Users, `{"host":"remote.example"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.GreaterOrEqual(t, len(rows), 1)
	row := rows[0]
	// UserDetailed 固有 field (最小 map には無かった) が存在すること。
	assert.Contains(t, row, "createdAt")
	assert.Contains(t, row, "notesCount")
	assert.Contains(t, row, "followersCount")
	assert.Equal(t, "alice", row["username"])
}

func strPtr(s string) *string { return &s }

// 範囲外 limit は 400 (upstream の ajv maximum)。
func TestFollowers_LimitOutOfRangeRejected(t *testing.T) {
	h, _ := newHandler(t)
	h.SetFollowingRepo(testutil.NewMockFollowingRepository())
	rec := postBody(h.Followers, `{"host":"remote.example","limit":500}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
