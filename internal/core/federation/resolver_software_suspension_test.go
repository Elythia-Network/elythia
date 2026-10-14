package federation_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 受信を拒否するソフトウェア (#3362) を、resolver 経由の取り込みにも効かせる (#3563)。
// 判定は本物の NewInboxSuspendedChecker で行い、meta と instance の行で設定する。

const suspActor = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://susp.example/users/carol",
	"type": "Person",
	"preferredUsername": "carol",
	"inbox": "https://susp.example/users/carol/inbox",
	"publicKey": {"publicKeyPem": "PEM"}
}`

const suspNoteURI = "https://susp.example/notes/s1"

const suspNote = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "` + suspNoteURI + `",
	"type": "Note",
	"attributedTo": "https://susp.example/users/carol",
	"content": "from a refused server",
	"to": ["https://www.w3.org/ns/activitystreams#Public"]
}`

// recordingDocFetcher serves per-URI documents and records the requested URIs.
type recordingDocFetcher struct {
	docFetcher
	mu  sync.Mutex
	got []string
}

func (f *recordingDocFetcher) FetchObject(uri string) ([]byte, error) {
	f.mu.Lock()
	f.got = append(f.got, uri)
	f.mu.Unlock()
	return f.docFetcher.FetchObject(uri)
}

func (f *recordingDocFetcher) fetched(uri string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.got {
		if u == uri {
			return true
		}
	}
	return false
}

// newSuspensionEnv builds a full processor whose resolver refuses
// susp.example (misskey 2024.10.0) through meta.inboxSuspendedSoftware.
func newSuspensionEnv(t *testing.T) (*fullProcessorEnv, *recordingDocFetcher, *testutil.MockMetaRepository) {
	t.Helper()
	fetcher := &recordingDocFetcher{docFetcher: docFetcher{docs: map[string]string{
		"https://remote.example/users/alice": aliceActor,
		"https://susp.example/users/carol":   suspActor,
		suspNoteURI:                          suspNote,
	}}}
	env := newFullProcessorFetcher(t, fetcher)

	instances := testutil.NewMockInstanceRepository()
	name, ver := "misskey", "2024.10.0"
	require.NoError(t, instances.Create(&model.Instance{ID: "i1", Host: "susp.example", SoftwareName: &name, SoftwareVersion: &ver}))
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{InboxSuspendedSoftware: []byte(`[{"software":"misskey","versionRange":"<=2024.11.0"}]`)}
	env.resolver.SetSoftwareSuspensionChecker(federation.NewInboxSuspendedChecker(meta, instances))
	require.True(t, env.resolver.HasSoftwareSuspensionChecker())
	return env, fetcher, meta
}

func TestResolver_InboxSuspendedSoftwareRejectsActorAndNote(t *testing.T) {
	env, fetcher, _ := newSuspensionEnv(t)

	_, err := env.resolver.ResolveActor("https://susp.example/users/carol")
	require.ErrorIs(t, err, federation.ErrHostNotAllowed)

	// 返信の連鎖・引用・ap/show が使う取得の経路。
	_, err = env.resolver.ResolveNote(suspNoteURI)
	require.ErrorIs(t, err, federation.ErrHostNotAllowed)

	assert.Empty(t, env.userRepo.Users, "拒否中のホストの actor は作らない")
	assert.Empty(t, env.noteRepo.Notes, "拒否中のホストの投稿は取り込まない")
	assert.False(t, fetcher.fetched(suspNoteURI), "拒否中のホストへは取りに行かない")
	assert.False(t, fetcher.fetched("https://susp.example/users/carol"), "拒否中のホストへは取りに行かない")
}

// 拒否していないサーバーから届いた Create の attributedTo が拒否中のホストを指す
// (転送された投稿) ときも取り込まない。
func TestResolver_InboxSuspendedSoftwareRejectsForwardedNote(t *testing.T) {
	env, _, _ := newSuspensionEnv(t)

	_, _, err := env.resolver.IngestNoteWithCreated([]byte(suspNote), "")
	require.ErrorIs(t, err, federation.ErrHostNotAllowed)
	assert.Empty(t, env.noteRepo.Notes)
}

// **拒否していないサーバーが、拒否中のホストの投稿をリノートしたら、リノートごと
// 捨てる。** ドメインブロックされたホストの投稿のリノートと同じ (ResolveNote が
// ErrHostNotAllowed を返し、handleAnnounce が permanent skip として ack する)。
func TestProcess_AnnounceOfInboxSuspendedSoftwareNoteIsDropped(t *testing.T) {
	env, fetcher, _ := newSuspensionEnv(t)
	seedAnnouncer(env)

	body := []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/a1",
		"actor": "https://remote.example/users/alice",
		"object": "` + suspNoteURI + `",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	require.NoError(t, env.processor.Process(body), "retry に乗せず ack する")
	assert.Empty(t, env.noteRepo.Notes, "元の投稿もリノートも作らない")
	assert.False(t, fetcher.fetched(suspNoteURI))
}

// 既に DB にある行は、ドメインブロックと同じく素通しになる (拒否を設定する前に
// 取り込んだ投稿を、既存のリノートや返信の表示から消さない)。
func TestResolver_InboxSuspendedSoftwareExistingRowPasses(t *testing.T) {
	env, _, _ := newSuspensionEnv(t)
	uri := suspNoteURI
	host := "susp.example"
	env.noteRepo.Notes["old"] = &model.Note{ID: "old", URI: &uri, UserHost: &host}

	got, err := env.resolver.ResolveNote(suspNoteURI)
	require.NoError(t, err)
	assert.Equal(t, "old", got.ID)
}

// 設定は判定のたびに読む。範囲から外せば、再起動せずに取り込めるようになる。
// 拒否していないホストは従来どおり取り込む。
func TestResolver_InboxSuspendedSoftwareFollowsSetting(t *testing.T) {
	env, _, meta := newSuspensionEnv(t)

	u, err := env.resolver.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err, "当たらないホストは取り込む")
	assert.Equal(t, "alice", u.Username)

	meta.Meta = &model.Meta{InboxSuspendedSoftware: []byte(`[{"software":"misskey","versionRange":"<2024.10.0"}]`)}
	note, err := env.resolver.ResolveNote(suspNoteURI)
	require.NoError(t, err, "範囲から外れたら取り込む")
	assert.Equal(t, suspNoteURI, *note.URI)
}

// stubSoftwareBlocker is a stub of federation.SoftwareSuspensionChecker that
// counts the lookups per host.
type stubSoftwareBlocker struct {
	suspended map[string]bool
	mu        sync.Mutex
	asked     map[string]int
}

func (s *stubSoftwareBlocker) IsSuspended(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asked == nil {
		s.asked = map[string]int{}
	}
	s.asked[host]++
	return s.suspended[host]
}

func (s *stubSoftwareBlocker) askedFor(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[host]
}

// 受信を拒否するソフトウェアのホストの、まだ知らない利用者へのメンションは取りに
// 行かない。**既知の利用者へのメンションでは判定しない** (instance の行を引かない。
// 既知の利用者はどのみち取りに行かない) (#3563)。
func TestIngestNote_MentionsOnInboxSuspendedSoftware(t *testing.T) {
	r, repo, f := newMentionFetchResolver(t)
	host := "known.example"
	known := "https://known.example/users/known"
	repo.Users["uKnown"] = &model.User{ID: "uKnown", Username: "known", UsernameLower: "known", Host: &host, URI: &known}
	const unknown = "https://susp.example/users/x"
	f.bodies[unknown] = actorDoc(unknown, "x")
	blocker := &stubSoftwareBlocker{suspended: map[string]bool{"susp.example": true, "known.example": true}}
	r.SetSoftwareSuspensionChecker(blocker)

	note, err := r.IngestNote(noteBody(t, "susp-m1", []string{known, unknown}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Equal(t, []string{"uKnown"}, []string(note.Mentions), "既知の利用者へのメンションは残る")
	assert.Zero(t, f.count(unknown), "拒否中のホストの利用者は取りに行かない")
	assert.Positive(t, blocker.askedFor("susp.example"))
	assert.Zero(t, blocker.askedFor("known.example"), "既知の利用者のホストは判定しない")
}

// 拒否中のホストへのメンションは、取りに行く件数の上限 (mentionLimit) を使わない。
// 使うと、その後ろに並んだ拒否していないホストの利用者を取りに行けなくなる。
func TestIngestNote_InboxSuspendedSoftwareMentionsDoNotUseFetchBudget(t *testing.T) {
	r, _, f := newMentionFetchResolver(t)
	var mentions []string
	for i := range corenote.DefaultMentionLimit {
		mentions = append(mentions, fmt.Sprintf("https://susp.example/users/u%d", i))
	}
	const legit = "https://ok.example/users/bob"
	f.bodies[legit] = actorDoc(legit, "bob")
	r.SetSoftwareSuspensionChecker(&stubSoftwareBlocker{suspended: map[string]bool{"susp.example": true}})

	// 宛先 (specified) の利用者は Mention の後に取りに行く。件数の上限で弾かれるか
	// どうかは問わない。見るのは取りに行ったかどうか。
	_, _ = r.IngestNote(noteBody(t, "susp-m2", mentions, []string{legit}, nil))
	assert.Equal(t, 1, f.count(legit), "拒否していないホストの利用者は取りに行く")
	assert.Equal(t, 1, f.total(), "拒否中のホストへは取りに行かない")
}

// newSpellingChecker returns the real inbox checker refusing misskey
// 2024.10.0, with instance rows stored the way hostFromURI stores them
// (idnhost.HostPort).
func newSpellingChecker(t *testing.T) *federation.SuspendedChecker {
	t.Helper()
	instances := testutil.NewMockInstanceRepository()
	name, ver := "misskey", "2024.10.0"
	for i, host := range []string{"susp.example", idnhost.Puny("例え.example"), "port.example:8443", "remote.example"} {
		require.NoError(t, instances.Create(&model.Instance{ID: fmt.Sprintf("i%d", i), Host: host, SoftwareName: &name, SoftwareVersion: &ver}))
	}
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{InboxSuspendedSoftware: []byte(`[{"software":"misskey","versionRange":"<=2024.11.0"}]`)}
	return federation.NewInboxSuspendedChecker(meta, instances)
}

// **instance の行が末尾のドットや :443 をそのまま持つ場合も当たる (#3563)。**
// 行は idnhost.HostPort の形で、`evil.example.` や http で :443 を使う相手の
// `h.example:443` を持ちうる。IsSuspended は渡された形のままでも引くので、揃えた
// 形 (ドットと :443 を落とした形) だけで引いていた頃のように外れない。受信の拒否と
// 配送停止 (#3558) の両方の checker で確かめる。
func TestSuspendedChecker_MatchesRowsKeepingDotOrPort(t *testing.T) {
	instances := testutil.NewMockInstanceRepository()
	name, ver := "misskey", "2024.10.0"
	for i, host := range []string{"susp.example.", "h.example:443", "dup.example."} {
		require.NoError(t, instances.Create(&model.Instance{ID: fmt.Sprintf("i%d", i), Host: host, SoftwareName: &name, SoftwareVersion: &ver}))
	}
	// 両方の形の行があるときは、渡された形のままの行を使う。
	newer := "2025.1.0"
	require.NoError(t, instances.Create(&model.Instance{ID: "dup", Host: "dup.example", SoftwareName: &name, SoftwareVersion: &newer}))
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{
		InboxSuspendedSoftware:   []byte(`[{"software":"misskey","versionRange":"<=2024.11.0"}]`),
		DeliverSuspendedSoftware: []byte(`[{"software":"misskey","versionRange":"<=2024.11.0"}]`),
	}
	gateHost := federation.NormalizeGateHost("https://susp.example./inbox")
	require.Equal(t, "susp.example.", gateHost, "行と同じ形になる前提")
	for _, c := range []struct {
		name    string
		checker *federation.SuspendedChecker
	}{
		{"inbox", federation.NewInboxSuspendedChecker(meta, instances)},
		{"deliver", federation.NewSuspendedChecker(meta, instances)},
	} {
		for _, host := range []string{"susp.example.", "SUSP.example.", gateHost, "h.example:443"} {
			assert.True(t, c.checker.IsSuspended(host), c.name+" "+host)
		}
		assert.False(t, c.checker.IsSuspended("h.example"), c.name+": :443 を持つ行は、ポートの無い名前とは別のホスト")
		assert.True(t, c.checker.IsSuspended("dup.example."), c.name+": 渡された形のままの行を優先する")
		assert.False(t, c.checker.IsSuspended("dup.example"), c.name)
	}
}

// **IsSuspended は、渡されたホストを instance の行の形 (小文字、punycode、末尾の
// ドットなし、既定のポートなし) に揃えた形でも引く (#3563)。** inbox の keyId、
// users/show の host、resolver は、それぞれ違う形でホストを渡す。
func TestInboxSuspendedChecker_NormalizesHostSpelling(t *testing.T) {
	c := newSpellingChecker(t)
	for host, want := range map[string]bool{
		"susp.example":       true,
		"SUSP.example":       true,
		"susp.example.":      true,
		"susp.example:443":   true,
		"SUSP.Example.:443":  true,
		" susp.example ":     true,
		"例え.example":         true,
		"例え。example":         true,
		"port.example:8443":  true,
		"PORT.example.:8443": true,
		"port.example":       false,
		"susp.example:8443":  false,
		"other.example":      false,
		"susp.example/x":     false,
		"":                   false,
	} {
		assert.Equal(t, want, c.IsSuspended(host), host)
	}
}

// **メンションの解決と users/show も、ドメインブロックと同じく WebFinger を投げる前に
// 止める (#3563)。** まだ知らない利用者は取り込まない。ホストの書き方は
// SuspendedChecker が揃える。
func TestRemoteUserResolver_ResolveByUsernameHost_SkipsInboxSuspendedSoftware(t *testing.T) {
	okHost := "ok.example"
	for _, tc := range []struct {
		name, host string
		wantSkip   bool
	}{
		{"suspended", "susp.example", true},
		{"suspended_uppercase_port", "SUSP.example:443", true},
		{"suspended_trailing_dot", "susp.example.", true},
		{"suspended_idn", "例え.example", true},
		{"other", "ok.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := &fakeWebFinger{uri: "https://ok.example/users/alice"}
			ar := &fakeActorResolver{user: &model.User{ID: "uR", Host: &okHost}}
			r := federation.NewRemoteUserResolver(wf, ar, testutil.NewMockUserRepository(), "local.example")
			r.SetSoftwareSuspensionChecker(newSpellingChecker(t))
			require.True(t, r.HasSoftwareSuspensionChecker())

			_, err := r.ResolveByUsernameHost("alice", tc.host)
			if tc.wantSkip {
				require.ErrorIs(t, err, federation.ErrRemoteUserHostNotAllowed)
				assert.Empty(t, wf.call.host, "WebFinger を投げない")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.host, wf.call.host)
		})
	}
}

// **保存済みの利用者の再同期は、拒否中のホストなら問い合わせずに保存済みの行を返す
// (#3563)。** 受信の拒否は配送を止めないので、失敗にするとこちらから出す投稿の
// メンションや DM の宛先から 24 時間に 1 回落ちる。連合ポリシーで拒否したホストは
// 従来どおり失敗にする (TestResyncIfStale_SkipsBlockedHost)。
func TestResyncIfStale_InboxSuspendedSoftwareKeepsStoredRow(t *testing.T) {
	r, wf, ar, repo, u := newResyncFixture(t, nil, aliceURI)
	r.SetSoftwareSuspensionChecker(newSpellingChecker(t))

	got, err := r.ResyncIfStale(u)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "uA", got.ID, "保存済みの行を返す")
	assert.Zero(t, wf.calls.Load(), "WebFinger を投げない")
	assert.Empty(t, ar.refreshed, "actor を取り直さない")
	assert.NotNil(t, repo.Users["uA"].LastFetchedAt, "再同期の時刻は進める (続く 24 時間は試さない)")

	// 連合ポリシーの拒否は、ソフトウェアの拒否より先に見て失敗にする。
	r2, wf2, _, _, u2 := newResyncFixture(t, nil, aliceURI)
	r2.SetSoftwareSuspensionChecker(newSpellingChecker(t))
	r2.SetHostBlockChecker(stubRemoteUserHostBlocker{blocked: map[string]bool{"remote.example": true}})
	_, err = r2.ResyncIfStale(u2)
	require.ErrorIs(t, err, federation.ErrRemoteUserHostNotAllowed)
	assert.Zero(t, wf2.calls.Load())
}
