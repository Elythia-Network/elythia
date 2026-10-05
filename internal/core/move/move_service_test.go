package move_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/move"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeResolver returns preset results and records which URIs were asked.
type fakeResolver struct {
	byURI map[string]*model.User
	err   error
	calls []string
	// localIDs maps a local actor URI to the user id the resolver would
	// extract. 本番の resolver は自ホストの URI をここで短絡する。
	localIDs map[string]string
}

func (f *fakeResolver) ExtractLocalUserID(uri string) string { return f.localIDs[uri] }

func (f *fakeResolver) ResolveActor(uri string) (*model.User, error) {
	f.calls = append(f.calls, uri)
	if f.err != nil {
		return nil, f.err
	}
	return f.byURI[uri], nil
}

// fakeDeliverer captures the activity body for assertions.
type fakeDeliverer struct {
	called    int
	signer    string
	body      []byte
	returnErr error
}

func (f *fakeDeliverer) DeliverToFollowers(signerUserID string, body []byte) error {
	f.called++
	f.signer = signerUserID
	f.body = body
	return f.returnErr
}

func strPtr(s string) *string { return &s }

// newService is a helper that wires all the mocks together. baseURL is fixed
// so tests can predict generated srcURI (urls.UserURI).
func newService(resolver move.Resolver, deliverer move.Deliverer) (*move.Service, *testutil.MockUserRepository, *testutil.MockFollowingRepository) {
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	svc := move.NewService(userRepo, followingRepo, urls, renderer, resolver, deliverer)
	return svc, userRepo, followingRepo
}

func TestMove_Success(t *testing.T) {
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"

	dst := &model.User{
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr(srcURI),
	}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	deliverer := &fakeDeliverer{}

	svc, userRepo, _ := newService(resolver, deliverer)
	me := &model.User{ID: "me", URI: strPtr(srcURI)}
	userRepo.Users[me.ID] = me

	require.NoError(t, svc.Move(me, dstURI))

	// In-place struct was updated so the caller sees the new state.
	assert.NotNil(t, me.MovedToURI)
	assert.Equal(t, dstURI, *me.MovedToURI)
	assert.NotNil(t, me.MovedAt)
	require.NotNil(t, me.AlsoKnownAs)
	assert.Equal(t, dstURI, *me.AlsoKnownAs)

	// UpdateUser was persisted to the repo row (not just the in-place struct).
	// これを検証しないと UpdateUser 呼び出しが消えたリグレッションを検出できない。
	persisted := userRepo.Users["me"]
	require.NotNil(t, persisted.MovedToURI)
	assert.Equal(t, dstURI, *persisted.MovedToURI)
	assert.NotNil(t, persisted.MovedAt)
	require.NotNil(t, persisted.AlsoKnownAs)
	assert.Equal(t, dstURI, *persisted.AlsoKnownAs)

	// Delivery was enqueued with a Move-shaped body.
	require.Equal(t, 1, deliverer.called)
	assert.Equal(t, "me", deliverer.signer)
	var body map[string]any
	require.NoError(t, json.Unmarshal(deliverer.body, &body))
	assert.Equal(t, "Move", body["type"])
	assert.Equal(t, srcURI, body["actor"])
	assert.Equal(t, srcURI, body["object"])
	assert.Equal(t, dstURI, body["target"])
	// To に follower collection URI が入る (一部 AP 実装が visibility 判定で参照)
	if to, ok := body["to"].([]any); ok {
		require.Len(t, to, 1)
		assert.Equal(t, srcURI+"/followers", to[0])
	} else {
		t.Fatalf("Move activity has no to addressing: %v", body)
	}
}

func TestMove_AlreadyMoved(t *testing.T) {
	svc, _, _ := newService(&fakeResolver{}, &fakeDeliverer{})
	existing := "https://other.example/users/x"
	me := &model.User{ID: "me", URI: strPtr("https://local.example/users/me"), MovedToURI: &existing}
	assert.ErrorIs(t, svc.Move(me, "https://other.example/users/new"), move.ErrAlreadyMoved)
}

func TestMove_RemoteSourceForbidden(t *testing.T) {
	svc, _, _ := newService(&fakeResolver{}, &fakeDeliverer{})
	host := "remote.example"
	me := &model.User{ID: "me", Host: &host, URI: strPtr("https://remote.example/users/me")}
	assert.ErrorIs(t, svc.Move(me, "https://other.example/users/new"), move.ErrRemoteSourceForbidden)
}

func TestMove_EmptyDestinationURI(t *testing.T) {
	svc, _, _ := newService(&fakeResolver{}, &fakeDeliverer{})
	me := &model.User{ID: "me"}
	assert.ErrorIs(t, svc.Move(me, ""), move.ErrNoSuchUser)
	assert.ErrorIs(t, svc.Move(me, "   "), move.ErrNoSuchUser)
}

func TestMove_ResolverError(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("boom")}
	svc, _, _ := newService(resolver, &fakeDeliverer{})
	me := &model.User{ID: "me", URI: strPtr("https://local.example/users/me")}
	assert.ErrorIs(t, svc.Move(me, "https://other.example/users/new"), move.ErrNoSuchUser)
}

func TestMove_ResolverReturnsNil(t *testing.T) {
	resolver := &fakeResolver{byURI: map[string]*model.User{}}
	svc, _, _ := newService(resolver, &fakeDeliverer{})
	me := &model.User{ID: "me", URI: strPtr("https://local.example/users/me")}
	assert.ErrorIs(t, svc.Move(me, "https://other.example/users/new"), move.ErrNoSuchUser)
}

func TestMove_ResolverMissing(t *testing.T) {
	svc, _, _ := newService(nil, nil)
	me := &model.User{ID: "me", URI: strPtr("https://local.example/users/me")}
	assert.ErrorIs(t, svc.Move(me, "https://other.example/users/new"), move.ErrNoSuchUser)
}

func TestMove_AlsoKnownAsMissing(t *testing.T) {
	dstURI := "https://other.example/users/new"
	dst := &model.User{ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: nil}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	svc, _, _ := newService(resolver, &fakeDeliverer{})
	me := &model.User{ID: "me", URI: strPtr("https://local.example/users/me")}
	assert.ErrorIs(t, svc.Move(me, dstURI), move.ErrDestinationForbids)
}

func TestMove_DestinationAlreadyMoved(t *testing.T) {
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"
	other := "https://yet.another/users/z"
	dst := &model.User{
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr(srcURI),
		MovedToURI:  &other,
	}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	svc, _, _ := newService(resolver, &fakeDeliverer{})
	me := &model.User{ID: "me", URI: strPtr(srcURI)}
	assert.ErrorIs(t, svc.Move(me, dstURI), move.ErrDestinationForbids)
}

func TestMove_AlsoKnownAsIncludesMultipleValues(t *testing.T) {
	// 自分の URI が csv の途中にあっても検出されることを確認する。
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"
	csv := "https://foo/users/a, " + srcURI + " , https://bar/users/b"
	dst := &model.User{ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: &csv}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	deliverer := &fakeDeliverer{}
	svc, userRepo, _ := newService(resolver, deliverer)
	me := &model.User{ID: "me", URI: strPtr(srcURI)}
	userRepo.Users[me.ID] = me

	require.NoError(t, svc.Move(me, dstURI))
	assert.Equal(t, 1, deliverer.called)
}

func TestMove_DelivererMissing_StillUpdatesDB(t *testing.T) {
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"
	dst := &model.User{ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI)}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	svc, userRepo, _ := newService(resolver, nil)
	me := &model.User{ID: "me", URI: strPtr(srcURI)}
	userRepo.Users[me.ID] = me

	require.NoError(t, svc.Move(me, dstURI))
	require.NotNil(t, me.MovedToURI)
	assert.Equal(t, dstURI, *me.MovedToURI)
}

func TestMove_AppendAlsoKnownAsDedup(t *testing.T) {
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"
	dst := &model.User{ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI)}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	svc, userRepo, _ := newService(resolver, &fakeDeliverer{})
	// me は既に dst を alsoKnownAs に入れている (2 回 Move を叩いたケース相当)
	existing := dstURI
	me := &model.User{ID: "me", URI: strPtr(srcURI), AlsoKnownAs: &existing}
	userRepo.Users[me.ID] = me

	require.NoError(t, svc.Move(me, dstURI))
	require.NotNil(t, me.AlsoKnownAs)
	// 重複は追加されない。
	assert.Equal(t, dstURI, *me.AlsoKnownAs)
}

// アカウント移行は不可逆なので、DB コミット後の配送失敗は握り潰して nil を
// 返す (そうしないとクライアントが再試行した際に ErrAlreadyMoved で詰む)。
func TestMove_DelivererErrorIsSwallowedAfterDBCommit(t *testing.T) {
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"
	dst := &model.User{ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI)}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	deliverer := &fakeDeliverer{returnErr: errors.New("enqueue boom")}
	svc, userRepo, _ := newService(resolver, deliverer)
	me := &model.User{ID: "me", URI: strPtr(srcURI)}
	userRepo.Users[me.ID] = me

	require.NoError(t, svc.Move(me, dstURI))
	assert.Equal(t, 1, deliverer.called, "配送は実行されているべき")
	// DB は確実に更新されている。
	persisted := userRepo.Users["me"]
	require.NotNil(t, persisted.MovedToURI)
	assert.Equal(t, dstURI, *persisted.MovedToURI)
}

func TestMove_NilSource(t *testing.T) {
	svc, _, _ := newService(&fakeResolver{}, &fakeDeliverer{})
	assert.ErrorIs(t, svc.Move(nil, "https://other.example/users/new"), move.ErrNoSuchUser)
}

// UpdateUser がエラーを返した場合、エラーが伝搬して delivery は発生しない。
func TestMove_UserRepoUpdateError(t *testing.T) {
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"
	dst := &model.User{ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI)}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	deliverer := &fakeDeliverer{}

	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	renderer := activitypub.NewRenderer(urls)
	// UpdateUser がエラーを返すラッパー。
	failRepo := &failingUserRepo{MockUserRepository: userRepo}
	svc := move.NewService(failRepo, followingRepo, urls, renderer, resolver, deliverer)
	me := &model.User{ID: "me", URI: strPtr(srcURI)}

	err := svc.Move(me, dstURI)
	require.Error(t, err)
	assert.Equal(t, 0, deliverer.called, "update 失敗時は deliver されない")
}

type failingUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUserRepo) UpdateUser(string, map[string]any) error {
	return errors.New("update boom")
}

// 解決後の dst.URI が空なら dstURI 引数をそのまま canonical として使うこと。
func TestMove_FallbackToInputURIWhenResolverURIEmpty(t *testing.T) {
	srcURI := "https://local.example/users/me"
	dstURI := "https://other.example/users/new"
	dst := &model.User{ID: "dst", URI: nil, AlsoKnownAs: strPtr(srcURI)}
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: dst}}
	svc, userRepo, _ := newService(resolver, &fakeDeliverer{})
	me := &model.User{ID: "me", URI: strPtr(srcURI)}
	userRepo.Users[me.ID] = me

	require.NoError(t, svc.Move(me, dstURI))
	require.NotNil(t, me.MovedToURI)
	assert.Equal(t, dstURI, *me.MovedToURI)
}

// fakeFollowQueue captures the follow payloads scheduled for follower migration.
type fakeFollowQueue struct {
	calls     int
	payloads  []queue.FollowPayload
	returnErr error

	// 遅延 unfollow (#2420) の記録。
	unfollowCalls    int
	unfollowPayloads []queue.UnfollowPayload
	unfollowDelay    time.Duration
	unfollowErr      error
}

func (f *fakeFollowQueue) EnqueueFollowBulk(payloads []queue.FollowPayload) error {
	f.calls++
	f.payloads = append(f.payloads, payloads...)
	return f.returnErr
}

func (f *fakeFollowQueue) EnqueueUnfollowBulkDelayed(payloads []queue.UnfollowPayload, delay time.Duration) error {
	f.unfollowCalls++
	f.unfollowPayloads = append(f.unfollowPayloads, payloads...)
	f.unfollowDelay = delay
	return f.unfollowErr
}

// putLocalFollowing registers a Following row whose follower is local
// (followerHost nil)。リモートフォロワーは移行対象外なので host 有りと区別する。
// MockFollowingRepository.Followings は id をキーにした map なので、衝突しない
// 合成 id を振る。
func putLocalFollowing(repo *testutil.MockFollowingRepository, followerID, followeeID string) {
	id := followerID + "->" + followeeID
	repo.Followings[id] = &model.Following{ID: id, FollowerID: followerID, FolloweeID: followeeID}
}

func putRemoteFollowing(repo *testutil.MockFollowingRepository, followerID, followeeID, host string) {
	id := followerID + "->" + followeeID
	repo.Followings[id] = &model.Following{
		ID: id, FollowerID: followerID, FolloweeID: followeeID, FollowerHost: &host,
	}
}

// 移行するとローカルフォロワーが移行先を follow し直す (#2418)。
// upstream postMoveProcess の後半にあたる。
func TestMove_MigratesLocalFollowers(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, followingRepo := newService(resolver, &fakeDeliverer{})
	fq := &fakeFollowQueue{}
	svc.SetFollowQueue(fq)

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	userRepo.Users["src"] = src
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}
	putLocalFollowing(followingRepo, "localA", "src")
	putLocalFollowing(followingRepo, "localB", "src")
	putRemoteFollowing(followingRepo, "remoteC", "src", "remote.example")
	putLocalFollowing(followingRepo, "localD", "someone-else") // 別ユーザーのフォロワーは無関係

	require.NoError(t, svc.Move(src, dstURI))

	require.Equal(t, 1, fq.calls, "follow jobs must be scheduled in one bulk call")
	got := make([]string, 0, len(fq.payloads))
	for _, p := range fq.payloads {
		assert.Equal(t, "dst", p.FolloweeID, "followers must be migrated onto the destination")
		got = append(got, p.FollowerID)
	}
	assert.ElementsMatch(t, []string{"localA", "localB"}, got,
		"only local followers of the source are migrated")
}

// proxy account はリスト購読のため機械的に follow しているだけなので、移行先へ
// follow させない (upstream の followerId: Not(proxy.id))。
func TestMove_ExcludesProxyAccountFromMigration(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, followingRepo := newService(resolver, &fakeDeliverer{})
	fq := &fakeFollowQueue{}
	svc.SetFollowQueue(fq)
	svc.SetProxyAccountResolver(func() (string, bool) { return "proxy", true })

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	userRepo.Users["src"] = src
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}
	putLocalFollowing(followingRepo, "proxy", "src")
	putLocalFollowing(followingRepo, "localA", "src")

	require.NoError(t, svc.Move(src, dstURI))

	require.Len(t, fq.payloads, 1)
	assert.Equal(t, "localA", fq.payloads[0].FollowerID, "proxy account must be excluded")
}

// upstream は「unfollow せず following count だけ落とす」。旧アカウントがまだ
// 機能しうるので関係を切らない、という意図的な設計 (#2418)。
func TestMove_AdjustsCountsWithoutUnfollowing(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, followingRepo := newService(resolver, &fakeDeliverer{})
	svc.SetFollowQueue(&fakeFollowQueue{})

	src := &model.User{
		ID: "src", Username: "src", URI: strPtr("https://local.example/users/src"),
		FollowersCount: 2, FollowingCount: 1,
	}
	userRepo.Users["src"] = src
	userRepo.Users["localA"] = &model.User{ID: "localA", FollowingCount: 5}
	userRepo.Users["followee1"] = &model.User{ID: "followee1", FollowersCount: 9}
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}
	putLocalFollowing(followingRepo, "localA", "src")
	putLocalFollowing(followingRepo, "src", "followee1") // src 自身のフォロー

	require.NoError(t, svc.Move(src, dstURI))

	assert.Equal(t, 0, userRepo.Users["src"].FollowersCount, "old account counters are zeroed")
	assert.Equal(t, 0, userRepo.Users["src"].FollowingCount)
	assert.Equal(t, 4, userRepo.Users["localA"].FollowingCount,
		"local follower's followingCount is decremented by 1")
	assert.Equal(t, 8, userRepo.Users["followee1"].FollowersCount,
		"followee of the old account loses one follower count")

	// フォロー行そのものは残る。ここが upstream の要点で、素直に unfollow へ
	// 置き換えると挙動が変わる。
	assert.Len(t, followingRepo.Followings, 2, "following rows must NOT be removed")
}

// フォロワーが 1 人も居ない移行ではカウント調整も enqueue も行わない。
func TestMove_NoFollowersSkipsMigration(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, _ := newService(resolver, &fakeDeliverer{})
	fq := &fakeFollowQueue{}
	svc.SetFollowQueue(fq)

	src := &model.User{
		ID: "src", Username: "src", URI: strPtr("https://local.example/users/src"),
		FollowersCount: 3,
	}
	userRepo.Users["src"] = src
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}

	require.NoError(t, svc.Move(src, dstURI))

	assert.Zero(t, fq.calls, "no follow jobs when there are no local followers")
	assert.Equal(t, 3, userRepo.Users["src"].FollowersCount,
		"counters are left alone when there is nothing to migrate")
}

// queue の enqueue が失敗しても Move 自体は成功させる。DB は既にコミット済みで、
// エラーを返して再試行されると次回 ErrAlreadyMoved で詰むため。
func TestMove_FollowQueueFailureDoesNotFailMove(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, followingRepo := newService(resolver, &fakeDeliverer{})
	svc.SetFollowQueue(&fakeFollowQueue{returnErr: errors.New("boom")})

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	userRepo.Users["src"] = src
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}
	putLocalFollowing(followingRepo, "localA", "src")

	assert.NoError(t, svc.Move(src, dstURI))
	require.NotNil(t, src.MovedToURI, "the move itself still succeeded")
}

// countFailUserRepo はカウント更新だけ失敗する。PostMoveProcess は
// best-effort なので、これらが失敗しても移行は完了しフォロワー移行の
// enqueue まで進む。
type countFailUserRepo struct {
	*testutil.MockUserRepository
	failUpdate bool
}

func (f *countFailUserRepo) UpdateUser(userID string, fields map[string]any) error {
	if f.failUpdate {
		if _, ok := fields["followersCount"]; ok {
			return errors.New("counter reset boom")
		}
	}
	return f.MockUserRepository.UpdateUser(userID, fields)
}

func (f *countFailUserRepo) IncrementFollowingCount(string, int) error {
	return errors.New("decrement following boom")
}

func (f *countFailUserRepo) IncrementFollowersCount(string, int) error {
	return errors.New("decrement followers boom")
}

// listFailFollowingRepo は ListFolloweeIDs だけ失敗する。
type listFailFollowingRepo struct {
	*testutil.MockFollowingRepository
	failLocalFollowers bool
}

func (f *listFailFollowingRepo) ListLocalFollowerIDs(followeeID string) ([]string, error) {
	if f.failLocalFollowers {
		return nil, errors.New("list local followers boom")
	}
	return f.MockFollowingRepository.ListLocalFollowerIDs(followeeID)
}

func (f *listFailFollowingRepo) ListFolloweeIDs(string) ([]string, error) {
	return nil, errors.New("list followees boom")
}

// moveWithRepos wires a Service with the supplied repos and runs a successful move.
func moveWithRepos(t *testing.T, userRepo repository.UserRepository, followingRepo repository.FollowingRepository, fq move.FollowEnqueuer) error {
	t.Helper()
	dstURI := "https://remote.example/users/dst"
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: {
		ID:          "dst",
		URI:         strPtr(dstURI),
		AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}}
	urls := activitypub.NewURLBuilder("https://local.example")
	svc := move.NewService(userRepo, followingRepo, urls, activitypub.NewRenderer(urls), resolver, &fakeDeliverer{})
	if fq != nil {
		svc.SetFollowQueue(fq)
	}
	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	return svc.Move(src, dstURI)
}

// フォロワー一覧の取得に失敗しても移行そのものは成功させる (best-effort)。
// DB は既にコミット済みで、再試行させると ErrAlreadyMoved で詰むため。
func TestMove_ListFollowersFailureDoesNotFailMove(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["src"] = &model.User{ID: "src"}
	followingRepo := &listFailFollowingRepo{
		MockFollowingRepository: testutil.NewMockFollowingRepository(),
		failLocalFollowers:      true,
	}
	fq := &fakeFollowQueue{}

	require.NoError(t, moveWithRepos(t, userRepo, followingRepo, fq))
	assert.Zero(t, fq.calls, "no follow jobs when the follower list could not be read")
}

// カウント更新が軒並み失敗しても移行は完了し、フォロワー移行の enqueue まで進む。
func TestMove_CountAdjustmentFailuresAreBestEffort(t *testing.T) {
	mock := testutil.NewMockUserRepository()
	mock.Users["src"] = &model.User{ID: "src"}
	userRepo := &countFailUserRepo{MockUserRepository: mock, failUpdate: true}
	inner := testutil.NewMockFollowingRepository()
	putLocalFollowing(inner, "localA", "src")
	followingRepo := &listFailFollowingRepo{MockFollowingRepository: inner}
	fq := &fakeFollowQueue{}

	require.NoError(t, moveWithRepos(t, userRepo, followingRepo, fq))
	require.Len(t, fq.payloads, 1, "follower migration proceeds despite counter failures")
	assert.Equal(t, "localA", fq.payloads[0].FollowerID)
}

// queue 未配線でも移行は成立させる。フォロワー引き継ぎだけが行われない。
func TestMove_WithoutFollowQueueStillMoves(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["src"] = &model.User{ID: "src", FollowersCount: 1}
	followingRepo := testutil.NewMockFollowingRepository()
	putLocalFollowing(followingRepo, "localA", "src")

	require.NoError(t, moveWithRepos(t, userRepo, followingRepo, nil))
	// queue が無くてもカウント調整は済ませる (upstream の順序と同じ)。
	assert.Equal(t, 0, userRepo.Users["src"].FollowersCount)
}

// PostMoveProcess を直接呼んだときの nil ガード。
func TestPostMoveProcess_NilArgs(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://local.example")
	svc := move.NewService(userRepo, followingRepo, urls, activitypub.NewRenderer(urls), &fakeResolver{}, &fakeDeliverer{})

	assert.NotPanics(t, func() { svc.PostMoveProcess(nil, &model.User{ID: "dst"}) })
	assert.NotPanics(t, func() { svc.PostMoveProcess(&model.User{ID: "src"}, nil) })
}

// fakeBlockQueue captures block payloads scheduled for carry-over.
type fakeBlockQueue struct {
	payloads []queue.BlockPayload
}

func (f *fakeBlockQueue) EnqueueBlockBulk(payloads []queue.BlockPayload) error {
	f.payloads = append(f.payloads, payloads...)
	return nil
}

// fakeRoleAssigner records Assign calls and serves preset roles.
type fakeRoleAssigner struct {
	assigns   []*model.RoleAssignment
	roles     map[string]*model.Role
	assigned  []string // "userID:roleID"
	alreadyOn map[string]bool
}

func (f *fakeRoleAssigner) GetUserAssigns(string) ([]*model.RoleAssignment, error) {
	return f.assigns, nil
}

func (f *fakeRoleAssigner) FindRole(roleID string) (*model.Role, error) {
	r, ok := f.roles[roleID]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}

func (f *fakeRoleAssigner) Assign(userID, roleID string, _ *time.Time) error {
	if f.alreadyOn[roleID] {
		return errAlreadyAssignedForTest
	}
	f.assigned = append(f.assigned, userID+":"+roleID)
	return nil
}

func (f *fakeRoleAssigner) IsAlreadyAssigned(err error) bool {
	return errors.Is(err, errAlreadyAssignedForTest)
}

var errAlreadyAssignedForTest = errors.New("already assigned")

// fakeAntennaMover records the OnMoveAccount call.
type fakeAntennaMover struct {
	src, dst *model.User
}

func (f *fakeAntennaMover) OnMoveAccount(src, dst *model.User) { f.src, f.dst = src, dst }

// carryOverFixture wires a Service with every carry-over dependency and runs a
// successful move, returning the fakes for assertions.
type carryOverFixture struct {
	blockingRepo *testutil.MockBlockingRepository
	mutingRepo   *testutil.MockMutingRepository
	userListRepo *testutil.MockUserListRepository
	blockQueue   *fakeBlockQueue
	roleAssigner *fakeRoleAssigner
	antennaMover *fakeAntennaMover
}

func runMoveWithCarryOver(t *testing.T, setup func(*carryOverFixture)) *carryOverFixture {
	t.Helper()
	fx := &carryOverFixture{
		blockingRepo: testutil.NewMockBlockingRepository(),
		mutingRepo:   testutil.NewMockMutingRepository(),
		userListRepo: testutil.NewMockUserListRepository(),
		blockQueue:   &fakeBlockQueue{},
		roleAssigner: &fakeRoleAssigner{roles: map[string]*model.Role{}, alreadyOn: map[string]bool{}},
		antennaMover: &fakeAntennaMover{},
	}
	if setup != nil {
		setup(fx)
	}
	dstURI := "https://remote.example/users/dst"
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: {
		ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["src"] = &model.User{ID: "src", Username: "src"}
	urls := activitypub.NewURLBuilder("https://local.example")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := move.NewService(userRepo, testutil.NewMockFollowingRepository(), urls,
		activitypub.NewRenderer(urls), resolver, &fakeDeliverer{})
	svc.SetCarryOverRepos(fx.blockingRepo, fx.mutingRepo, fx.userListRepo, idGen)
	svc.SetBlockQueue(fx.blockQueue)
	svc.SetRoleAssigner(fx.roleAssigner)
	svc.SetAntennaMover(fx.antennaMover)

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	require.NoError(t, svc.Move(src, dstURI))
	return fx
}

// src をブロックしている人に dst もブロックさせる。既に dst をブロック済みの
// blocker はスキップし、旧アカウントの unblock はしない (#2419)。
func TestMove_CopiesBlocking(t *testing.T) {
	fx := runMoveWithCarryOver(t, func(fx *carryOverFixture) {
		fx.blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "alice", BlockeeID: "src"}
		fx.blockingRepo.Blockings["b2"] = &model.Blocking{ID: "b2", BlockerID: "bob", BlockeeID: "src"}
		// bob は既に dst をブロック済み → 重複させない
		fx.blockingRepo.Blockings["b3"] = &model.Blocking{ID: "b3", BlockerID: "bob", BlockeeID: "dst"}
	})

	require.Len(t, fx.blockQueue.payloads, 1)
	assert.Equal(t, "alice", fx.blockQueue.payloads[0].BlockerID)
	assert.Equal(t, "dst", fx.blockQueue.payloads[0].BlockeeID)
	// 旧アカウントへのブロックは残る。
	assert.Len(t, fx.blockingRepo.Blockings, 3, "blocks against the old account are kept")
}

// 有効なミュートを dst にも張る。expiresAt はそのまま引き継ぐ。dst を無期限
// ミュート済みの muter はスキップする。
func TestMove_CopiesMutings(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	past := time.Now().Add(-1 * time.Hour)
	fx := runMoveWithCarryOver(t, func(fx *carryOverFixture) {
		fx.mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "alice", MuteeID: "src"}
		fx.mutingRepo.Mutings["m2"] = &model.Muting{ID: "m2", MuterID: "bob", MuteeID: "src", ExpiresAt: &future}
		// 期限切れは対象外
		fx.mutingRepo.Mutings["m3"] = &model.Muting{ID: "m3", MuterID: "carol", MuteeID: "src", ExpiresAt: &past}
		// dave は既に dst を無期限ミュート済み → 重複させない
		fx.mutingRepo.Mutings["m4"] = &model.Muting{ID: "m4", MuterID: "dave", MuteeID: "src"}
		fx.mutingRepo.Mutings["m5"] = &model.Muting{ID: "m5", MuterID: "dave", MuteeID: "dst"}
	})

	got := map[string]*time.Time{}
	for _, m := range fx.mutingRepo.Mutings {
		if m.MuteeID == "dst" && m.MuterID != "dave" {
			got[m.MuterID] = m.ExpiresAt
		}
	}
	require.Len(t, got, 2, "only alice and bob get a new mute (carol expired, dave already muted)")
	assert.Nil(t, got["alice"], "indefinite mute stays indefinite")
	require.NotNil(t, got["bob"])
	assert.WithinDuration(t, future, *got["bob"], time.Second, "expiresAt is carried over as-is")
}

// preserveAssignmentOnMoveAccount が true のロールだけ引き継ぐ。
func TestMove_CopiesOnlyPreservedRoles(t *testing.T) {
	fx := runMoveWithCarryOver(t, func(fx *carryOverFixture) {
		fx.roleAssigner.assigns = []*model.RoleAssignment{
			{RoleID: "keep"}, {RoleID: "drop"}, {RoleID: "gone"}, {RoleID: "dup"},
		}
		fx.roleAssigner.roles["keep"] = &model.Role{ID: "keep", PreserveAssignmentOnMoveAccount: true}
		fx.roleAssigner.roles["drop"] = &model.Role{ID: "drop"}
		fx.roleAssigner.roles["dup"] = &model.Role{ID: "dup", PreserveAssignmentOnMoveAccount: true}
		// 既に割り当て済みならエラーを握り潰して継続する
		fx.roleAssigner.alreadyOn["dup"] = true
		// "gone" は roles に無い = 削除済みロール → skip
	})

	assert.Equal(t, []string{"dst:keep"}, fx.roleAssigner.assigned)
}

// dst を「src が入っていたリスト」に追加する。旧アカウントはリストから外さない。
func TestMove_UpdatesLists(t *testing.T) {
	owner := "owner1"
	fx := runMoveWithCarryOver(t, func(fx *carryOverFixture) {
		fx.userListRepo.Members = []*model.UserListMembership{
			{ID: "mem1", UserID: "src", UserListID: "listA", UserListUserID: &owner},
			{ID: "mem2", UserID: "src", UserListID: "listB", UserListUserID: &owner},
			// dst は既に listB に入っている → 重複させない
			{ID: "mem3", UserID: "dst", UserListID: "listB", UserListUserID: &owner},
		}
	})

	var addedLists []string
	srcStill := 0
	for _, m := range fx.userListRepo.Members {
		if m.UserID == "dst" && m.ID != "mem3" {
			addedLists = append(addedLists, m.UserListID)
		}
		if m.UserID == "src" {
			srcStill++
		}
	}
	assert.Equal(t, []string{"listA"}, addedLists, "only listA is new for dst")
	assert.Equal(t, 2, srcStill, "the old account is NOT removed from lists")
}

// antenna 側へ移行を通知する。
func TestMove_NotifiesAntennaMover(t *testing.T) {
	fx := runMoveWithCarryOver(t, nil)
	require.NotNil(t, fx.antennaMover.src)
	require.NotNil(t, fx.antennaMover.dst)
	assert.Equal(t, "src", fx.antennaMover.src.ID)
	assert.Equal(t, "dst", fx.antennaMover.dst.ID)
}

// carry-over の依存が 1 つも配線されていなくても移行は成立する。
func TestMove_CarryOverIsOptional(t *testing.T) {
	dstURI := "https://remote.example/users/dst"
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: {
		ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["src"] = &model.User{ID: "src"}
	urls := activitypub.NewURLBuilder("https://local.example")
	svc := move.NewService(userRepo, testutil.NewMockFollowingRepository(), urls,
		activitypub.NewRenderer(urls), resolver, &fakeDeliverer{})

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	assert.NotPanics(t, func() { require.NoError(t, svc.Move(src, dstURI)) })
}

// --- carry-over のエラーパス -------------------------------------------------
//
// carry-over は best-effort で、個々の失敗は log に落として次へ進む。どれが
// 落ちても Move 自体は成功し、他系統の引き継ぎは継続する。

type failingBlockingRepo struct {
	*testutil.MockBlockingRepository
	failOn string // "src" なら 1 回目、"dst" なら 2 回目で失敗
	calls  int
}

func (f *failingBlockingRepo) ListBlockerIDs(blockeeID string) ([]string, error) {
	f.calls++
	if (f.failOn == "src" && f.calls == 1) || (f.failOn == "dst" && f.calls == 2) {
		return nil, errors.New("list blockers boom")
	}
	return f.MockBlockingRepository.ListBlockerIDs(blockeeID)
}

type failingMutingRepo struct {
	*testutil.MockMutingRepository
	failOn     string // "src" / "dst"
	failCreate bool
	calls      int
}

func (f *failingMutingRepo) ListByMutee(muteeID string) ([]*model.Muting, error) {
	f.calls++
	if (f.failOn == "src" && f.calls == 1) || (f.failOn == "dst" && f.calls == 2) {
		return nil, errors.New("list mutings boom")
	}
	return f.MockMutingRepository.ListByMutee(muteeID)
}

func (f *failingMutingRepo) Create(m *model.Muting) error {
	if f.failCreate {
		return errors.New("create muting boom")
	}
	return f.MockMutingRepository.Create(m)
}

type failingUserListRepoMove struct {
	*testutil.MockUserListRepository
	failOn  string // "src" / "dst"
	failAdd bool
	calls   int
}

func (f *failingUserListRepoMove) ListMembershipsByUser(userID string) ([]*model.UserListMembership, error) {
	f.calls++
	if (f.failOn == "src" && f.calls == 1) || (f.failOn == "dst" && f.calls == 2) {
		return nil, errors.New("list memberships boom")
	}
	return f.MockUserListRepository.ListMembershipsByUser(userID)
}

func (f *failingUserListRepoMove) AddMember(m *model.UserListMembership) error {
	if f.failAdd {
		return errors.New("add member boom")
	}
	return f.MockUserListRepository.AddMember(m)
}

type failingRoleAssigner struct {
	listErr   bool
	assignErr bool
}

func (f *failingRoleAssigner) GetUserAssigns(string) ([]*model.RoleAssignment, error) {
	if f.listErr {
		return nil, errors.New("list assigns boom")
	}
	return []*model.RoleAssignment{{RoleID: "keep"}}, nil
}

func (f *failingRoleAssigner) FindRole(string) (*model.Role, error) {
	return &model.Role{ID: "keep", PreserveAssignmentOnMoveAccount: true}, nil
}

func (f *failingRoleAssigner) Assign(string, string, *time.Time) error {
	if f.assignErr {
		return errors.New("assign boom")
	}
	return nil
}

func (f *failingRoleAssigner) IsAlreadyAssigned(error) bool { return false }

type failingBlockQueue struct{}

func (failingBlockQueue) EnqueueBlockBulk([]queue.BlockPayload) error {
	return errors.New("enqueue block boom")
}

// moveWithCarryOverDeps runs a successful move with the supplied carry-over deps.
func moveWithCarryOverDeps(
	t *testing.T,
	blockingRepo repository.BlockingRepository,
	mutingRepo repository.MutingRepository,
	userListRepo repository.UserListRepository,
	blockQueue move.BlockEnqueuer,
	roleAssigner move.RoleAssigner,
) {
	t.Helper()
	dstURI := "https://remote.example/users/dst"
	resolver := &fakeResolver{byURI: map[string]*model.User{dstURI: {
		ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["src"] = &model.User{ID: "src"}
	urls := activitypub.NewURLBuilder("https://local.example")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := move.NewService(userRepo, testutil.NewMockFollowingRepository(), urls,
		activitypub.NewRenderer(urls), resolver, &fakeDeliverer{})
	svc.SetCarryOverRepos(blockingRepo, mutingRepo, userListRepo, idGen)
	if blockQueue != nil {
		svc.SetBlockQueue(blockQueue)
	}
	if roleAssigner != nil {
		svc.SetRoleAssigner(roleAssigner)
	}
	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	require.NoError(t, svc.Move(src, dstURI))
}

func TestMove_CarryOverFailuresAreBestEffort(t *testing.T) {
	seedBlocking := func() *testutil.MockBlockingRepository {
		r := testutil.NewMockBlockingRepository()
		r.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "alice", BlockeeID: "src"}
		return r
	}
	seedMuting := func() *testutil.MockMutingRepository {
		r := testutil.NewMockMutingRepository()
		r.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "alice", MuteeID: "src"}
		return r
	}
	seedList := func() *testutil.MockUserListRepository {
		r := testutil.NewMockUserListRepository()
		r.Members = []*model.UserListMembership{{ID: "mem1", UserID: "src", UserListID: "listA"}}
		return r
	}

	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"blocker一覧(src)の取得失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t,
				&failingBlockingRepo{MockBlockingRepository: seedBlocking(), failOn: "src"},
				seedMuting(), seedList(), &fakeBlockQueue{}, nil)
		}},
		{"blocker一覧(dst)の取得失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t,
				&failingBlockingRepo{MockBlockingRepository: seedBlocking(), failOn: "dst"},
				seedMuting(), seedList(), &fakeBlockQueue{}, nil)
		}},
		{"blockのenqueue失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(), seedMuting(), seedList(), failingBlockQueue{}, nil)
		}},
		{"mute一覧(src)の取得失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(),
				&failingMutingRepo{MockMutingRepository: seedMuting(), failOn: "src"},
				seedList(), &fakeBlockQueue{}, nil)
		}},
		{"mute一覧(dst)の取得失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(),
				&failingMutingRepo{MockMutingRepository: seedMuting(), failOn: "dst"},
				seedList(), &fakeBlockQueue{}, nil)
		}},
		{"muteの作成失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(),
				&failingMutingRepo{MockMutingRepository: seedMuting(), failCreate: true},
				seedList(), &fakeBlockQueue{}, nil)
		}},
		{"list一覧(src)の取得失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(), seedMuting(),
				&failingUserListRepoMove{MockUserListRepository: seedList(), failOn: "src"},
				&fakeBlockQueue{}, nil)
		}},
		{"list一覧(dst)の取得失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(), seedMuting(),
				&failingUserListRepoMove{MockUserListRepository: seedList(), failOn: "dst"},
				&fakeBlockQueue{}, nil)
		}},
		{"listへの追加失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(), seedMuting(),
				&failingUserListRepoMove{MockUserListRepository: seedList(), failAdd: true},
				&fakeBlockQueue{}, nil)
		}},
		{"role割り当て一覧の取得失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(), seedMuting(), seedList(),
				&fakeBlockQueue{}, &failingRoleAssigner{listErr: true})
		}},
		{"roleの割り当て失敗", func(t *testing.T) {
			moveWithCarryOverDeps(t, seedBlocking(), seedMuting(), seedList(),
				&fakeBlockQueue{}, &failingRoleAssigner{assignErr: true})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Move が error を返さないことが本体。require.NoError は helper 側。
			assert.NotPanics(t, func() { tc.run(t) })
		})
	}
}

// 引き継ぐものが無い場合は早期 return する (空ケースの分岐)。
func TestMove_CarryOverWithNothingToCopy(t *testing.T) {
	fx := runMoveWithCarryOver(t, nil)
	assert.Empty(t, fx.blockQueue.payloads)
	assert.Empty(t, fx.roleAssigner.assigned)
}

// 移行直後の import 上限緩和の判定 (#2415)。
//
// **相互確認が security boundary。** alsoKnownAs は自己申告なので、移行元側が
// 自分を指し返していることまで確かめないと、任意の actor を並べるだけで緩和された
// 上限を得られる。
func TestHasRecentConfirmedMoveIn(t *testing.T) {
	const dstURI = "https://local.example/users/dstUser"
	const srcURI = "https://remote.example/users/oldMe"
	recent := time.Now().Add(-30 * time.Minute)
	old := time.Now().Add(-3 * time.Hour)

	cases := []struct {
		name string
		// 移行元の行 (nil なら DB に居ない)
		src  *model.User
		aka  string // dst.alsoKnownAs
		want bool
	}{
		{
			name: "相互確認が取れて2時間以内なら緩和する",
			src:  &model.User{ID: "oldMe", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI), MovedAt: &recent},
			aka:  srcURI,
			want: true,
		},
		{
			name: "移行元が自分を指し返していなければ緩和しない",
			src:  &model.User{ID: "oldMe", URI: strPtr(srcURI), MovedToURI: strPtr("https://elsewhere.example/users/x"), MovedAt: &recent},
			aka:  srcURI,
			want: false,
		},
		{
			name: "移行元が移行していなければ緩和しない",
			src:  &model.User{ID: "oldMe", URI: strPtr(srcURI), MovedAt: &recent},
			aka:  srcURI,
			want: false,
		},
		{
			name: "2時間を過ぎていれば緩和しない",
			src:  &model.User{ID: "oldMe", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI), MovedAt: &old},
			aka:  srcURI,
			want: false,
		},
		{
			name: "movedAt が無ければ緩和しない",
			src:  &model.User{ID: "oldMe", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI)},
			aka:  srcURI,
			want: false,
		},
		{
			name: "このサーバーが知らない移行元は無視する",
			src:  nil,
			aka:  srcURI,
			want: false,
		},
		{
			name: "alsoKnownAs が空なら緩和しない",
			src:  &model.User{ID: "oldMe", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI), MovedAt: &recent},
			aka:  "",
			want: false,
		},
		{
			name: "自分自身を alsoKnownAs に入れても緩和しない",
			src:  nil,
			aka:  dstURI,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			if tc.src != nil {
				userRepo.Users["oldMe"] = tc.src
			}
			urls := activitypub.NewURLBuilder("https://local.example")
			svc := move.NewService(userRepo, testutil.NewMockFollowingRepository(), urls,
				activitypub.NewRenderer(urls), &fakeResolver{}, &fakeDeliverer{})

			dst := &model.User{ID: "dstUser", Username: "me"}
			if tc.aka != "" {
				dst.AlsoKnownAs = strPtr(tc.aka)
			}
			assert.Equal(t, tc.want, svc.HasRecentConfirmedMoveIn(dst))
		})
	}
}

// alsoKnownAs は csv。複数並んでいても、条件を満たすものが 1 件あれば緩和する。
func TestHasRecentConfirmedMoveIn_MultipleAlsoKnownAs(t *testing.T) {
	const dstURI = "https://local.example/users/dstUser"
	recent := time.Now().Add(-10 * time.Minute)

	userRepo := testutil.NewMockUserRepository()
	// 1 件目は無関係、2 件目が相互確認の取れる移行元。
	userRepo.Users["stranger"] = &model.User{
		ID: "stranger", URI: strPtr("https://a.example/users/stranger"),
	}
	userRepo.Users["oldMe"] = &model.User{
		ID: "oldMe", URI: strPtr("https://b.example/users/oldMe"),
		MovedToURI: strPtr(dstURI), MovedAt: &recent,
	}
	urls := activitypub.NewURLBuilder("https://local.example")
	svc := move.NewService(userRepo, testutil.NewMockFollowingRepository(), urls,
		activitypub.NewRenderer(urls), &fakeResolver{}, &fakeDeliverer{})

	dst := &model.User{
		ID: "dstUser", Username: "me",
		AlsoKnownAs: strPtr("https://a.example/users/stranger, https://b.example/users/oldMe"),
	}
	assert.True(t, svc.HasRecentConfirmedMoveIn(dst))
}

// ローカル同士の移行でも、移行元を id で引いて相互確認できる。
func TestHasRecentConfirmedMoveIn_LocalSource(t *testing.T) {
	urls := activitypub.NewURLBuilder("https://local.example")
	dstURI := urls.UserURI("dstUser")
	srcURI := urls.UserURI("oldLocal")
	recent := time.Now().Add(-5 * time.Minute)

	userRepo := testutil.NewMockUserRepository()
	// ローカルユーザーは uri 列を持たない。
	userRepo.Users["oldLocal"] = &model.User{
		ID: "oldLocal", Username: "old", MovedToURI: strPtr(dstURI), MovedAt: &recent,
	}
	svc := move.NewService(userRepo, testutil.NewMockFollowingRepository(), urls,
		activitypub.NewRenderer(urls), &fakeResolver{}, &fakeDeliverer{})

	dst := &model.User{ID: "dstUser", Username: "me", AlsoKnownAs: strPtr(srcURI)}
	assert.True(t, svc.HasRecentConfirmedMoveIn(dst))
}

// nil / 未配線で panic しない。
func TestHasRecentConfirmedMoveIn_NilSafe(t *testing.T) {
	urls := activitypub.NewURLBuilder("https://local.example")
	svc := move.NewService(testutil.NewMockUserRepository(), testutil.NewMockFollowingRepository(),
		urls, activitypub.NewRenderer(urls), &fakeResolver{}, &fakeDeliverer{})
	assert.False(t, svc.HasRecentConfirmedMoveIn(nil))
}

// 移行すると旧アカウント自身のフォローが 24 時間後の unfollow として積まれる (#2420)。
//
// `adjustFollowingCounts` はフォロー先の followersCount を先に減らす一方でフォロー行を
// 残すため、最終的に行も消さないと行数とカウンタが恒久的にずれる。
func TestMove_SchedulesDelayedUnfollowOfOwnFollows(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, followingRepo := newService(resolver, &fakeDeliverer{})
	fq := &fakeFollowQueue{}
	svc.SetFollowQueue(fq)

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	userRepo.Users["src"] = src
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}
	// src がフォローしている相手 2 人 + src をフォローしている人 1 人。
	putLocalFollowing(followingRepo, "src", "followee1")
	putLocalFollowing(followingRepo, "src", "followee2")
	putLocalFollowing(followingRepo, "localA", "src")

	require.NoError(t, svc.Move(src, dstURI))

	require.Equal(t, 1, fq.unfollowCalls, "遅延 unfollow は 1 回の bulk で積む")
	assert.Equal(t, 24*time.Hour, fq.unfollowDelay,
		"upstream と同じ 24 時間。即座に消すと移行直後に Undo(Follow) が殺到する")

	got := make([]string, 0, len(fq.unfollowPayloads))
	for _, p := range fq.unfollowPayloads {
		assert.Equal(t, "src", p.FollowerID, "解除するのは旧アカウント自身のフォローだけ")
		got = append(got, p.FolloweeID)
	}
	assert.ElementsMatch(t, []string{"followee1", "followee2"}, got,
		"src をフォローしている人 (localA) は対象外")
}

// 旧アカウントが誰もフォローしていなければ何も積まない。
func TestMove_NoOutgoingFollowsSchedulesNothing(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, followingRepo := newService(resolver, &fakeDeliverer{})
	fq := &fakeFollowQueue{}
	svc.SetFollowQueue(fq)

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	userRepo.Users["src"] = src
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}
	// src はフォローされているだけ。
	putLocalFollowing(followingRepo, "localA", "src")

	require.NoError(t, svc.Move(src, dstURI))
	assert.Zero(t, fq.unfollowCalls)
}

// 遅延 unfollow の enqueue に失敗しても移行は成功させる (best-effort)。
func TestMove_DelayedUnfollowFailureDoesNotFailMove(t *testing.T) {
	resolver := &fakeResolver{}
	svc, userRepo, followingRepo := newService(resolver, &fakeDeliverer{})
	svc.SetFollowQueue(&fakeFollowQueue{unfollowErr: errors.New("enqueue boom")})

	src := &model.User{ID: "src", Username: "src", URI: strPtr("https://local.example/users/src")}
	userRepo.Users["src"] = src
	dstURI := "https://remote.example/users/dst"
	resolver.byURI = map[string]*model.User{dstURI: {
		ID: "dst", URI: strPtr(dstURI), AlsoKnownAs: strPtr("https://local.example/users/src"),
	}}
	putLocalFollowing(followingRepo, "src", "followee1")

	assert.NoError(t, svc.Move(src, dstURI))
	require.NotNil(t, src.MovedToURI, "移行そのものは成立する")
}

// フォロー一覧の取得に失敗しても移行は成功させる。
func TestMove_DelayedUnfollowListFailureIsBestEffort(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["src"] = &model.User{ID: "src"}
	followingRepo := &listFailFollowingRepo{
		MockFollowingRepository: testutil.NewMockFollowingRepository(),
	}
	fq := &fakeFollowQueue{}

	require.NoError(t, moveWithRepos(t, userRepo, followingRepo, fq))
	assert.Zero(t, fq.unfollowCalls)
}

// **同一インスタンス内の移行を AP で解決しない。** `ResolveActor` は自ホストの
// actor URI を拒否する (自分自身を AP で取りに行くと「リモート扱いの」shadow
// user 行ができるため)。ローカル宛ての移行はそれとは別の話なので、`user` 行を
// 直接引く (upstream の `fetchPerson` と同じ扱い)。
func TestMove_LocalDestinationResolvedFromDB(t *testing.T) {
	const srcURI = "https://local.example/users/src"
	const dstURI = "https://local.example/users/dst"

	resolver := &fakeResolver{
		byURI:    map[string]*model.User{},
		localIDs: map[string]string{dstURI: "dst"},
	}
	svc, userRepo, _ := newService(resolver, &fakeDeliverer{})
	src := &model.User{ID: "src", Username: "me"}
	userRepo.Users["src"] = src
	userRepo.Users["dst"] = &model.User{ID: "dst", Username: "other", AlsoKnownAs: strPtr(srcURI)}

	require.NoError(t, svc.Move(src, dstURI))
	assert.Empty(t, resolver.calls, "ローカル宛ての移行で AP の actor 取得を呼んでいる")

	moved, err := userRepo.FindByID("src")
	require.NoError(t, err)
	require.NotNil(t, moved.MovedToURI)
	assert.Equal(t, dstURI, *moved.MovedToURI)
}

// #3330: ExtractLocalUserID reads the last segment of any `{url}/` URI (like
// upstream fetchPerson), so a non-canonical local URI can name the
// destination. The canonical actor URI, not the given one, is what gets
// stored in movedToUri / alsoKnownAs and delivered.
func TestMove_LocalDestinationStoresCanonicalURI(t *testing.T) {
	const srcURI = "https://local.example/users/src"
	const givenURI = "https://local.example/notes/dst"
	const canonical = "https://local.example/users/dst"

	resolver := &fakeResolver{
		byURI:    map[string]*model.User{},
		localIDs: map[string]string{givenURI: "dst"},
	}
	deliverer := &fakeDeliverer{}
	svc, userRepo, _ := newService(resolver, deliverer)
	src := &model.User{ID: "src", Username: "me"}
	userRepo.Users["src"] = src
	userRepo.Users["dst"] = &model.User{ID: "dst", Username: "other", AlsoKnownAs: strPtr(srcURI)}

	require.NoError(t, svc.Move(src, givenURI))

	moved, err := userRepo.FindByID("src")
	require.NoError(t, err)
	require.NotNil(t, moved.MovedToURI)
	assert.Equal(t, canonical, *moved.MovedToURI)
	require.NotNil(t, moved.AlsoKnownAs)
	assert.Equal(t, canonical, *moved.AlsoKnownAs)
	require.NotEmpty(t, deliverer.body)
	assert.Contains(t, string(deliverer.body), canonical)
	assert.NotContains(t, string(deliverer.body), givenURI)
}
