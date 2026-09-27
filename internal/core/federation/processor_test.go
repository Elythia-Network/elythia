package federation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	coreblocking "github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/core/federation"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const aliceActor = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/users/alice",
	"type": "Person",
	"preferredUsername": "alice",
	"name": "Alice",
	"inbox": "https://remote.example/users/alice/inbox",
	"publicKey": {"publicKeyPem": "PEM"}
}`

func newProcessor(t *testing.T, fetcherBody string) (*federation.Processor, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockNoteRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(fetcherBody)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	processor := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	return processor, repo, followingRepo, noteRepo
}

func TestProcess_FollowHappyPath(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	// 受信側の自分 (local) を登録
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, followingRepo.Followings, 1)
}

// stubInboundFollowAcceptor records SendAcceptForInboundFollow /
// SendRejectForInboundFollow calls for assertion.
type stubInboundFollowAcceptor struct {
	calls []struct {
		followerID, followeeID string
		followRaw              string
	}
	rejects []struct {
		followerID, followeeID string
		followRaw              string
	}
}

func (s *stubInboundFollowAcceptor) SendAcceptForInboundFollow(follower, followee *model.User, raw json.RawMessage) error {
	s.calls = append(s.calls, struct {
		followerID, followeeID string
		followRaw              string
	}{follower.ID, followee.ID, string(raw)})
	return nil
}

func (s *stubInboundFollowAcceptor) SendRejectForInboundFollow(follower, followee *model.User, raw json.RawMessage) error {
	s.rejects = append(s.rejects, struct {
		followerID, followeeID string
		followRaw              string
	}{follower.ID, followee.ID, string(raw)})
	return nil
}

func TestProcess_FollowInvokesAcceptorWithOriginalRaw(t *testing.T) {
	// inbound Follow が成立 (Following 作成) したら、original Follow の raw を
	// そのまま InboundFollowAcceptor に渡す。相手は自分の送った Follow.id と
	// 照合できるようになる。
	p, repo, _, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	acceptor := &stubInboundFollowAcceptor{}
	p.SetInboundFollowAcceptor(acceptor)

	body := []byte(`{
		"type": "Follow",
		"id": "https://remote.example/follows/abc123",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, acceptor.calls, 1)
	assert.Equal(t, "bob", acceptor.calls[0].followeeID)
	// original の id を含んだ raw JSON が渡っていること。
	assert.Contains(t, acceptor.calls[0].followRaw, "https://remote.example/follows/abc123")
}

func TestProcess_FollowAlreadyFollowingStillSendsAccept(t *testing.T) {
	// 既に Following が成立している状態で再度 Follow が来た場合 (相手が
	// Accept を見逃したなど) も Accept を再送する。remote 側の pending
	// 状態を idempotent に解消するため。
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	// 事前に Following を作っておく (remote alice → local bob)
	// alice の ID は aliceActor の ResolveActor 結果に依存するので一旦
	// Process を走らせて row を作らせる。
	body := []byte(`{
		"type": "Follow",
		"id": "https://remote.example/follows/first",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, followingRepo.Followings, 1)

	// 同じ Follow を再送 → ErrAlreadyFollowing で service.Follow は弾くが
	// acceptor は呼ばれるべき。
	acceptor := &stubInboundFollowAcceptor{}
	p.SetInboundFollowAcceptor(acceptor)
	retryBody := []byte(`{
		"type": "Follow",
		"id": "https://remote.example/follows/retry",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(retryBody))
	require.Len(t, acceptor.calls, 1)
	assert.Contains(t, acceptor.calls[0].followRaw, "https://remote.example/follows/retry")
}

func TestProcess_FollowAlreadyRequestedStillSucceeds(t *testing.T) {
	// locked local user に対する再送 Follow。1回目で FollowRequest が作られ、
	// 2回目は ErrAlreadyRequested で弾かれるが handleFollow 側では吸収して
	// エラーを返さない (相手が retry を続けるのを防ぐ)。Accept は送らない
	// (まだ承認されていないので)。
	p, repo, _, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	// locked local user
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", IsLocked: true}
	acceptor := &stubInboundFollowAcceptor{}
	p.SetInboundFollowAcceptor(acceptor)

	body := []byte(`{
		"type": "Follow",
		"id": "https://remote.example/follows/first",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	// 2回目もエラーを返さない
	require.NoError(t, p.Process(body))
	// locked 相手の Follow は Accept を送らない (承認時に送る)
	assert.Empty(t, acceptor.calls)
}

func TestProcess_FollowLocalUserByIDResolution(t *testing.T) {
	// 実本番のローカルユーザー row は user.uri が NULL のまま保存されるため、
	// FindByURI では解決できない。localBaseURL が設定されていれば
	// "{baseURL}/users/{id}" 形式の URI から ID を抜き出して FindByID で
	// lookup する経路が通るはず。
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	// URI を持たないローカルユーザー
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, followingRepo.Followings, 1)
	for _, f := range followingRepo.Followings {
		assert.Equal(t, "bob", f.FolloweeID)
	}
}

func TestProcess_FollowAlreadyFollowing(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	// alice → bob を resolver で予め解決させる
	body := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))

	// 2回目: ErrAlreadyFollowing は飲み込まれる
	require.NoError(t, p.Process(body))
	assert.Len(t, followingRepo.Followings, 1)
}

func TestProcess_FollowUnknownFollowee(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/ghost"
	}`)
	err := p.Process(body)
	assert.Error(t, err)
}

func TestProcess_FollowResolveError(t *testing.T) {
	p, _, _, _ := newProcessor(t, "{not json")
	body := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	err := p.Process(body)
	assert.Error(t, err)
}

func TestProcess_FollowMissingObject(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice"}`)
	err := p.Process(body)
	assert.Error(t, err)
}

func TestProcess_UndoFollow(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	// First Follow
	follow := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(follow))
	require.Len(t, followingRepo.Followings, 1)

	// Then Undo
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://remote.example/users/alice",
			"object": "https://example.com/users/bob"
		}
	}`)
	require.NoError(t, p.Process(undo))
	assert.Empty(t, followingRepo.Followings)
}

// #1948-21: locked local user 宛の Undo(Follow) は pending FollowRequest を取り消す
// (upstream undoFollow は cancelFollowRequest を呼ぶ。これが無いと phantom request が残る)。
func TestProcess_UndoFollowCancelsPendingRequest(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	followReqRepo := testutil.NewMockFollowRequestRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, followReqRepo, idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURL("https://example.com")
	p.SetInboundFollowAcceptor(&stubInboundFollowAcceptor{})
	// locked local user → Follow は FollowRequest を作る。
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", IsLocked: true}

	follow := []byte(`{
		"type": "Follow",
		"id": "https://remote.example/follows/1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(follow))
	received, _ := followReqRepo.CountReceived("bob")
	require.Equal(t, int64(1), received, "locked user への Follow で FollowRequest が作られる")

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://remote.example/users/alice",
			"object": "https://example.com/users/bob"
		}
	}`)
	require.NoError(t, p.Process(undo))
	received, _ = followReqRepo.CountReceived("bob")
	assert.Equal(t, int64(0), received, "Undo(Follow) で pending FollowRequest が削除される (#1948-21)")
}

// #1948-21: target が無い Move は actor を force-resolve しない (upstream は
// 'skip: invalid activity target')。
func TestProcess_MoveWithoutTargetSkips(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{"type":"Move","actor":"https://remote.example/users/alice"}`)
	require.NoError(t, p.Process(body))
	_, err := repo.FindByURI("https://remote.example/users/alice")
	assert.Error(t, err, "target が無い Move は actor を force-resolve しない (#1948-21)")
}

// #1948-21: target がある Move は actor を force-resolve する (upstream updatePerson)。
func TestProcess_MoveWithTargetResolvesActor(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{"type":"Move","actor":"https://remote.example/users/alice","target":"https://remote.example/users/alice2"}`)
	require.NoError(t, p.Process(body))
	u, err := repo.FindByURI("https://remote.example/users/alice")
	require.NoError(t, err, "target がある Move は actor を force-resolve する (#1948-21)")
	assert.NotNil(t, u)
}

func TestProcess_UndoUnknownType(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Like"}
	}`)
	err := p.Process(undo)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_UndoBadInner(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": "not-a-json-object"
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

func TestProcess_UndoNotFollowing(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://remote.example/users/alice",
			"object": "https://example.com/users/bob"
		}
	}`)
	// 既にフォロー解除済みでもエラーにならない
	require.NoError(t, p.Process(undo))
}

func TestProcess_UndoResolveError(t *testing.T) {
	p, _, _, _ := newProcessor(t, "{not json")
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Follow","actor":"x","object":"https://example.com/users/bob"}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

func TestProcess_UndoUnknownFollowee(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Follow","actor":"x","object":"https://example.com/users/ghost"}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

func TestProcess_UndoMissingObject(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {"type":"Follow","actor":"x"}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

func TestProcess_Accept(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{"type":"Accept","actor":"https://remote.example/users/alice","object":{}}`)
	require.NoError(t, p.Process(body))
}

func TestProcess_UnsupportedType(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{"type":"Like","actor":"https://remote.example/users/alice"}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

func TestProcess_UnknownType(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{"type":"TentativeReject","actor":"https://remote.example/users/alice"}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

func TestProcess_BadJSON(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	err := p.Process([]byte(`{not json`))
	assert.Error(t, err)
}

func TestProcess_MissingActor(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	err := p.Process([]byte(`{"type":"Follow"}`))
	assert.Error(t, err)
}

// failingFollowingService isn't easy to construct since it's a concrete type;
// we exercise the non-AlreadyFollowing follow error path by using a follower
// who doesn't exist yet (resolver create works but follow service errors out
// because follower==followee). Use SelfFollow.
func TestProcess_UndoFollowWithNestedObject(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	// First follow
	follow := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(follow))
	require.Len(t, followingRepo.Followings, 1)

	// Undo with nested object form (object: {id: ...})
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"object": {"id": "https://example.com/users/bob"}
		}
	}`)
	require.NoError(t, p.Process(undo))
	assert.Empty(t, followingRepo.Followings)
}

func TestProcess_UndoNestedObjectInvalid(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	// nested object missing id field
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"object": {"foo": "bar"}
		}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

// inner activity に型エラーがあっても Undo を落とさない。`inner.actor` が
// embedded object だと `Actor string` で型エラーになり、握らないと
// `invalid undo object` を返す。**inbox job は error だと retry を
// 使い切って failed bucket に落ちる** (既定 8 回、
// `server/queue_factory.go` の defaultInboxJobMaxAttempts) ので、
// unfollow がまったく届かない (#2665)。
//
// (この経路が使うのは外側の `act.Actor` と `inner.Object` で、`inner.Actor`
// 自体は読まない。効いているのは型エラーを握ることだけ。)
func TestProcess_UndoFollow_InnerActorEmbeddedObject(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	// **local user は本番と同じく uri NULL。** `resolveTargetUser` の
	// local-ID 分岐を通すために base URL を配線する。
	p.SetLocalBaseURL("https://example.com")
	// remote follower alice が local followee bob をフォロー済み。
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "alice1", FolloweeID: "bob"}

	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": {"id": "https://remote.example/users/alice", "type": "Person"},
			"object": "https://example.com/users/bob"
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, followingRepo.Followings, "embedded object の actor でも Undo(Follow) が届くこと")
}

func TestProcess_UndoNestedObjectBadJSON(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	// nested object is a JSON number — not string and not object
	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"object": 42
		}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

// failingFollowingRepo causes Delete to fail (non-NotFollowing path).
type failingFollowingRepo struct {
	*testutil.MockFollowingRepository
}

func (f *failingFollowingRepo) Delete(_ *model.Following) error {
	return errors.New("delete failed")
}

func TestProcess_UndoFollowDeleteError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	mockFR := testutil.NewMockFollowingRepository()
	// pre-existing follow record so Unfollow finds it
	mockFR.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "alice", FolloweeID: "bob"}
	followingRepo := &failingFollowingRepo{MockFollowingRepository: mockFR}

	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	// Setup followee bob
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	// Pre-resolve alice to ID "alice" by first calling resolver
	aliceURI := "https://remote.example/users/alice"
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", URI: &aliceURI}

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"object": "https://example.com/users/bob"
		}
	}`)
	err := p.Process(undo)
	assert.Error(t, err)
}

func TestProcess_FollowSelfFollow(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	// alice/bob 同IDのケース: alice as both actor and target via URI alias
	uri := "https://remote.example/users/alice"
	// alice already in repo (resolver populates) ... we register the same user
	// as a local "followee" by setting URI.
	repo.Users["alice-local"] = &model.User{ID: "alice-local", Username: "alice", URI: &uri}
	// 直接呼ぶことで follower==followee を発生させる
	body := []byte(`{
		"type": "Follow",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/users/alice"
	}`)
	err := p.Process(body)
	// SelfFollow がそのまま伝搬する
	assert.Error(t, err)
}

// --- Block ---

func newProcessorWithBlocking(t *testing.T) (*federation.Processor, *testutil.MockUserRepository, *testutil.MockBlockingRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	blockingSvc := coreblocking.NewService(repo, blockingRepo, followingRepo, idGen)
	processor := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	processor.SetBlockingService(blockingSvc)
	return processor, repo, blockingRepo
}

func TestProcess_BlockHappyPath(t *testing.T) {
	p, repo, blockingRepo := newProcessorWithBlocking(t)
	// 実本番のローカルユーザー row は user.uri が NULL なので、この条件で
	// test する (handleBlock が resolveTargetUser で ID 解決することを検証)。
	p.SetLocalBaseURL("https://example.com")
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Block",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))
	// resolverがaliceを解決した際に生成されたIDを取得
	var aliceID string
	for _, u := range repo.Users {
		if u.Username == "alice" {
			aliceID = u.ID
			break
		}
	}
	require.NotEmpty(t, aliceID)
	exists, _ := blockingRepo.Exists(aliceID, "bob")
	assert.True(t, exists)
}

func TestProcess_BlockAlreadyBlocking(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Block",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	// 2回ブロックしてもエラーにならない（冪等）
	require.NoError(t, p.Process(body))
	require.NoError(t, p.Process(body))
}

func TestProcess_BlockRemoteUser_Ignored(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	host := "remote2.example"
	charlieURI := "https://remote2.example/users/charlie"
	repo.Users["charlie"] = &model.User{ID: "charlie", Username: "charlie", URI: &charlieURI, Host: &host}

	body := []byte(`{
		"type": "Block",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote2.example/users/charlie"
	}`)
	// リモートユーザーへのブロックは無視
	require.NoError(t, p.Process(body))
}

func TestProcess_UndoBlock(t *testing.T) {
	p, repo, blockingRepo := newProcessorWithBlocking(t)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	// まずブロック
	body := []byte(`{
		"type": "Block",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	require.NoError(t, p.Process(body))

	// resolverが生成したaliceのIDを取得
	var aliceID string
	for _, u := range repo.Users {
		if u.Username == "alice" {
			aliceID = u.ID
			break
		}
	}

	// Undo Block
	undoBody := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Block",
			"actor": "https://remote.example/users/alice",
			"object": "https://example.com/users/bob"
		}
	}`)
	require.NoError(t, p.Process(undoBody))
	exists, _ := blockingRepo.Exists(aliceID, "bob")
	assert.False(t, exists)
}

func TestProcess_UndoBlock_NotBlocking(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	// ブロックしていない状態でUndo Blockしてもエラーにならない
	undoBody := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Block",
			"actor": "https://remote.example/users/alice",
			"object": "https://example.com/users/bob"
		}
	}`)
	require.NoError(t, p.Process(undoBody))
}

// --- Flag ---

func TestProcess_FlagHappyPath(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)

	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": ["https://example.com/users/bob"],
		"content": "spam"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, abuseRepo.Reports, 1)
	for _, r := range abuseRepo.Reports {
		// #1560: comment は content + "\n" + JSON(flagged URIs)。
		assert.Equal(t, "spam\n[\"https://example.com/users/bob\"]", r.Comment)
	}
}

func TestProcess_FlagSingleURI(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)

	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob",
		"content": "abuse"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, abuseRepo.Reports, 1)
}

// upstream は `getApIds(activity.object)` なので、要素が `{"id": ...}` の
// object 形式でも読む (ApInboxService.ts:560)。`[]string` 決め打ちに戻すと
// この形の通報が丸ごと落ちる (#2665)。
func TestProcess_FlagObjectFormURIs(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)

	// **local user は本番と同じく uri NULL。** handleFlag は
	// `ExtractLocalUserID` → `FindByID` で解決する (#1560 で「任意 host の
	// URI を受ける」旧実装を潰した硬化)。偽の uri を持たせると `FindByURI`
	// に差し戻す変異が素通りする。
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": [{"id": "https://example.com/users/bob", "type": "Person"}],
		"content": "spam"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, abuseRepo.Reports, 1, "object 形式の要素でも通報を保存する")
	for _, r := range abuseRepo.Reports {
		// **`TestProcess_FlagHappyPath` (素の URI 配列) と一字一句同じになる
		// ことが「id を剥がしている」ことの証明。** object がそのまま
		// 載っていれば別の文字列になる。
		assert.Equal(t, "spam\n[\"https://example.com/users/bob\"]", r.Comment)
	}
}

// 単一の object 形式 (配列ですらない) も同じ。`getApIds` は非配列を 1 件に
// 包んでから `getApId` を通す。
//
// **uniqueness は主張しない。** `APIDList` の型レベルは
// `internal/activitypub` の `TestAPIDList_UnmarshalJSON/single_object` が
// 固定しており、同じ shape は `TestMergeCreateAudience` も通る。ここが見て
// いるのは handleFlag が `[]string` 決め打ちに戻る変異で、それを落とすのは
// Flag 系 3 件 (SingleURI / ObjectFormURIs / 本テスト) だけ (実測)。
func TestProcess_FlagSingleObjectForm(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)

	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": {"id": "https://example.com/users/bob", "type": "Person"},
		"content": "abuse"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, abuseRepo.Reports, 1, "単一 object 形式でも通報を保存する")
}

func TestProcess_FlagFromNote(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	noteRepo := testutil.NewMockNoteRepository()

	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	noteURI := "https://example.com/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", URI: &noteURI}

	// noteRepoはProcessor内のものとは別なので、直接ユーザーURIで解決する
	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": ["https://example.com/users/bob"],
		"content": "reported note"
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, abuseRepo.Reports, 1)
}

// --- Move ---

func TestProcess_Move(t *testing.T) {
	p, _, _ := newProcessorWithBlocking(t)
	// target を伴う Move は actor を再解決する (target-less の skip は
	// TestProcess_MoveWithoutTargetSkips で別途 pin、#1948-21)。
	body := []byte(`{
		"type": "Move",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/users/alice",
		"target": "https://remote.example/users/alice2"
	}`)
	require.NoError(t, p.Process(body))
}

// #1948-21: target が AS Link object ({href:...}) 形式でも getApHrefNullable 相当に
// href を読んで受理する (readObjectString の {id} 読みでは誤 skip していた)。
func TestProcess_MoveTargetHrefObject(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Move",
		"actor": "https://remote.example/users/alice",
		"target": {"type": "Link", "href": "https://remote.example/users/alice2"}
	}`)
	require.NoError(t, p.Process(body))
	u, err := repo.FindByURI("https://remote.example/users/alice")
	require.NoError(t, err, "{href} 形式 target でも actor を force-resolve する (#1948-21)")
	assert.NotNil(t, u)
}

// --- EmojiReaction ---

func TestProcess_EmojiReaction(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	// EmojiReaction typeはLikeと同じハンドラにルーティングされる
	// reactionServiceがnilなので ErrUnsupportedActivity が返る
	body := []byte(`{
		"type": "EmojiReaction",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "👍"
	}`)
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_EmojiReact(t *testing.T) {
	p, _, _ := newProcessorWithBlocking(t)
	body := []byte(`{
		"type": "EmojiReact",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/n1",
		"content": "❤️"
	}`)
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

// --- Block nil service ---

func TestProcess_BlockWithoutService(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Block",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/users/bob"
	}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

// --- Add/Remove ---

func newProcessorWithPinning(t *testing.T) (*federation.Processor, *testutil.MockUserRepository, *testutil.MockNoteRepository, *testutil.MockUserNotePiningRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	processor := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	processor.SetPinningRepo(piningRepo, idGen)
	return processor, repo, noteRepo, piningRepo
}

// resolveAliceAndSetFeatured はalice actorを事前解決してFeaturedフィールドを設定する。
func resolveAliceAndSetFeatured(t *testing.T, p *federation.Processor, repo *testutil.MockUserRepository) string {
	t.Helper()
	// Followでaliceを解決させる（ダミーのfollowee不要、ResolveActorが呼ばれれば良い）
	dummyURI := "https://example.com/users/dummy"
	repo.Users["dummy"] = &model.User{ID: "dummy", Username: "dummy", URI: &dummyURI}
	_ = p.Process([]byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/dummy"}`))

	var aliceID string
	featured := "https://remote.example/users/alice/collections/featured"
	for _, u := range repo.Users {
		if u.Username == "alice" {
			aliceID = u.ID
			u.Featured = &featured
			break
		}
	}
	require.NotEmpty(t, aliceID)
	return aliceID
}

func TestProcess_Add(t *testing.T) {
	p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
	aliceID := resolveAliceAndSetFeatured(t, p, repo)

	noteURI := "https://remote.example/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: aliceID, URI: &noteURI}

	body := []byte(`{
		"type": "Add",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1",
		"target": "https://remote.example/users/alice/collections/featured"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, piningRepo.Pinings, 1)
	for _, pin := range piningRepo.Pinings {
		assert.Equal(t, aliceID, pin.UserID)
		assert.Equal(t, "n1", pin.NoteID)
	}
}

// **`Add` でピン留めできるのは自分の投稿だけ。** 見ないと、署名さえ通る
// リモート actor が他人のノートを自分のプロフィールに並べられる。ローカルの
// 非公開ノートも `ResolveNote` は可視性を見ずに引くので、`users/show` の
// `pinnedNoteIds` 経由でその ID が未認証の相手へ出る。
//
// pull 側 (`resolveFeaturedNotes`) には同じ規則が既にあり、push 側のここだけが
// 取り残されていた。
func TestProcess_Add_RejectsOtherUsersNote(t *testing.T) {
	p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
	_ = resolveAliceAndSetFeatured(t, p, repo)

	// 著者は alice ではない。URI は alice のホストなので、host 一致だけでは
	// 弾けないことに注意。
	noteURI := "https://remote.example/notes/victim"
	noteRepo.Notes["victim"] = &model.Note{ID: "victim", UserID: "someone-else", URI: &noteURI}

	body := []byte(`{
		"type": "Add",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/victim",
		"target": "https://remote.example/users/alice/collections/featured"
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, piningRepo.Pinings, "他人のノートがピン留めされた")
}

// **件数の上限。** `Add` は署名さえ通れば何度でも送れるので、上限が無いと
// `user_note_pining` を無制限に増やせる。`internal/api/users/handler.go` の
// 設計メモ (#1489) は `pinnedNoteIds` を絞らない理由として「pinning は
// per-user 上限が厳しい」と書いており、その前提がここで壊れる。
//
// **上限に当たったら捨てずに入れ替える。** upstream は `Remove(A)` と
// `Add(B)` を別々の deliver job で配るので、こちらの inbox が並列に処理すると
// `Add(B)` が先に着くことがある。そこで捨てると B は次の actor 更新まで
// 現れない (既定 TTL 24 時間)。
//
// **「上限未満なら通る」まで見る。** それが無いと、常に弾く実装でもテストが
// 通ってしまう (変異検証で実測)。
func TestProcess_Add_EvictsTheOldestAtThePinLimit(t *testing.T) {
	p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
	aliceID := resolveAliceAndSetFeatured(t, p, repo)

	add := func(n string) {
		t.Helper()
		uri := "https://remote.example/notes/" + n
		noteRepo.Notes[n] = &model.Note{ID: n, UserID: aliceID, URI: &uri}
		body := []byte(`{
			"type": "Add",
			"actor": "https://remote.example/users/alice",
			"object": "` + uri + `",
			"target": "https://remote.example/users/alice/collections/featured"
		}`)
		require.NoError(t, p.Process(body))
	}
	pinnedNotes := func() []string {
		var out []string
		for _, pin := range piningRepo.Pinings {
			out = append(out, pin.NoteID)
		}
		sort.Strings(out)
		return out
	}

	for i := range federation.FeaturedPinLimit {
		add(fmt.Sprintf("n%d", i))
		require.Len(t, piningRepo.Pinings, i+1, "上限に達する前のピンが弾かれた")
	}

	add("overflow")
	assert.Len(t, piningRepo.Pinings, federation.FeaturedPinLimit, "上限を超えてピン留めできた")
	assert.Contains(t, pinnedNotes(), "overflow", "上限に当たった Add を捨てている")
	assert.NotContains(t, pinnedNotes(), "n0", "最も古いピンが外れていない")
}

// **同じノートの再送は何も動かさない。** 上限判定より前に重複を弾かないと、
// 「既にあるものを入れるために別のピンを外す」ことになる。
func TestProcess_Add_DuplicateAtTheLimitIsANoOp(t *testing.T) {
	p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
	aliceID := resolveAliceAndSetFeatured(t, p, repo)

	add := func(n string) {
		t.Helper()
		uri := "https://remote.example/notes/" + n
		noteRepo.Notes[n] = &model.Note{ID: n, UserID: aliceID, URI: &uri}
		require.NoError(t, p.Process([]byte(`{
			"type": "Add",
			"actor": "https://remote.example/users/alice",
			"object": "`+uri+`",
			"target": "https://remote.example/users/alice/collections/featured"
		}`)))
	}
	for i := range federation.FeaturedPinLimit {
		add(fmt.Sprintf("n%d", i))
	}
	before := make(map[string]string, len(piningRepo.Pinings))
	for id, pin := range piningRepo.Pinings {
		before[id] = pin.NoteID
	}

	// 上限に達した状態で、既にピン留め済みのノートをもう一度送る。
	add("n0")

	after := make(map[string]string, len(piningRepo.Pinings))
	for id, pin := range piningRepo.Pinings {
		after[id] = pin.NoteID
	}
	assert.Equal(t, before, after, "重複した Add でピンが入れ替わっている")
}

// 数えられなかったときは error を返す。握り潰すと上限が効かないまま進むし、
// nil で捨てると inbox job が「処理済み」として再試行しない。
func TestProcess_Add_CountFailureIsRetryable(t *testing.T) {
	p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
	aliceID := resolveAliceAndSetFeatured(t, p, repo)

	noteURI := "https://remote.example/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: aliceID, URI: &noteURI}
	piningRepo.CountErr = errors.New("boom")

	body := []byte(`{
		"type": "Add",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1",
		"target": "https://remote.example/users/alice/collections/featured"
	}`)
	err := p.Process(body)
	require.Error(t, err)
	assert.Empty(t, piningRepo.Pinings)
}

func TestProcess_Add_WrongTarget(t *testing.T) {
	p, _, _, piningRepo := newProcessorWithPinning(t)
	// targetがfeaturedでない場合は無視
	body := []byte(`{
		"type": "Add",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1",
		"target": "https://remote.example/users/alice/collections/other"
	}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, piningRepo.Pinings)
}

func TestProcess_Remove(t *testing.T) {
	p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
	aliceID := resolveAliceAndSetFeatured(t, p, repo)

	noteURI := "https://remote.example/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: aliceID, URI: &noteURI}

	// まずAddでピン留め
	addBody := []byte(`{
		"type": "Add",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1",
		"target": "https://remote.example/users/alice/collections/featured"
	}`)
	require.NoError(t, p.Process(addBody))
	require.True(t, len(piningRepo.Pinings) > 0)

	// Remove
	removeBody := []byte(`{
		"type": "Remove",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1",
		"target": "https://remote.example/users/alice/collections/featured"
	}`)
	require.NoError(t, p.Process(removeBody))
}

// ピン留めしていないノートの Remove はエラーにならない (not-found は ack)。
//
// **featured を設定しないと pin の lookup に到達しない。** `handleRemove` は
// actor の featured と target が一致しなければそこで抜けるので、設定しないと
// 「pin が無い」を試したつもりで分岐に一度も入らない (#3115 で実測)。
func TestProcess_Remove_NotPinned(t *testing.T) {
	p, repo, noteRepo, _ := newProcessorWithPinning(t)
	aliceID := resolveAliceAndSetFeatured(t, p, repo)
	noteURI := "https://remote.example/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: aliceID, URI: &noteURI}

	require.NoError(t, p.Process([]byte(removeFeaturedBody)))
}

// **unpin 側も DB 障害は ack しない** (#3115)。
//
// ack すると pin が外れないまま残り、相手は再送しないので二度と直らない。
// 直上の `Remove_NotPinned` (not-found = ack が正しい) と対にして読むこと。
func TestProcess_Remove_LookupFailurePropagates(t *testing.T) {
	// **featured を先に設定する。** `handleRemove` は actor の featured と
	// target が一致しなければ**そこで抜ける**ので、設定しないと note の lookup に
	// 一度も到達しない (この経路を試したつもりで何も試していない形になる)。
	boom := errors.New("connection refused")
	t.Run("note の lookup が落ちる", func(t *testing.T) {
		p, repo, noteRepo, _ := newProcessorWithPinning(t)
		resolveAliceAndSetFeatured(t, p, repo)
		noteRepo.FindByURIErr = boom
		require.ErrorIs(t, p.Process([]byte(removeFeaturedBody)), boom, "DB 障害を ack している")
	})
	t.Run("pin の lookup が落ちる", func(t *testing.T) {
		p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
		aliceID := resolveAliceAndSetFeatured(t, p, repo)
		noteURI := "https://remote.example/notes/n1"
		noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: aliceID, URI: &noteURI}
		piningRepo.FindErr = boom
		require.ErrorIs(t, p.Process([]byte(removeFeaturedBody)), boom, "DB 障害を ack している")
	})
	// **書き込みの失敗も伝播する。** lookup だけ retry に倒してここを捨てると、
	// 「pin が外れないまま残る」という同じ結末が DELETE 側で残る。
	t.Run("pin の削除が落ちる", func(t *testing.T) {
		p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
		aliceID := resolveAliceAndSetFeatured(t, p, repo)
		noteURI := "https://remote.example/notes/n1"
		noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: aliceID, URI: &noteURI}
		piningRepo.Pinings["p1"] = &model.UserNotePining{ID: "p1", UserID: aliceID, NoteID: "n1"}
		piningRepo.DeleteErr = boom
		require.ErrorIs(t, p.Process([]byte(removeFeaturedBody)), boom, "削除の失敗を ack している")
	})
	// **not-found は ack のまま。** こちらを retry に倒すと、こちらが取り込んで
	// いないノートの pin 解除という**ごく普通の Remove** が dead letter に積まれる。
	t.Run("note が無いのは ack", func(t *testing.T) {
		p, repo, _, _ := newProcessorWithPinning(t)
		resolveAliceAndSetFeatured(t, p, repo)
		require.NoError(t, p.Process([]byte(removeFeaturedBody)))
	})
}

const removeFeaturedBody = `{
	"type": "Remove",
	"actor": "https://remote.example/users/alice",
	"object": "https://remote.example/notes/n1",
	"target": "https://remote.example/users/alice/collections/featured"
}`

func TestProcess_AddWithoutRepo(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Add",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1",
		"target": "https://remote.example/users/alice/collections/featured"
	}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

func TestProcess_RemoveWithoutRepo(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Remove",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/notes/n1",
		"target": "https://remote.example/users/alice/collections/featured"
	}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

// --- Accept ---

func TestProcess_AcceptFollow(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	// ローカルユーザーbob（follower）を登録
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	// aliceからのAcceptを処理（bobからaliceへのFollow承認）
	// ExtractLocalUserIDで /users/bob → ID "bob" と解決される
	acceptBody := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/bob",
			"object": "https://remote.example/users/alice"
		}
	}`)
	// AcceptRequest自体はフォローリクエストが存在しなくてもエラーにならない（nil返却）
	require.NoError(t, p.Process(acceptBody))
}

// upstream Misskey #17340 (triage #999) の actor normalization は inner
// activity (Accept / Reject / Undo の object 内 activity) にも適用する設計。
// inner.actor が {"id": "..."} embedded object 形式で送られてきた場合に、
// inner.normalizeActor(act.Object) 経由で string ID に展開され、後段の
// readActorString(inner) で follower URI が抽出できることを実証する。
// TestProcess_AcceptFollow と対になる "inner side" の coverage。
func TestProcess_AcceptFollow_InnerActorEmbeddedObject(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	reqRepo := testutil.NewMockFollowRequestRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, reqRepo, idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	// remote followee alice と local follower bob。**承認待ちの request を
	// 置いて「承認された」ことを観測可能にする。** nil 返却だけを見る形だと
	// 救済が効かなくなっても test が通ってしまう (#2665)。
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}

	acceptBody := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": {"id": "https://example.com/users/bob", "type": "Person"},
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(acceptBody))
	assert.Len(t, followingRepo.Followings, 1, "embedded object の actor でもフォローが成立すること")
	assert.Empty(t, reqRepo.Requests, "承認された request は消えること")
}

// stubAcceptBlockingChecker reports the configured pair as blocked.
type stubAcceptBlockingChecker struct{ blockerID, blockeeID string }

func (s *stubAcceptBlockingChecker) IsBlocked(blockerID, blockeeID string) (bool, error) {
	return blockerID == s.blockerID && blockeeID == s.blockeeID, nil
}

// AcceptRequest の多層防御 (#3111 レビュー) が ErrBlocked を返すケース:
// local follower (bob) が remote followee (alice) を block した後、best-effort
// な申請取り消しが失敗して申請が残ったまま alice からの Accept が届いた状況を
// 再現する。retry しても結果は変わらないので ack (nil 返却) すること、
// エラーのまま inbox job を落とさないことを見る (#534 の idempotency
// invariant)。
func TestProcess_AcceptFollow_BlockedSwallowsError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	reqRepo := testutil.NewMockFollowRequestRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, reqRepo, idGen)
	followingSvc.SetBlockingChecker(&stubAcceptBlockingChecker{blockerID: "bob", blockeeID: "alice1"})
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	// block 時の取り消しが失敗した想定で申請を残したままにする。
	reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}

	acceptBody := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/bob",
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(acceptBody), "block による拒否は ack すべきで error を返してはいけない")
	assert.Empty(t, followingRepo.Followings, "block 中なので Following は成立しない")
	assert.Len(t, reqRepo.Requests, 1, "承認しないだけで申請行は消さない")
}

func TestProcess_AcceptNonFollow(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	// inner objectがFollowでない場合は無視
	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Like",
			"actor": "https://example.com/users/bob"
		}
	}`)
	require.NoError(t, p.Process(body))
}

func TestProcess_AcceptBadObject(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	// objectが文字列（Follow IDのURI）の場合は無視
	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/follows/123"
	}`)
	require.NoError(t, p.Process(body))
}

func TestProcess_AcceptUnknownFollower(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	// followerが見つからない場合は無視
	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/ghost",
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(body))
}

// **DB 障害は ack しない** (#3115)。
//
// ack すると相手は 2xx を受け取って**再送しない**ので、一時的な接続断だけで
// Accept が恒久的に失われ、ローカル利用者のフォローリクエストは「リクエスト中」
// のまま永久に残る。error を返せば inbox job が retry する。
//
// **直上の `AcceptUnknownFollower` と対にして読むこと。** あちらは not-found で
// ack が正しく、この 2 本が揃って初めて「種別を見て分けている」ことになる
// (片方だけだと、両方を同じ側へ倒す実装で緑になる)。
func TestProcess_AcceptFollowerLookupFailurePropagates(t *testing.T) {
	// **2 経路とも試す。** follower の URI が自ホストの `/users/{id}` なら
	// `FindByID`、そうでなければ `FindByURI` を引く。**片方だけ guard する形**
	// (#3025 が名指しした失敗形) は、1 経路しか試さないと素通りする。
	// mock の hook も `FindErr` / `FindByURIErr` で分かれている。
	cases := []struct {
		name       string
		followerID string
		arm        func(*testutil.MockUserRepository, error)
	}{
		{
			name:       "ローカル URI (FindByID)",
			followerID: "https://example.com/users/ghost",
			arm:        func(r *testutil.MockUserRepository, e error) { r.FindErr = e },
		},
		{
			name:       "ローカルでない URI (FindByURI)",
			followerID: "https://other.example/users/ghost",
			arm:        func(r *testutil.MockUserRepository, e error) { r.FindByURIErr = e },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, _, _ := newProcessor(t, aliceActor)
			boom := errors.New("connection refused")
			tc.arm(repo, boom)
			body := []byte(`{
				"type": "Accept",
				"actor": "https://remote.example/users/alice",
				"object": {
					"type": "Follow",
					"actor": "` + tc.followerID + `",
					"object": "https://remote.example/users/alice"
				}
			}`)
			require.ErrorIs(t, p.Process(body), boom,
				"DB 障害を ack している (job が成功扱いになり retry されないので Accept が失われる)")
		})
	}
}

// Reject も同じ (#3115)。**Accept と Reject は同じ決定の裏表**なので、片方だけ
// 直すと「承認は拾えるが拒否は落ちる」という非対称が残る。
//
// **Accept と経路が違う。** あちらは `resolver.ExtractLocalUserID` (URL builder 由来)
// で分岐するが、Reject は `resolveTargetUser` = `Processor.localBaseURL` で分岐する。
// `SetLocalBaseURL` を呼ばないと**常に `FindByURI` 側**へ落ちるので、`FindErr`
// だけを立てたテストは分岐に到達せず緑のまま通る (実測)。両方を試す。
func TestProcess_RejectFollowerLookupFailurePropagates(t *testing.T) {
	cases := []struct {
		name         string
		localBaseURL string
		arm          func(*testutil.MockUserRepository, error)
	}{
		{
			name:         "localBaseURL あり (FindByID)",
			localBaseURL: "https://example.com",
			arm:          func(r *testutil.MockUserRepository, e error) { r.FindErr = e },
		},
		{
			name: "localBaseURL なし (FindByURI)",
			arm:  func(r *testutil.MockUserRepository, e error) { r.FindByURIErr = e },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, _, _ := newProcessor(t, aliceActor)
			if tc.localBaseURL != "" {
				p.SetLocalBaseURL(tc.localBaseURL)
			}
			// **actor の解決を先に済ませておく。** `FindByURIErr` を立てると
			// `ResolveActor` も落ちてしまい、その手前で return されて
			// follower の lookup に到達しない。
			dummyURI := "https://example.com/users/dummy"
			repo.Users["dummy"] = &model.User{ID: "dummy", Username: "dummy", URI: &dummyURI}
			require.NoError(t, p.Process([]byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/dummy"}`)))
			boom := errors.New("connection refused")
			tc.arm(repo, boom)
			body := []byte(`{
				"type": "Reject",
				"actor": "https://remote.example/users/alice",
				"object": {
					"type": "Follow",
					"actor": "https://example.com/users/ghost",
					"object": "https://remote.example/users/alice"
				}
			}`)
			require.ErrorIs(t, p.Process(body), boom, "DB 障害を ack している")
		})
	}
}

// **lookup の失敗を ack しない** (#3116)。
//
// #3115 が Accept / Reject / unpin で確立した原則を、残りの経路へ広げたもの。
// ack すると job が成功扱いになって queue が retry しないので、一時的な接続断
// だけで activity が恒久的に失われる。**not-found は ack のまま**で、両方向を
// 対にして固定する (片方だけだと、両方を同じ側へ倒す実装で緑になる)。
func TestProcess_LookupFailuresPropagate(t *testing.T) {
	boom := errors.New("connection refused")

	// 凍結済み actor の判定。**素通しにすると moderation の fail-open** —
	// DB 障害のあいだ凍結済み actor の activity がそのまま処理される。
	t.Run("凍結判定が引けない", func(t *testing.T) {
		p, repo, _, _ := newProcessor(t, aliceActor)
		repo.FindByURIErr = boom
		body := []byte(`{"type":"Delete","actor":"https://remote.example/users/alice","object":"https://remote.example/notes/x"}`)
		require.ErrorIs(t, p.Process(body), boom, "凍結判定を素通りしている")
	})

	// Announce の重複判定。**「重複でない」に倒すと二重 renote になる。**
	// 対象はローカル note にして、`ResolveNote` が fetch せず DB から引ける
	// 形にする (そうしないと dedup の手前で返ってしまう)。
	t.Run("Announce の重複判定が引けない", func(t *testing.T) {
		env := newFullProcessor(t, aliceActor)
		env.noteRepo.Notes["n1"] = &model.Note{
			ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
		}
		env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
		env.noteRepo.FindByURIErr = boom
		body := []byte(`{"id":"https://remote.example/announces/a1","type":"Announce",` +
			`"actor":"https://remote.example/users/alice","object":"https://example.com/notes/n1"}`)
		require.ErrorIs(t, env.processor.Process(body), boom, "重複判定を素通りしている")
	})

	// Delete の note lookup。**ack すると削除されたはずのノートが残る。**
	t.Run("Delete の note が引けない", func(t *testing.T) {
		p, repo, _, noteRepo := newProcessor(t, aliceActor)
		// actor は引けて、note だけ落ちる状況を作る。
		uri := "https://remote.example/users/alice"
		repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", URI: &uri}
		noteRepo.FindByURIErr = boom
		body := []byte(`{"type":"Delete","actor":"https://remote.example/users/alice","object":{"type":"Tombstone","id":"https://remote.example/notes/n1"}}`)
		require.ErrorIs(t, p.Process(body), boom, "削除を取りこぼしている")
	})

	// Update(Person)。**ack するとプロフィールの更新が消える。**
	//
	// **凍結判定と同じ `FindByURI` を使う経路**なので、URI 別に落とす。
	// `FindByURIErr` を立てると手前の凍結判定で返ってしまい、**目的の分岐を
	// 一度も通らないまま緑になる** (実測)。
	t.Run("Update(Person) の user が引けない", func(t *testing.T) {
		p, repo, _, _ := newProcessor(t, aliceActor)
		const actor = "https://remote.example/users/alice"
		// 凍結判定は「まだ取り込んでいない」で通し、handleUpdate の lookup だけ落とす。
		callCount := 0
		repo.FindByURIHook = func(uri string) error {
			if uri != actor {
				return nil
			}
			callCount++
			if callCount == 1 {
				return nil // 凍結判定 (= not-found で素通し)
			}
			return boom // handleUpdate の lookup
		}
		body := []byte(`{"type":"Update","actor":"` + actor + `","object":{"type":"Person","id":"` + actor + `"}}`)
		require.ErrorIs(t, p.Process(body), boom, "プロフィール更新を取りこぼしている")
	})

	// Delete(Actor)。**ack するとリモートのアカウント削除を取りこぼす。**
	t.Run("Delete(Actor) の user が引けない", func(t *testing.T) {
		p, repo, _, _ := newProcessor(t, aliceActor)
		const actor = "https://remote.example/users/alice"
		callCount := 0
		repo.FindByURIHook = func(uri string) error {
			if uri != actor {
				return nil
			}
			callCount++
			if callCount == 1 {
				return nil // 凍結判定
			}
			return boom // isActorDelete の存在確認
		}
		body := []byte(`{"type":"Delete","actor":"` + actor + `","object":"` + actor + `"}`)
		require.ErrorIs(t, p.Process(body), boom, "アカウント削除を取りこぼしている")
	})

	// Update(Note)。**同じ handler の中で最も多い Update 型** — ack すると
	// リモートのノート編集 (text / cw / 添付) が恒久的に落ちる。
	t.Run("Update(Note) の note が引けない", func(t *testing.T) {
		p, repo, _, noteRepo := newProcessor(t, aliceActor)
		host := "remote.example"
		uri := "https://remote.example/users/alice"
		repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", URI: &uri, Host: &host}
		noteRepo.FindByURIErr = boom
		body := []byte(`{"type":"Update","actor":"` + uri + `","object":{"id":"https://remote.example/notes/n1",` +
			`"type":"Note","attributedTo":"` + uri + `","content":"edited"}}`)
		require.ErrorIs(t, p.Process(body), boom, "ノート編集を取りこぼしている")
	})

	// Undo(Accept)。**#3115 が直した Accept / Reject の裏返し** — ack すると
	// accept を撤回された相手をフォローし続ける。
	t.Run("Undo(Accept) の follower が引けない", func(t *testing.T) {
		p, repo, _, _ := newProcessor(t, aliceActor)
		const actor = "https://remote.example/users/alice"
		// **follower はローカル利用者**なので `ExtractLocalUserID` → `FindByID`
		// を通る (`FindByURI` ではない)。凍結判定は `FindByURI` なので干渉しない。
		repo.FindErr = boom
		body := []byte(`{"type":"Undo","actor":"` + actor + `","object":{"type":"Accept","actor":"` + actor + `",` +
			`"object":{"type":"Follow","actor":"https://example.com/users/bob","object":"` + actor + `"}}}`)
		require.ErrorIs(t, p.Process(body), boom, "フォロー解除を取りこぼしている")
	})

	// **not-found は ack のまま。** 居ない follower の Undo(Accept) を retry に
	// 倒すと、ごく普通の activity が dead letter に積まれる。
	t.Run("Undo(Accept) の follower が居ないのは ack", func(t *testing.T) {
		p, _, _, _ := newProcessor(t, aliceActor)
		const actor = "https://remote.example/users/alice"
		body := []byte(`{"type":"Undo","actor":"` + actor + `","object":{"type":"Accept","actor":"` + actor + `",` +
			`"object":{"type":"Follow","actor":"https://example.com/users/ghost","object":"` + actor + `"}}}`)
		require.NoError(t, p.Process(body), "not-found を retry に倒している")
	})

	// pin の重複判定。**「まだ無い」に倒すと、既にあるものを入れるために
	// 別のピンを外すことになる。**
	t.Run("pin の重複判定が引けない", func(t *testing.T) {
		p, repo, noteRepo, piningRepo := newProcessorWithPinning(t)
		aliceID := resolveAliceAndSetFeatured(t, p, repo)
		noteURI := "https://remote.example/notes/n1"
		noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: aliceID, URI: &noteURI}
		piningRepo.FindErr = boom
		body := []byte(`{"type":"Add","actor":"https://remote.example/users/alice",` +
			`"object":"https://remote.example/notes/n1",` +
			`"target":"https://remote.example/users/alice/collections/featured"}`)
		require.ErrorIs(t, p.Process(body), boom, "重複判定を素通りしている")
	})
}

// **ingest の lookup 失敗も ack しない** (#3121)。
//
// ここが厄介なのは、**ack すると URI が DB に入る**こと — retry は dedup に
// ヒットするので、落ちた紐付け (返信先 / 投票) は**二度と直らない**。
func TestProcess_IngestLookupFailuresPropagate(t *testing.T) {
	boom := errors.New("connection refused")

	// dedup。**「まだ無い」に倒すと再 fetch して INSERT へ進む。**
	t.Run("dedup の lookup が引けない", func(t *testing.T) {
		p, _, _, noteRepo := newProcessor(t, aliceActor)
		noteRepo.FindByURIErr = boom
		body := []byte(`{"type":"Create","actor":"https://remote.example/users/alice",` +
			`"object":{"id":"https://remote.example/notes/n1","type":"Note",` +
			`"attributedTo":"https://remote.example/users/alice","content":"hi"}}`)
		require.ErrorIs(t, p.Process(body), boom, "dedup を素通りしている")
		assert.Empty(t, noteRepo.Notes, "引けないまま INSERT へ進んでいる")
	})

	// 返信先。**ack すると ReplyID=nil のまま確定し、スレッド接続が失われる。**
	t.Run("返信先の lookup が引けない", func(t *testing.T) {
		env := newFullProcessor(t, aliceActor)
		env.noteRepo.Notes["t1"] = &model.Note{ID: "t1", UserID: "bob", Visibility: model.NoteVisibilityPublic}
		env.userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
		// dedup は not-found で通し、返信先の FindByID だけ落とす。
		env.noteRepo.FindErr = boom
		body := []byte(`{"type":"Create","actor":"https://remote.example/users/alice",` +
			`"object":{"id":"https://remote.example/notes/r1","type":"Note",` +
			`"attributedTo":"https://remote.example/users/alice","content":"re",` +
			`"inReplyTo":"https://example.com/notes/t1"}}`)
		require.ErrorIs(t, env.processor.Process(body), boom, "返信先の解決を素通りしている")
		// **harm も固定する。** error を返しつつ保存もする実装では、
		// `note.uri` が DB に入るので retry が dedup にヒットして直らない。
		_, ferr := env.noteRepo.FindByURI("https://remote.example/notes/r1")
		assert.True(t, repository.IsNotFound(ferr), "返信先が空のまま note を確定させている")
	})
}

// --- Question/Poll ---

func TestProcess_CreateQuestion(t *testing.T) {
	_, repo, _, noteRepo := newProcessor(t, aliceActor)
	// resolverにpollRepoを注入するためresolverへのアクセスが必要
	// newProcessorで作成されたresolverにSetPollRepoできないため、
	// processor経由でresolverを取得する方法がない。代わりにfull setup。
	pollRepo := testutil.NewMockPollRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen2, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen2)
	resolver.SetPollRepo(pollRepo)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen2)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Question",
			"id": "https://remote.example/notes/q1",
			"attributedTo": "https://remote.example/users/alice",
			"content": "Best language?",
			"oneOf": [
				{"type": "Note", "name": "Go", "replies": {"type": "Collection", "totalItems": 5}},
				{"type": "Note", "name": "Rust", "replies": {"type": "Collection", "totalItems": 3}}
			],
			"endTime": "2026-12-31T23:59:59Z",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	// ノートが作成されていること
	assert.True(t, len(noteRepo.Notes) > 0)
	// Pollが作成されていること
	assert.Len(t, pollRepo.Polls, 1)
	for _, poll := range pollRepo.Polls {
		assert.False(t, poll.Multiple) // oneOf → single choice
		assert.Len(t, poll.Choices, 2)
		assert.Equal(t, "Go", poll.Choices[0])
		assert.Equal(t, "Rust", poll.Choices[1])
	}
}

func TestProcess_CreateQuestionAnyOf(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen2, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen2)
	resolver.SetPollRepo(pollRepo)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen2)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Question",
			"id": "https://remote.example/notes/q2",
			"attributedTo": "https://remote.example/users/alice",
			"content": "Pick all that apply",
			"anyOf": [
				{"type": "Note", "name": "A"},
				{"type": "Note", "name": "B"},
				{"type": "Note", "name": "C"}
			],
			"closed": "2026-12-31T23:59:59Z",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Len(t, pollRepo.Polls, 1)
	for _, poll := range pollRepo.Polls {
		assert.True(t, poll.Multiple) // anyOf → multiple choice
		assert.Len(t, poll.Choices, 3)
		assert.NotNil(t, poll.ExpiresAt) // closedから設定
	}
}

// --- ExtractLocalUserID ---

func TestExtractLocalUserID(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)

	assert.Equal(t, "abc123", resolver.ExtractLocalUserID("https://example.com/users/abc123"))
	assert.Equal(t, "", resolver.ExtractLocalUserID("https://remote.example/users/abc123"))
	assert.Equal(t, "", resolver.ExtractLocalUserID(""))
}

func TestProcess_FlagWithoutRepo(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": ["https://example.com/users/bob"]
	}`)
	assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
}

// fakeRelayMarker records MarkAccepted/MarkRejected calls for assertion and
// serves the relay rows that the ownership check looks up.
type fakeRelayMarker struct {
	relays   map[string]*model.Relay
	accepted []string
	rejected []string
	lookups  []string
	err      error
	findErr  error
}

// newFakeRelayMarker returns a marker holding a single relay row whose inbox
// is inbox (empty inbox = no row at all, so FindByID fails).
func newFakeRelayMarker(id, inbox string) *fakeRelayMarker {
	m := &fakeRelayMarker{relays: map[string]*model.Relay{}}
	if inbox != "" {
		m.relays[id] = &model.Relay{ID: id, Inbox: inbox, Status: "requesting"}
	}
	return m
}

func (f *fakeRelayMarker) MarkAccepted(_ context.Context, id string) error {
	f.accepted = append(f.accepted, id)
	return f.err
}

func (f *fakeRelayMarker) MarkRejected(_ context.Context, id string) error {
	f.rejected = append(f.rejected, id)
	return f.err
}

func (f *fakeRelayMarker) FindByID(_ context.Context, id string) (*model.Relay, error) {
	f.lookups = append(f.lookups, id)
	if f.findErr != nil {
		return nil, f.findErr
	}
	rel, ok := f.relays[id]
	if !ok {
		return nil, errors.New("no such relay")
	}
	return rel, nil
}

// relayAcceptBody builds an Accept(Follow) whose inner id is activityID,
// signed by actor.
func relayAcceptBody(kind, actor, activityID string) []byte {
	return []byte(`{
		"type": "` + kind + `",
		"actor": "` + actor + `",
		"object": {
			"id": "` + activityID + `",
			"type": "Follow",
			"actor": "https://example.com/users/relay.actor",
			"object": "https://www.w3.org/ns/activitystreams#Public"
		}
	}`)
}

func TestProcess_AcceptFollowRelay_MarksAccepted(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	marker := newFakeRelayMarker("rel123", "https://relay.example/inbox")
	p.SetRelayMarker(marker)

	// 送信元は relay 自身 (inbox と同じ host)。
	body := relayAcceptBody("Accept", "https://relay.example/actor",
		"https://example.com/activities/follow-relay/rel123")
	require.NoError(t, p.Process(body))
	assert.Equal(t, []string{"rel123"}, marker.accepted)
	assert.Empty(t, marker.rejected)
}

func TestProcess_RejectFollowRelay_MarksRejected(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	marker := newFakeRelayMarker("rel456", "https://relay.example/inbox")
	p.SetRelayMarker(marker)

	body := relayAcceptBody("Reject", "https://relay.example/actor",
		"https://example.com/activities/follow-relay/rel456")
	require.NoError(t, p.Process(body))
	assert.Equal(t, []string{"rel456"}, marker.rejected)
	assert.Empty(t, marker.accepted)
}

// TestProcess_FollowRelay_HostNormalizedMatch は host 比較が punycode / 大小文字を
// 揃えてから行われることを固定する (素の文字列比較へ退行すると落ちる)。
func TestProcess_FollowRelay_HostNormalizedMatch(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	marker := newFakeRelayMarker("rel1", "https://xn--eckve.example/inbox")
	p.SetRelayMarker(marker)

	body := relayAcceptBody("Accept", "https://パイ.Example/actor",
		"https://example.com/activities/follow-relay/rel1")
	require.NoError(t, p.Process(body))
	assert.Equal(t, []string{"rel1"}, marker.accepted)
}

// TestProcess_AcceptFollowRelay_ForeignActorDropped は「署名が通る任意の actor が
// relay を accepted に倒せる」欠陥 (upstream ApInboxService.ts:250 と同型) を固定する。
// accepted に倒されると DeliverToAccepted が未承認の inbox へ全公開ノートを流し始める。
func TestProcess_AcceptFollowRelay_ForeignActorDropped(t *testing.T) {
	for _, kind := range []string{"Accept", "Reject"} {
		t.Run(kind, func(t *testing.T) {
			p, _, _, _ := newProcessor(t, aliceActor)
			p.SetLocalBaseURL("https://example.com")
			marker := newFakeRelayMarker("rel123", "https://relay.example/inbox")
			p.SetRelayMarker(marker)

			// relay とは無関係な remote actor。
			body := relayAcceptBody(kind, "https://remote.example/users/alice",
				"https://example.com/activities/follow-relay/rel123")
			// drop は ack 扱い (retry させない)。
			require.NoError(t, p.Process(body))
			assert.Empty(t, marker.accepted)
			assert.Empty(t, marker.rejected)
			// 行の lookup 自体は行われている (= 検証を通って落ちた)。
			assert.Equal(t, []string{"rel123"}, marker.lookups)
		})
	}
}

// TestProcess_AcceptFollowRelay_ForeignActivityIDIgnored は inner.id の host を
// 見ていなかった欠陥を固定する。他ホストの follow-relay URI は relay 行を
// 一切触らせない (通常の Accept(Follow) 経路へ落ちる)。
func TestProcess_AcceptFollowRelay_ForeignActivityIDIgnored(t *testing.T) {
	for _, activityID := range []string{
		"https://evil.test/activities/follow-relay/rel123",
		// prefix を伸ばした紛らわしい host も自ホストではない。
		"https://example.com.evil.test/activities/follow-relay/rel123",
		// 自ホスト配下でも path が違えば follow-relay URI ではない。
		"https://example.com/activities/follow-relay/rel123/extra",
	} {
		t.Run(activityID, func(t *testing.T) {
			p, _, _, _ := newProcessor(t, aliceActor)
			p.SetLocalBaseURL("https://example.com")
			marker := newFakeRelayMarker("rel123", "https://relay.example/inbox")
			p.SetRelayMarker(marker)

			body := relayAcceptBody("Accept", "https://remote.example/users/alice", activityID)
			require.NoError(t, p.Process(body))
			assert.Empty(t, marker.accepted)
			assert.Empty(t, marker.rejected)
			// relay 行を引きにすら行かない。
			assert.Empty(t, marker.lookups)
		})
	}
}

// TestProcess_AcceptFollowRelay_UnparsableHostDropped は host を取り出せない URI
// (壊れた actor / scheme 無しの inbox / inbox 未設定の行) を fail-closed で
// 落とすことを固定する。
func TestProcess_AcceptFollowRelay_UnparsableHostDropped(t *testing.T) {
	cases := map[string]struct{ actor, inbox string }{
		"malformed actor":  {actor: "not-a-uri", inbox: "https://relay.example/inbox"},
		"schemeless inbox": {actor: "https://relay.example/actor", inbox: "relay.example/inbox"},
		"empty inbox":      {actor: "https://relay.example/actor", inbox: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, _, _, _ := newProcessor(t, aliceActor)
			p.SetLocalBaseURL("https://example.com")
			// inbox が空の行も「存在はする」状態として作る。
			marker := &fakeRelayMarker{relays: map[string]*model.Relay{
				"rel123": {ID: "rel123", Inbox: tc.inbox, Status: "requesting"},
			}}
			p.SetRelayMarker(marker)

			body := relayAcceptBody("Accept", tc.actor,
				"https://example.com/activities/follow-relay/rel123")
			require.NoError(t, p.Process(body))
			assert.Empty(t, marker.accepted)
		})
	}
}

// TestProcess_AcceptFollowRelay_UnknownRelayDropped は存在しない relay id を
// 指す Accept が status を触らないことを固定する。
func TestProcess_AcceptFollowRelay_UnknownRelayDropped(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	marker := newFakeRelayMarker("rel123", "")
	p.SetRelayMarker(marker)

	body := relayAcceptBody("Accept", "https://relay.example/actor",
		"https://example.com/activities/follow-relay/rel123")
	require.NoError(t, p.Process(body))
	assert.Empty(t, marker.accepted)
	assert.Equal(t, []string{"rel123"}, marker.lookups)
}

// TestProcess_AcceptFollowRelay_LookupErrorDropped は relay 行の取得が失敗した
// ときに status を書き換えないこと (fail-closed) を固定する。
func TestProcess_AcceptFollowRelay_LookupErrorDropped(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	p.SetLocalBaseURL("https://example.com")
	marker := newFakeRelayMarker("rel123", "https://relay.example/inbox")
	marker.findErr = errors.New("db down")
	p.SetRelayMarker(marker)

	body := relayAcceptBody("Accept", "https://relay.example/actor",
		"https://example.com/activities/follow-relay/rel123")
	require.NoError(t, p.Process(body))
	assert.Empty(t, marker.accepted)
}

// TestProcess_AcceptFollowRelay_NoLocalBaseURLDropped は localBaseURL が
// 未配線のときに fail-closed になることを固定する (自ホスト判定ができない以上、
// どの follow-relay URI も信用できない)。
func TestProcess_AcceptFollowRelay_NoLocalBaseURLDropped(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	// inbox host と actor host は一致している = localBaseURL さえ配線されていれば
	// 通るはずの activity。それでも触らせない。
	marker := newFakeRelayMarker("rel123", "https://remote.example/inbox")
	p.SetRelayMarker(marker)

	body := relayAcceptBody("Accept", "https://remote.example/users/alice",
		"https://example.com/activities/follow-relay/rel123")
	require.NoError(t, p.Process(body))
	assert.Empty(t, marker.accepted)
	assert.Empty(t, marker.lookups)
}

func TestProcess_AcceptNonRelay_IgnoresMarker(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	marker := &fakeRelayMarker{}
	p.SetRelayMarker(marker)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/activities/follow/xyz",
			"type": "Follow",
			"actor": "https://example.com/users/bob",
			"object": "https://remote.example/users/alice"
		}
	}`)
	// フォローリクエストが無いので内部的には ErrRequestNotFound を飲み込む
	require.NoError(t, p.Process(body))
	// relay marker は呼ばれない
	assert.Empty(t, marker.accepted)
}

// --- Chat federation (Misskey:ChatMessage) ---

type stubChatReceiver struct {
	called   int
	lastURI  string
	lastFrom *model.User
	lastTo   string
	lastText string
	// lastMFM は相手が併記した MFM の原文 (`source` / `_misskey_content`)。
	lastMFM   string
	returnErr error
}

func (s *stubChatReceiver) CreateMessageViaAP(_ context.Context, uri string, fromUser *model.User, toUserID, text, mfmSource string) (*model.ChatMessage, error) {
	s.called++
	s.lastURI = uri
	s.lastFrom = fromUser
	s.lastTo = toUserID
	s.lastText = text
	s.lastMFM = mfmSource
	if s.returnErr != nil {
		return nil, s.returnErr
	}
	return &model.ChatMessage{ID: "cm_ap_1"}, nil
}

func TestProcess_ChatMessage_HappyPath(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	// ローカルユーザーはURI==nil (本番と同じ)。ExtractLocalUserID→FindByIDで解決。
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)

	body := []byte(`{
		"id": "https://remote.example/chat-messages/1",
		"type": "Misskey:ChatMessage",
		"actor": "https://remote.example/users/alice",
		"attributedTo": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"content": "hello via AP"
	}`)
	require.NoError(t, p.Process(body))
	assert.Equal(t, 1, chatSvc.called)
	assert.Equal(t, "https://remote.example/chat-messages/1", chatSvc.lastURI)
	assert.Equal(t, "bob", chatSvc.lastTo)
	assert.Equal(t, "hello via AP", chatSvc.lastText)
}

func TestProcess_ChatMessage_NoChatService(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	body := []byte(`{
		"id": "https://remote.example/chat-messages/2",
		"type": "Misskey:ChatMessage",
		"actor": "https://remote.example/users/alice",
		"attributedTo": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"content": "hi"
	}`)
	err := p.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestProcess_ChatMessage_ActorMismatch(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)
	body := []byte(`{
		"id": "https://remote.example/chat-messages/3",
		"type": "Misskey:ChatMessage",
		"actor": "https://remote.example/users/alice",
		"attributedTo": "https://remote.example/users/mallory",
		"to": "https://example.com/users/bob",
		"content": "spoof"
	}`)
	err := p.Process(body)
	assert.Error(t, err)
	assert.Equal(t, 0, chatSvc.called)
}

func TestProcess_ChatMessage_MissingTo(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)
	body := []byte(`{
		"id": "https://remote.example/chat-messages/4",
		"type": "Misskey:ChatMessage",
		"actor": "https://remote.example/users/alice",
		"attributedTo": "https://remote.example/users/alice",
		"content": "no to"
	}`)
	err := p.Process(body)
	assert.Error(t, err)
}

// --- Chat federation (CherryPick: Create + Note(_misskey_talk:true), #692) ---
//
// CherryPick / レガシー Misskey の chat 連合 wire format。Create に Note を
// 包んで `_misskey_talk: true` flag を立てる形。to は string[] で、handleCreate
// が IngestNote の前に分岐して chatService にルートする。

func TestProcess_CreateChat_HappyPath_ToArray(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)

	body := []byte(`{
		"id": "https://remote.example/activities/create-chat-1",
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat-messages/cm1",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "yo via cherrypick",
			"to": ["https://example.com/users/bob"],
			"_misskey_talk": true
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Equal(t, 1, chatSvc.called)
	assert.Equal(t, "https://remote.example/chat-messages/cm1", chatSvc.lastURI)
	assert.Equal(t, "bob", chatSvc.lastTo)
	assert.Equal(t, "yo via cherrypick", chatSvc.lastText)
}

func TestProcess_CreateChat_HappyPath_ToString(t *testing.T) {
	// to が array でなく単一 string でも受け付ける (legacy 実装互換)。
	p, repo, _, _ := newProcessor(t, aliceActor)
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)

	body := []byte(`{
		"id": "https://remote.example/activities/create-chat-2",
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat-messages/cm2",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "single recipient",
			"to": "https://example.com/users/bob",
			"_misskey_talk": true
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.Equal(t, 1, chatSvc.called)
	assert.Equal(t, "single recipient", chatSvc.lastText)
}

func TestProcess_CreateChat_FlagFalseFallsThroughToIngest(t *testing.T) {
	// `_misskey_talk` flag が false / 無い場合は chat 経路に分岐せず通常の
	// IngestNote 処理に流れる。chatService は呼ばれないことだけを assert。
	// IngestNote の実 persist 動作は newFullProcessor 経由の既存
	// TestProcess_CreateNote でカバーしている。
	p, _, _, _ := newProcessor(t, aliceActor)
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)
	body := []byte(`{
		"id": "https://remote.example/activities/create-note-1",
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/n1",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "regular post",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	_ = p.Process(body)
	assert.Equal(t, 0, chatSvc.called, "chat service must NOT be hit for non-chat notes")
}

// #692: handleChatCreate / readRecipientURI のエッジケース。

func TestProcess_CreateChat_EmptyTo(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat-messages/cm-empty",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "no recipient",
			"to": "",
			"_misskey_talk": true
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
	assert.Equal(t, 0, chatSvc.called)
}

func TestProcess_CreateChat_ToNumber_Rejected(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat-messages/cm-num",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "weird",
			"to": 42,
			"_misskey_talk": true
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
	assert.Equal(t, 0, chatSvc.called)
}

func TestProcess_CreateChat_EmptyArrayTo(t *testing.T) {
	p, _, _, _ := newProcessor(t, aliceActor)
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat-messages/cm-empty-arr",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "no recipient",
			"to": [""],
			"_misskey_talk": true
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
	assert.Equal(t, 0, chatSvc.called)
}

func TestProcess_CreateChat_RecipientNotFound(t *testing.T) {
	// FindByURI が err、ExtractLocalUserID も local URI でない → resolve 失敗。
	p, _, _, _ := newProcessor(t, aliceActor)
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat-messages/cm-no-recip",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "x",
			"to": "https://other.example/users/unknown",
			"_misskey_talk": true
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
	assert.Equal(t, 0, chatSvc.called)
}

func TestProcess_CreateChat_RecipientNotLocal(t *testing.T) {
	// recipient が remote 解決されると loopback delivery として拒絶する。
	p, repo, _, _ := newProcessor(t, aliceActor)
	remoteURI := "https://remote2.example/users/charlie"
	remoteHost := "remote2.example"
	repo.Users["charlie"] = &model.User{ID: "charlie", Username: "charlie", URI: &remoteURI, Host: &remoteHost}
	chatSvc := &stubChatReceiver{}
	p.SetChatService(chatSvc)

	body := []byte(`{
		"id": "https://remote.example/activities/create-chat-3",
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/chat-messages/cm3",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "wrong recipient",
			"to": ["https://remote2.example/users/charlie"],
			"_misskey_talk": true
		}
	}`)
	err := p.Process(body)
	assert.Error(t, err)
	assert.Equal(t, 0, chatSvc.called)
}

// --- FanoutHook ---

// fakeFanoutHook records OnNoteCreated calls for assertion. #569 で
// federation processor の hook が async (safeGo 経由) になったので、
// 並行 append safety のため mu を持つ。caller 側は assert.Eventually
// 等で hook 反映を待つ。
type fakeFanoutHook struct {
	mu    sync.Mutex
	calls []fakeFanoutCall
}

type fakeFanoutCall struct {
	noteID   string
	authorID string
	note     *model.Note
}

func (f *fakeFanoutHook) OnNoteCreated(note *model.Note, author *model.User) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeFanoutCall{noteID: note.ID, authorID: author.ID, note: note})
	f.mu.Unlock()
}

func (f *fakeFanoutHook) snapshot() []fakeFanoutCall {
	f.mu.Lock()
	out := make([]fakeFanoutCall, len(f.calls))
	copy(out, f.calls)
	f.mu.Unlock()
	return out
}

func TestProcess_CreateCallsFanoutHook(t *testing.T) {
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	hook := &fakeFanoutHook{}
	p.SetFanoutHook(hook)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/n1",
			"attributedTo": "https://remote.example/users/alice",
			"content": "Hello from remote",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	// ノートが取り込まれたこと
	assert.True(t, len(noteRepo.Notes) > 0)
	// fanoutHookが呼ばれたこと (#569 で async になったので Eventually で待つ)
	require.Eventually(t, func() bool {
		return len(hook.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	calls := hook.snapshot()
	assert.NotEmpty(t, calls[0].noteID)
	assert.NotEmpty(t, calls[0].authorID)
}

func TestProcess_AnnounceCallsFanoutHook(t *testing.T) {
	p, repo, _, noteRepo := newProcessor(t, aliceActor)
	hook := &fakeFanoutHook{}
	p.SetFanoutHook(hook)

	// Announceのターゲットとなるローカルノートを用意
	noteURI := "https://example.com/notes/local1"
	noteRepo.Notes["local1"] = &model.Note{
		ID:         "local1",
		UserID:     "bob",
		URI:        &noteURI,
		Visibility: model.NoteVisibilityPublic,
	}
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/activities/announce1",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/local1"
	}`)
	require.NoError(t, p.Process(body))
	// fanoutHookが呼ばれたこと (#1158 で async になったので Eventually で待つ)
	require.Eventually(t, func() bool {
		return len(hook.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	calls := hook.snapshot()
	// Announce で作成された renote の ID が渡される
	assert.NotEqual(t, "local1", calls[0].noteID)
	// hydrateNoteForFanout により Renote relation が preload された状態で
	// 渡される (#416: streaming payload で renote が null にならない保証)。
	require.NotNil(t, calls[0].note)
	require.NotNil(t, calls[0].note.Renote, "Renote relation must be hydrated before fanout")
	assert.Equal(t, "local1", calls[0].note.Renote.ID)
}

func TestProcess_CreateWithoutFanoutHook(t *testing.T) {
	// fanoutHookがnilでもCreate処理はpanicしない
	p, _, _, noteRepo := newProcessor(t, aliceActor)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/n2",
			"attributedTo": "https://remote.example/users/alice",
			"content": "No hook",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.True(t, len(noteRepo.Notes) > 0)
}

func TestProcess_AnnounceWithoutFanoutHook(t *testing.T) {
	// fanoutHookがnilでもAnnounce処理はpanicしない
	p, repo, _, noteRepo := newProcessor(t, aliceActor)

	noteURI := "https://example.com/notes/local2"
	noteRepo.Notes["local2"] = &model.Note{
		ID:         "local2",
		UserID:     "bob",
		URI:        &noteURI,
		Visibility: model.NoteVisibilityPublic,
	}
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/activities/announce2",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/local2"
	}`)
	require.NoError(t, p.Process(body))
}

// --- NotificationHook (#415) ---

// fakeNotificationHook records OnNoteCreated calls.
type fakeNotificationHook struct {
	mu      sync.Mutex
	calls   []fakeNotificationCall
	deleted []string
}

type fakeNotificationCall struct {
	noteID       string
	authorID     string
	replyTarget  *model.Note
	renoteTarget *model.Note
}

func (f *fakeNotificationHook) OnNoteCreated(note *model.Note, author *model.User, replyTarget, renoteTarget *model.Note) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeNotificationCall{
		noteID:       note.ID,
		authorID:     author.ID,
		replyTarget:  replyTarget,
		renoteTarget: renoteTarget,
	})
	f.mu.Unlock()
}

func (f *fakeNotificationHook) OnNoteDeleted(note *model.Note) {
	f.mu.Lock()
	renoteUser := ""
	if note.RenoteUserID != nil {
		renoteUser = *note.RenoteUserID
	}
	f.deleted = append(f.deleted, note.ID+" "+note.UserID+"->"+renoteUser)
	f.mu.Unlock()
}

func (f *fakeNotificationHook) snapshot() []fakeNotificationCall {
	f.mu.Lock()
	out := make([]fakeNotificationCall, len(f.calls))
	copy(out, f.calls)
	f.mu.Unlock()
	return out
}

func TestProcess_AnnounceCallsNotificationHook(t *testing.T) {
	// remote user が local note を renote すると元投稿者 (bob) への renote
	// 通知が生成される経路を確認する (#415)。
	p, repo, _, noteRepo := newProcessor(t, aliceActor)
	hook := &fakeNotificationHook{}
	p.SetNotificationHook(hook)

	noteURI := "https://example.com/notes/local1"
	noteRepo.Notes["local1"] = &model.Note{
		ID:         "local1",
		UserID:     "bob",
		URI:        &noteURI,
		Visibility: model.NoteVisibilityPublic,
	}
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/activities/announce-notify",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/local1"
	}`)
	require.NoError(t, p.Process(body))

	// notificationHook が呼ばれたこと (#1158 で async になったので Eventually で待つ)
	require.Eventually(t, func() bool {
		return len(hook.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	calls := hook.snapshot()
	// renoteTarget に local note が渡る。hook 内部の notifyLocalUser が
	// renoteTarget.UserID == "bob" を見て bob へ通知を送る。
	require.NotNil(t, calls[0].renoteTarget)
	assert.Equal(t, "local1", calls[0].renoteTarget.ID)
	assert.Equal(t, "bob", calls[0].renoteTarget.UserID)
	assert.Nil(t, calls[0].replyTarget, "Announce は reply ではない")
}

func TestProcess_CreateCallsNotificationHook(t *testing.T) {
	// remote user の普通の Note 作成では notificationHook を通過させるが、
	// reply / renote target が無いので hook 側で通知は発火しない。呼び出しが
	// 発生することだけを確認する (reply target 検証は別途)。
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	hook := &fakeNotificationHook{}
	p.SetNotificationHook(hook)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/n-notify",
			"attributedTo": "https://remote.example/users/alice",
			"content": "Hello from remote",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))

	assert.True(t, len(noteRepo.Notes) > 0)
	require.Eventually(t, func() bool {
		return len(hook.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	calls := hook.snapshot()
	assert.Nil(t, calls[0].replyTarget)
	assert.Nil(t, calls[0].renoteTarget)
}

func TestProcess_CreateReplyPassesReplyTarget(t *testing.T) {
	// Remote reply to a local note → hook に replyTarget として local note が
	// 渡る (#415 reply 通知経路)。
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	hook := &fakeNotificationHook{}
	p.SetNotificationHook(hook)

	localURI := "https://example.com/notes/local-parent"
	noteRepo.Notes["local-parent"] = &model.Note{
		ID:         "local-parent",
		UserID:     "bob",
		URI:        &localURI,
		Visibility: model.NoteVisibilityPublic,
	}

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/reply1",
			"attributedTo": "https://remote.example/users/alice",
			"inReplyTo": "https://example.com/notes/local-parent",
			"content": "remote reply",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))

	require.Eventually(t, func() bool {
		return len(hook.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	calls := hook.snapshot()
	require.NotNil(t, calls[0].replyTarget, "replyTarget must be set for inReplyTo=local")
	assert.Equal(t, "local-parent", calls[0].replyTarget.ID)
}

// #1931: AP 経由で取り込んだ reply note も threadId を thread root に揃える
// (local create path #1928 と同 logic)。これが無いと remote thread の deep muting が効かない。
func TestProcess_CreateReplySetsThreadID(t *testing.T) {
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	localURI := "https://example.com/notes/root-parent"
	// root-parent は threadId 無し (= thread root)。
	noteRepo.Notes["root-parent"] = &model.Note{
		ID: "root-parent", UserID: "bob", URI: &localURI, Visibility: model.NoteVisibilityPublic,
	}
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/reply1",
			"attributedTo": "https://remote.example/users/alice",
			"inReplyTo": "https://example.com/notes/root-parent",
			"content": "remote reply",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	require.Eventually(t, func() bool {
		n, err := noteRepo.FindByURI("https://remote.example/notes/reply1")
		return err == nil && n != nil
	}, time.Second, 10*time.Millisecond)
	created, err := noteRepo.FindByURI("https://remote.example/notes/reply1")
	require.NoError(t, err)
	require.NotNil(t, created.ThreadID)
	assert.Equal(t, "root-parent", *created.ThreadID, "AP reply の threadId は root.id を指す")
}

// #1931: 深い AP reply (親が既に threadId を持つ) は root threadId を継承する。
func TestProcess_CreateReplyInheritsThreadID(t *testing.T) {
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	rootThread := "root"
	localURI := "https://example.com/notes/mid-parent"
	noteRepo.Notes["mid-parent"] = &model.Note{
		ID: "mid-parent", UserID: "bob", URI: &localURI, Visibility: model.NoteVisibilityPublic, ThreadID: &rootThread,
	}
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/reply2",
			"attributedTo": "https://remote.example/users/alice",
			"inReplyTo": "https://example.com/notes/mid-parent",
			"content": "deep remote reply",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	require.Eventually(t, func() bool {
		n, err := noteRepo.FindByURI("https://remote.example/notes/reply2")
		return err == nil && n != nil
	}, time.Second, 10*time.Millisecond)
	created, err := noteRepo.FindByURI("https://remote.example/notes/reply2")
	require.NoError(t, err)
	require.NotNil(t, created.ThreadID)
	assert.Equal(t, "root", *created.ThreadID, "深い AP reply は root threadId を継承")
}

func TestProcess_CreateWithoutNotificationHook(t *testing.T) {
	// notificationHook が nil でも Create は panic しない。
	p, _, _, noteRepo := newProcessor(t, aliceActor)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/no-notif",
			"attributedTo": "https://remote.example/users/alice",
			"content": "No notif",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.True(t, len(noteRepo.Notes) > 0)
}

func TestProcess_AnnounceWithoutNotificationHook(t *testing.T) {
	// notificationHook が nil でも Announce は panic しない。
	p, repo, _, noteRepo := newProcessor(t, aliceActor)

	noteURI := "https://example.com/notes/local3"
	noteRepo.Notes["local3"] = &model.Note{
		ID:         "local3",
		UserID:     "bob",
		URI:        &noteURI,
		Visibility: model.NoteVisibilityPublic,
	}
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/activities/announce3",
		"actor": "https://remote.example/users/alice",
		"object": "https://example.com/notes/local3"
	}`)
	require.NoError(t, p.Process(body))
}

// findIngestedRemoteNote returns the single non-seed note ingested by a Create
// Process call. Seed notes set up by a test live under their own keys; the
// freshly ingested one carries the given remote URI.
func findIngestedRemoteNote(t *testing.T, noteRepo *testutil.MockNoteRepository, uri string) *model.Note {
	t.Helper()
	n, err := noteRepo.FindByURI(uri)
	require.NoError(t, err, "ingested note %s should be persisted", uri)
	return n
}

// TestProcess_CreateNote_AudienceOnActivityDerivesVisibility seals the #1560
// fix: a Create whose to/cc audience lives on the activity (not on the Note
// object) must still derive the correct visibility. Before the fix, the Note
// object's empty to/cc yielded "specified" regardless of the activity audience.
func TestProcess_CreateNote_AudienceOnActivityDerivesVisibility(t *testing.T) {
	cases := []struct {
		name       string
		activityTo string
		activityCC string
		want       model.NoteVisibility
	}{
		{
			name:       "public_from_activity_to",
			activityTo: `["https://www.w3.org/ns/activitystreams#Public"]`,
			activityCC: `["https://remote.example/users/alice/followers"]`,
			want:       model.NoteVisibilityPublic,
		},
		{
			name:       "home_from_activity_to_cc",
			activityTo: `["https://remote.example/users/alice/followers"]`,
			activityCC: `["https://www.w3.org/ns/activitystreams#Public"]`,
			want:       model.NoteVisibilityHome,
		},
		{
			name:       "followers_from_activity_to",
			activityTo: `["https://remote.example/users/alice/followers"]`,
			activityCC: `[]`,
			want:       model.NoteVisibilityFollowers,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _, noteRepo := newProcessor(t, aliceActor)
			noteURI := "https://remote.example/notes/aud" + string(rune('a'+i))
			// Note object 自身には to/cc を一切載せない。audience は activity 側のみ。
			body := []byte(`{
				"type": "Create",
				"actor": "https://remote.example/users/alice",
				"to": ` + tc.activityTo + `,
				"cc": ` + tc.activityCC + `,
				"object": {
					"type": "Note",
					"id": "` + noteURI + `",
					"attributedTo": "https://remote.example/users/alice",
					"content": "hi"
				}
			}`)
			require.NoError(t, p.Process(body))
			note := findIngestedRemoteNote(t, noteRepo, noteURI)
			assert.Equal(t, tc.want, note.Visibility)
		})
	}
}

// TestProcess_CreateNote_SpecifiedVisibleUserIDsFromActivityTo confirms that
// for a "specified" note whose recipient URI is carried only on the activity
// to, the unioned audience drives visibleUserIds resolution (#1560).
func TestProcess_CreateNote_SpecifiedVisibleUserIDsFromActivityTo(t *testing.T) {
	p, repo, _, noteRepo := newProcessor(t, aliceActor)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	noteURI := "https://remote.example/notes/spec1"
	// to に local user URI のみ (Public/followers 無し) → specified。
	// recipient URI も activity 側にしか無い。
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"to": ["https://example.com/users/bob"],
		"object": {
			"type": "Note",
			"id": "` + noteURI + `",
			"attributedTo": "https://remote.example/users/alice",
			"content": "dm"
		}
	}`)
	require.NoError(t, p.Process(body))
	note := findIngestedRemoteNote(t, noteRepo, noteURI)
	assert.Equal(t, model.NoteVisibilitySpecified, note.Visibility)
	assert.Equal(t, []string{"bob"}, []string(note.VisibleUserIDs))
}

// TestProcess_CreateNote_AttributedToFilledFromActor verifies that a Note object
// lacking attributedTo is no longer rejected as invalid: the activity actor is
// used to fill it before ingest, matching upstream create() (#1560).
func TestProcess_CreateNote_AttributedToFilledFromActor(t *testing.T) {
	p, repo, _, noteRepo := newProcessor(t, aliceActor)
	noteURI := "https://remote.example/notes/noattr1"
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"object": {
			"type": "Note",
			"id": "` + noteURI + `",
			"content": "no attributedTo on object"
		}
	}`)
	require.NoError(t, p.Process(body))
	note := findIngestedRemoteNote(t, noteRepo, noteURI)
	// attributedTo を activity.actor から補ったので ingest は成功し、note は
	// 解決済み actor (alice, generated ID) に紐づく。
	require.NotEmpty(t, note.UserID)
	alice := repo.Users[note.UserID]
	require.NotNil(t, alice, "ingested note should belong to the resolved actor")
	assert.Equal(t, "alice", alice.Username)
	assert.Equal(t, model.NoteVisibilityPublic, note.Visibility)
}

// TestProcess_CreateNote_ObjectAudienceStillHonored is a non-regression guard:
// when the Note object already carries to/cc and the activity does not, the
// union is a no-op and visibility is unchanged (#1560).
func TestProcess_CreateNote_ObjectAudienceStillHonored(t *testing.T) {
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	noteURI := "https://remote.example/notes/objaud1"
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "` + noteURI + `",
			"attributedTo": "https://remote.example/users/alice",
			"to": ["https://www.w3.org/ns/activitystreams#Public"],
			"content": "object carries audience"
		}
	}`)
	require.NoError(t, p.Process(body))
	note := findIngestedRemoteNote(t, noteRepo, noteURI)
	assert.Equal(t, model.NoteVisibilityPublic, note.Visibility)
}

// #2106 L30: remote followee 宛の Undo(Follow) は skip される (handleFollow gate と対称)。
func TestProcess_UndoFollow_RemoteFolloweeSkipped(t *testing.T) {
	p, repo, _, _ := newProcessor(t, aliceActor)
	bobURI := "https://remote2.example/users/bob"
	bobHost := "remote2.example"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI, Host: &bobHost}

	undo := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://remote.example/users/alice",
			"object": "https://remote2.example/users/bob"
		}
	}`)
	require.NoError(t, p.Process(undo)) // remote followee なので skip (nil)
}

// #2106 L30: remote blockee 宛の Undo(Block) は skip される (handleBlock gate と対称)。
func TestProcess_UndoBlock_RemoteBlockeeSkipped(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	bobURI := "https://remote2.example/users/bob"
	bobHost := "remote2.example"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI, Host: &bobHost}

	undoBody := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Block",
			"actor": "https://remote.example/users/alice",
			"object": "https://remote2.example/users/bob"
		}
	}`)
	require.NoError(t, p.Process(undoBody)) // remote blockee なので skip (nil)
}

// upstream 2026.7.0 #17747: AP 受信の reply も reply 先より広い可視性には
// できない。upstream の ApNoteService は NoteCreateService.create 経由なので
// 同じクランプが掛かる。これが無いと「ローカルの followers 限定 note へ
// リモートが to:[Public] で返信」でローカル TL / 再配送に文脈が漏れる。
func TestProcess_CreateReplyClampsVisibilityToTarget(t *testing.T) {
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	localURI := "https://example.com/notes/followers-parent"
	noteRepo.Notes["followers-parent"] = &model.Note{
		ID: "followers-parent", UserID: "bob", URI: &localURI, Visibility: model.NoteVisibilityFollowers,
	}
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/reply-clamp",
			"attributedTo": "https://remote.example/users/alice",
			"inReplyTo": "https://example.com/notes/followers-parent",
			"content": "remote public reply",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	require.Eventually(t, func() bool {
		n, err := noteRepo.FindByURI("https://remote.example/notes/reply-clamp")
		return err == nil && n != nil
	}, time.Second, 10*time.Millisecond)
	created, err := noteRepo.FindByURI("https://remote.example/notes/reply-clamp")
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityFollowers, created.Visibility,
		"followers 限定 note への public reply は followers に降格される")
}

// #17747 clamp で specified に降格したリモート reply は、reply 先の作者を
// visibleUserIds に含める必要がある (apNote.To が Public だけだと宛先が 1 件も
// 解決できず、DM の当事者すら本文を参照できない note が残る)。
func TestProcess_CreateReplyClampedToSpecifiedKeepsTargetVisible(t *testing.T) {
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	localURI := "https://example.com/notes/specified-parent"
	noteRepo.Notes["specified-parent"] = &model.Note{
		ID: "specified-parent", UserID: "bob", URI: &localURI,
		Visibility: model.NoteVisibilitySpecified,
	}
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/reply-specified",
			"attributedTo": "https://remote.example/users/alice",
			"inReplyTo": "https://example.com/notes/specified-parent",
			"content": "remote reply to DM",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	require.Eventually(t, func() bool {
		n, err := noteRepo.FindByURI("https://remote.example/notes/reply-specified")
		return err == nil && n != nil
	}, time.Second, 10*time.Millisecond)
	created, err := noteRepo.FindByURI("https://remote.example/notes/reply-specified")
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, created.Visibility)
	assert.Contains(t, []string(created.VisibleUserIDs), "bob",
		"reply 先の作者が visibleUserIds に入っていないと当人が本文を読めない")
}

// 同じ Create(Note) が再配送されても通知は 1 度しか出ないこと。
//
// **通知は冪等ではない。** 呼ぶたびに新しい id で 1 件積まれるので、配送リトライや
// inbox / sharedInbox への二重投げがあると、受信者の通知一覧に同じメンションや
// DM が 2 件並ぶ。実運用のインスタンスでは日常的に起きる (実験環境では配送が
// 一度きりなので再現しない)。fanout は note id で冪等なので従来どおり発火する。
func TestProcess_CreateDoesNotRenotifyOnRedelivery(t *testing.T) {
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	hook := &fakeNotificationHook{}
	p.SetNotificationHook(hook)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/notes/n-redelivered",
			"attributedTo": "https://remote.example/users/alice",
			"content": "Hello again",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)

	require.NoError(t, p.Process(body))
	require.Eventually(t, func() bool {
		return len(hook.snapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	notesAfterFirst := len(noteRepo.Notes)

	// 同じ activity をもう一度受ける (= 配送リトライ / 二重投げ)。
	require.NoError(t, p.Process(body))

	// ノートは増えない (URI で dedup される)。
	assert.Equal(t, notesAfterFirst, len(noteRepo.Notes))
	// 通知も増えないこと。**ここが増えると一覧に同じ通知が 2 件並ぶ。**
	assert.Never(t, func() bool {
		return len(hook.snapshot()) > 1
	}, 200*time.Millisecond, 20*time.Millisecond)
}
