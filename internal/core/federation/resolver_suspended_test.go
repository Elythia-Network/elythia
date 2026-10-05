package federation_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// stubOriginRepo is an in-memory UserSuspensionOriginRepository (#2973).
type stubOriginRepo struct {
	mu      sync.Mutex
	origins map[string]string
	readErr error
	setErr  error
	sets    []string
}

func newStubOriginRepo() *stubOriginRepo {
	return &stubOriginRepo{origins: map[string]string{}}
}

func (s *stubOriginRepo) Origin(userID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return "", s.readErr
	}
	return s.origins[userID], nil
}

func (s *stubOriginRepo) Set(userID, origin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.origins[userID] = origin
	s.sets = append(s.sets, userID+"="+origin)
	return nil
}

func (s *stubOriginRepo) Clear(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.origins, userID)
	return nil
}

// actorWithSuspended returns sampleActor with the Mastodon `suspended`
// extension set to the given literal (raw JSON なので "true" / `"yes"` など
// 壊れた形も渡せる)。
func actorWithSuspended(t *testing.T, literal string) string {
	t.Helper()
	out := strings.Replace(sampleActor,
		`"type": "Person",`,
		`"type": "Person",`+"\n\t\"suspended\": "+literal+",", 1)
	// **置換が空振りしたら落とす。** `strings.Replace` は一致しなければ元の
	// 文字列をそのまま返すので、`sampleActor` の整形が変わると
	// 「suspended を含まない actor」で否定側テスト (false で解除しない /
	// 壊れた値で凍結しない) が**空虚に PASS** する。
	if !strings.Contains(out, `"suspended"`) {
		t.Fatalf("actorWithSuspended: 置換が空振りした (sampleActor の整形が変わった?)")
	}
	return out
}

// **発信元が凍結を明示していたら、取り込んだ時点で凍結する** (#2951)。
//
// Mastodon 系は凍結しても `Delete` を送らず actor に `suspended: true` を
// 立てるだけなので、読まないと「相手は切ったのにこちらでは生きている」状態に
// なる。Misskey 系は `Delete` を送るので同じ結果になる。
func TestResolveActor_SuspendedTrue_SuspendsNewUser(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "true"), nil)
	r.SetSuspensionOriginRepo(newStubOriginRepo())

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Truef(t, user.IsSuspended,
		"発信元が suspended を立てているのに凍結されていない")

	stored := repo.Users[user.ID]
	require.NotNil(t, stored)
	assert.True(t, stored.IsSuspended, "保存された行にも反映されていること")
}

// 立っていないときは触らない (既定の挙動を変えない)。
func TestResolveActor_NoSuspended_DoesNotSuspend(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	r.SetSuspensionOriginRepo(newStubOriginRepo())
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.False(t, user.IsSuspended)
}

// **由来が記録されていない行は `false` で解除しない** (#2973)。
//
// この表より前から凍結されている行にはモデレーターの判断が混ざっているので、
// リモートに解除させない (安全側)。由来が `remote` の行だけが追従する —
// それは `TestResolveActor_RemoteSuspend_IsLiftedByRemote` が見る。
func TestResolveActor_SuspendedFalse_DoesNotUnsuspend(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "false"), nil)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)

	// ローカルのモデレーターが先に凍結している状態を作る。
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: true,
	}

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Truef(t, user.IsSuspended,
		"発信元の suspended:false でローカルの凍結が解除された。"+
			"侵害された発信元がモデレーターの判断を取り消せてしまう")
	assert.True(t, repo.Users["existing"].IsSuspended, "保存された行も凍結のままであること")
}

// 既存ユーザーが後から凍結されたら追従する (更新経路)。
func TestResolveActor_SuspendedTrue_SuspendsExistingUser(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "true"), nil)
	r.SetSuspensionOriginRepo(newStubOriginRepo())

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: false,
	}

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Truef(t, user.IsSuspended, "後から凍結された actor に追従していない")
	assert.True(t, repo.Users["existing"].IsSuspended)
}

// **壊れた値で誤って凍結しない。** `APLenientBool` は読めない形を false に
// 倒す (`APTruthyBool` だと不明な形が true になり、意味が反転する)。
func TestResolveActor_SuspendedMalformed_DoesNotSuspend(t *testing.T) {
	// `[]` / `{"a":1}` / `"maybe"` は **`APLenientBool` と `APTruthyBool` で
	// 結果が変わる**入力 (実測)。後者だと true になり、壊れた値で誤凍結する。
	for _, literal := range []string{`"no"`, `{"a":1}`, `[]`, `null`, `0`, `"maybe"`} {
		t.Run(literal, func(t *testing.T) {
			r, _ := newResolver(t, actorWithSuspended(t, literal), nil)
			r.SetSuspensionOriginRepo(newStubOriginRepo())
			user, err := r.ResolveActor("https://remote.example/users/alice")
			require.NoErrorf(t, err, "actor ごと読めなくなっている (document を壊してはいけない)")
			assert.Falsef(t, user.IsSuspended,
				"%s を凍結として読んでいる。壊れた値で誤凍結する", literal)
		})
	}
}

// 文字列の "true" は PostgreSQL の boolean 入力構文として真。
func TestResolveActor_SuspendedStringTrue_Suspends(t *testing.T) {
	r, _ := newResolver(t, actorWithSuspended(t, `"true"`), nil)
	r.SetSuspensionOriginRepo(newStubOriginRepo())
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.True(t, user.IsSuspended)
}

// --- #2973: 凍結の由来 (local / remote) ---

// **モデレーターが解除した行は、発信元が立てていても再凍結しない。**
//
// これが #2951 の既知の制約そのもの。由来を持たないと `admin/unsuspend-user`
// の結果が次の actor refresh で無言で戻り、しかも
// `/api/federation/update-remote-user` は RequireAuth だけなので任意のログイン
// 利用者が TTL を待たずに発火できた。
func TestResolveActor_LocalUnsuspend_IsNotOverriddenByRemote(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "true"), nil)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: false,
	}
	// モデレーターが解除した (= local 由来)。
	require.NoError(t, origins.Set("existing", model.SuspensionOriginLocal))

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Falsef(t, user.IsSuspended,
		"モデレーターの解除がリモートに巻き戻された。#2951 の制約が残っている")
	assert.False(t, repo.Users["existing"].IsSuspended)
}

// **モデレーターが凍結した行は、発信元が下ろしても解除しない。**
func TestResolveActor_LocalSuspend_IsNotLiftedByRemote(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "false"), nil)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: true,
	}
	require.NoError(t, origins.Set("existing", model.SuspensionOriginLocal))

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Truef(t, user.IsSuspended,
		"モデレーターの凍結がリモートに解除された")
}

// **リモート由来の凍結は、発信元が下ろせば解除する** (Mastodon と同じ two-way)。
func TestResolveActor_RemoteSuspend_IsLiftedByRemote(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "false"), nil)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: true,
	}
	// こちらが actor を見て凍結した (= remote 由来)。
	require.NoError(t, origins.Set("existing", model.SuspensionOriginRemote))

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Falsef(t, user.IsSuspended,
		"リモート由来の凍結が発信元の解除に追従していない")
}

// **記録が無い行は local 扱い** (安全側)。この表より前から凍結されている行を
// リモートが解除できてはいけない。
func TestResolveActor_UnknownOrigin_IsTreatedAsLocal(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "false"), nil)
	r.SetSuspensionOriginRepo(newStubOriginRepo()) // 記録なし

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: true,
	}

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Truef(t, user.IsSuspended,
		"由来不明の凍結がリモートに解除された。移行前の行が巻き添えになる")
}

// 自動凍結したら由来を remote として刻む (次の解除に追従できるように)。
func TestResolveActor_RemoteSuspend_RecordsOrigin(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "true"), nil)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: false,
	}

	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	got, err := origins.Origin("existing")
	require.NoError(t, err)
	assert.Equal(t, model.SuspensionOriginRemote, got, "由来を刻んでいない")
}

// **由来を記録できなかったら凍結もしない。** 記録が無いまま凍結すると、次の
// refresh で「由来不明 = local」として扱われ、モデレーターが解除できるかどうかが
// 運任せになる。
func TestResolveActor_OriginWriteFails_DoesNotSuspend(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "true"), nil)
	origins := newStubOriginRepo()
	origins.setErr = errors.New("connection refused")
	r.SetSuspensionOriginRepo(origins)

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: false,
	}

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Falsef(t, user.IsSuspended,
		"由来を記録できないのに凍結した。解除できない行が生まれる")
}

// **由来の読み取りが失敗したら触らない。** 「記録が無い」と取り違えると、
// 接続断の瞬間にモデレーターの判断を上書きしうる。
func TestResolveActor_OriginReadFails_LeavesStateUntouched(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "true"), nil)
	origins := newStubOriginRepo()
	origins.readErr = errors.New("connection refused")
	r.SetSuspensionOriginRepo(origins)

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: false,
	}

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.False(t, user.IsSuspended, "DB 障害を「記録が無い」と取り違えている")
}

// **由来を記録できない (未配線) なら、そもそも読まない。** #2951 の制約を
// 再生産しないため。
func TestResolveActor_NoOriginRepo_DoesNotSuspend(t *testing.T) {
	r, _ := newResolver(t, actorWithSuspended(t, "true"), nil)
	// SetSuspensionOriginRepo を呼ばない

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Falsef(t, user.IsSuspended,
		"由来を持てないのに凍結した。モデレーターが解除しても戻ってしまう")
}

// **凍結して作った行にも由来を刻む** (#2973 レビュー H1)。
//
// 刻まないと、この行は次の refresh で「由来不明 = 解除方向では local 扱い」に
// なり、**発信元が解除しても永久に凍結のまま**になる (two-way が効かない層)。
func TestResolveActor_SuspendedOnCreate_RecordsOrigin(t *testing.T) {
	r, _ := newResolver(t, actorWithSuspended(t, "true"), nil)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.True(t, user.IsSuspended)

	got, err := origins.Origin(user.ID)
	require.NoError(t, err)
	assert.Equalf(t, model.SuspensionOriginRemote, got,
		"作成時に由来を刻んでいない。発信元が解除しても永久に凍結のままになる")
}

// 作成時に由来を記録できなかったら凍結を取り下げる (宣言どおり)。
func TestResolveActor_SuspendedOnCreate_OriginWriteFails_Unsuspends(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "true"), nil)
	origins := newStubOriginRepo()
	origins.setErr = errors.New("connection refused")
	r.SetSuspensionOriginRepo(origins)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Falsef(t, user.IsSuspended,
		"由来を記録できないのに凍結した。解除できない行が生まれる")
	assert.False(t, repo.Users[user.ID].IsSuspended, "DB 側も取り下げること")
}

// **削除済みの行には触らない** (#2973 レビュー H2)。
// `admin/accounts/delete` は isSuspended と isDeleted を同時に立てるので、
// remote 由来の記録が残っていると発信元が tombstone の凍結を外せる。
func TestResolveActor_DeletedTombstone_IsNotUnsuspended(t *testing.T) {
	r, repo := newResolver(t, actorWithSuspended(t, "false"), nil)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)

	host := "remote.example"
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &uri, IsSuspended: true, IsDeleted: true,
	}
	require.NoError(t, origins.Set("existing", model.SuspensionOriginRemote))

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Truef(t, user.IsSuspended,
		"削除済み tombstone の凍結が発信元に解除された。inbound gate は isSuspended しか見ないので activity が再び通る")
}
