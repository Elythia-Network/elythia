package federation_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localActorBody is what our own /users/<id> endpoint would serve.
const localActorBody = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://example.com/users/9local",
	"type": "Person",
	"preferredUsername": "alice",
	"name": "Alice",
	"inbox": "https://example.com/users/9local/inbox",
	"publicKey": {
		"id": "https://example.com/users/9local#main-key",
		"owner": "https://example.com/users/9local",
		"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
	}
}`

func newLocalGuardResolver(t *testing.T, f federation.HTTPFetcher) (*federation.Resolver, *testutil.MockUserRepository, *stubInstanceTracker) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(userRepo, testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), f, idGen)
	tracker := &stubInstanceTracker{}
	r.SetInstanceTracker(tracker)
	return r, userRepo, tracker
}

// 自ホストの actor URI を解決させても「リモート扱いの」shadow 行を作らないこと。
//
// upstream は ApPersonService.createPerson で `cannot resolve local user` を
// 投げる。到達経路は**未認証**で、inbox の署名検証が keyId の base URL を
// ResolveActor に渡すのが署名検証より前なので、誰でも任意のローカル利用者の
// URI を送れる。作られると user / user_profile に加えて自ホストの instance 行と
// chart まで出来る。
func TestResolveActor_RejectsSelfHostURI(t *testing.T) {
	// 本番と同じく host binding が全部走る条件 (finalURL を返す fetcher)。
	f := &finalURLStubFetcher{body: []byte(localActorBody), finalURL: "https://example.com/users/9local"}
	r, userRepo, tracker := newLocalGuardResolver(t, f)

	_, err := r.ResolveActor("https://example.com/users/9local")
	require.Error(t, err)
	assert.ErrorIs(t, err, federation.ErrLocalActor)
	// isPermanentSkipError が拾える形であること (retry キューへ戻さない)。
	assert.ErrorIs(t, err, federation.ErrInvalidActor)
	assert.Empty(t, userRepo.Users, "自ホストの shadow user 行を作ってはいけない")
	assert.Empty(t, tracker.hosts, "自ホストの instance 行 / chart を作ってはいけない")
}

// 判定は **host** で行うこと。接頭辞一致だと scheme 違いで素通りし、その後の
// host binding は host しか見ないので shadow 行が作られる。
func TestResolveActor_RejectsSelfHostURIWithDifferentScheme(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(localActorBody), finalURL: "http://example.com/users/9local"}
	r, userRepo, _ := newLocalGuardResolver(t, f)

	_, err := r.ResolveActor("http://example.com/users/9local")
	assert.ErrorIs(t, err, federation.ErrLocalActor)
	assert.Empty(t, userRepo.Users)
}

// 既定ポートを明記した綴りでも同じこと (host の正規形で判定しているか)。
func TestResolveActor_RejectsSelfHostURIWithDefaultPort(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(localActorBody), finalURL: "https://example.com:443/users/9local"}
	r, userRepo, _ := newLocalGuardResolver(t, f)

	_, err := r.ResolveActor("https://example.com:443/users/9local")
	assert.ErrorIs(t, err, federation.ErrLocalActor)
	assert.Empty(t, userRepo.Users)
}

// `/users/` 以外の自ホスト URI も同じ扱い (upstream も host で見る)。
func TestResolveActor_RejectsSelfHostNonUserURI(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(localActorBody), finalURL: "https://example.com/actor"}
	r, userRepo, _ := newLocalGuardResolver(t, f)

	_, err := r.ResolveActor("https://example.com/actor")
	assert.ErrorIs(t, err, federation.ErrLocalActor)
	assert.Empty(t, userRepo.Users)
}

// リモートが自ホストの id を名乗る document を返しても行を作らないこと。
//
// **この条件では finalURL ↔ id の binding が先に落とす** (実測: allowCrossHost が
// 緩めるのは request URL ↔ id のほうだけ)。守りたいのは「行が出来ないこと」
// なので、どちらのゲートが落としたかではなく結果を固定する。
func TestResolveActorAllowCrossHost_SelfHostActorIDCreatesNoRow(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(localActorBody), finalURL: "https://evil.example/users/x"}
	r, userRepo, tracker := newLocalGuardResolver(t, f)

	_, err := r.ResolveActorAllowCrossHost("https://evil.example/users/x")
	require.Error(t, err)
	assert.Empty(t, userRepo.Users, "自ホストの id を名乗る document で行を作ってはいけない")
	assert.Empty(t, tracker.hosts)
}

// host binding が両方とも効かない条件でも、id が自ホストを指す document は
// 取り込まないこと。fetch 後の actor.id host を見ている層が生きていることを
// 固定する。
//
// 条件: allowCrossHost (= request URL ↔ id の binding が softfail) かつ
// fetcher が finalURLFetcher を実装しない (= finalURL ↔ id の binding を skip)。
func TestResolveActorAllowCrossHost_RejectsSelfHostActorIDWithoutHostBinding(t *testing.T) {
	r, userRepo, tracker := newLocalGuardResolver(t, &stubFetcher{body: []byte(localActorBody)})

	_, err := r.ResolveActorAllowCrossHost("https://evil.example/users/x")
	assert.ErrorIs(t, err, federation.ErrLocalActor)
	assert.Empty(t, userRepo.Users)
	assert.Empty(t, tracker.hosts)
}

// 過去に作られてしまった shadow 行があっても返さない (返すと refresh まで走る)。
// ローカル利用者は user.uri が NULL なので、ここで引けるのは shadow 行だけ。
func TestResolveActor_DoesNotReturnPreExistingSelfHostRow(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(localActorBody), finalURL: "https://example.com/users/9local"}
	r, userRepo, _ := newLocalGuardResolver(t, f)
	uri := "https://example.com/users/9local"
	host := "example.com"
	userRepo.Users["shadow"] = &model.User{ID: "shadow", Username: "alice", URI: &uri, Host: &host}

	got, err := r.ResolveActor(uri)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, federation.ErrLocalActor)
}

// リモート actor の解決は従来どおり動くこと (境界の反対側)。guard が
// 「全部落とす」形になっていないことを固定する。
func TestResolveActor_RemoteActorStillResolves(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(sampleActor), finalURL: "https://remote.example/users/alice"}
	r, userRepo, tracker := newLocalGuardResolver(t, f)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
	assert.Len(t, userRepo.Users, 1)
	assert.Equal(t, []string{"remote.example"}, tracker.hosts)
}

// 自ホストのサブドメインは別インスタンス。誤って落とすと連合が壊れる。
func TestResolveActor_SubdomainOfSelfHostIsRemote(t *testing.T) {
	body := `{"@context":"https://www.w3.org/ns/activitystreams",
		"id":"https://ap.example.com/users/bob","type":"Person","preferredUsername":"bob",
		"inbox":"https://ap.example.com/users/bob/inbox"}`
	f := &finalURLStubFetcher{body: []byte(body), finalURL: "https://ap.example.com/users/bob"}
	r, userRepo, _ := newLocalGuardResolver(t, f)

	user, err := r.ResolveActor("https://ap.example.com/users/bob")
	require.NoError(t, err)
	require.NotNil(t, user.Host)
	assert.Equal(t, "ap.example.com", *user.Host)
	assert.Len(t, userRepo.Users, 1)
}

// URL builder 未配線 (baseURL 空) の resolver では判定できないので、従来どおり
// 解決する。`selfHost() == ""` を「全部自ホスト」に倒すと全連合が止まる。
func TestResolveActor_UnwiredURLBuilderDoesNotRejectEverything(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(userRepo, testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder(""), &stubFetcher{body: []byte(sampleActor)}, idGen)

	_, err = r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Len(t, userRepo.Users, 1)
}

// ErrLocalActor が ErrInvalidActor を包んでいること自体を固定する。包みを
// 外すと inbox の Like / Announce 等が retry キューへ戻る (upstream は ack)。
func TestErrLocalActor_IsPermanentSkipShaped(t *testing.T) {
	assert.True(t, errors.Is(federation.ErrLocalActor, federation.ErrInvalidActor))
}

// ForceResolveActor は FindByURI → refreshActor を直接叩くので
// resolveActorOnceWithID の早期 guard を通らない。fetchActor 側の guard が
// 生きていること (= 自ホストへ HTTP を出しに行かないこと) を固定する。
//
// scheme を変えた綴りも同じ扱いであること。接頭辞一致 (`urls.IsLocalURI`) で
// 判定すると baseURL と scheme が違うだけで素通りする。
func TestForceResolveActor_DoesNotRefetchSelfHostActor(t *testing.T) {
	for _, uri := range []string{
		"https://example.com/users/9local",
		"http://example.com/users/9local",
		"https://example.com:443/users/9local",
	} {
		t.Run(uri, func(t *testing.T) {
			userRepo := testutil.NewMockUserRepository()
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			f := &countingFetcher{body: []byte(localActorBody)}
			r := federation.NewResolver(userRepo, testutil.NewMockNoteRepository(),
				activitypub.NewURLBuilder("https://example.com"), f, idGen)

			host := "example.com"
			u := uri
			userRepo.Users["shadow"] = &model.User{ID: "shadow", Username: "alice", URI: &u, Host: &host}

			_, _ = r.ForceResolveActor(uri)
			assert.Zero(t, f.calls, "自ホストの actor を取りに行ってはいけない")
		})
	}
}

// 既に作られてしまった shadow 行は、scheme 違いの綴りでも返さないこと。
// 接頭辞一致で判定すると baseURL と scheme が違う行が素通りする。
func TestResolveActor_DoesNotReturnPreExistingSelfHostRowWithOtherScheme(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(localActorBody), finalURL: "http://example.com/users/9local"}
	r, userRepo, _ := newLocalGuardResolver(t, f)
	uri := "http://example.com/users/9local"
	host := "example.com"
	userRepo.Users["shadow"] = &model.User{ID: "shadow", Username: "alice", URI: &uri, Host: &host}

	got, err := r.ResolveActor(uri)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, federation.ErrLocalActor)
}
