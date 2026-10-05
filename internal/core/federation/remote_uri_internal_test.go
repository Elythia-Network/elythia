package federation

import (
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"

	"github.com/stretchr/testify/require"

	"github.com/stretchr/testify/assert"
)

// fitsColumn は rune 数で見て、NUL は長さに関わらず弾くこと (#2723)。
func TestFitsColumn(t *testing.T) {
	nul := string(rune(0))
	assert.True(t, fitsColumn("abc", 3))
	assert.False(t, fitsColumn("abcd", 3))
	// PostgreSQL の varchar はコードポイント数で数える。byte で見ると 3 倍になる。
	assert.True(t, fitsColumn(strings.Repeat("\u3042", 3), 3))
	assert.False(t, fitsColumn("a"+nul, 10), "NUL は長さに関わらず弾く")
	assert.True(t, fitsColumn("", 0))
}

// remoteURIValue は**切らずに捨てる** (#2723)。
//
// 途中で切った URI は別物で、取りに行っても無駄なうえ壊れた参照を保存する
// ことになる。`remoteMediaURL` と同じ判断。
func TestRemoteURIValue(t *testing.T) {
	nul := string(rune(0))
	long := "https://remote.example/" + strings.Repeat("a", 512)

	assert.Equal(t, "https://remote.example/u", remoteURIValue("https://remote.example/u", "user.inbox", "https://remote.example/u"))
	assert.Empty(t, remoteURIValue("https://remote.example/u", "user.inbox", long), "切らずに捨てること")
	assert.Empty(t, remoteURIValue("https://remote.example/u", "user.inbox", "https://h/"+nul), "NUL を含む URI を捨てること")
	assert.Empty(t, remoteURIValue("https://remote.example/u", "user.inbox", ""))

	// ちょうど列に収まる長さは通す (境界を 1 つずらす実装を弾く)。
	fit := "https://h/" + strings.Repeat("a", userURIMaxRunes-len("https://h/"))
	require.Len(t, []rune(fit), userURIMaxRunes)
	assert.Equal(t, fit, remoteURIValue("https://remote.example/u", "user.inbox", fit))
	assert.Empty(t, remoteURIValue("https://remote.example/u", "user.inbox", fit+"a"))
}

// alsoKnownAs は要素ごとに捨てること (#2723)。
//
// 列は text なので長さは効かないが、**NUL は 22021 で弾かれる**。1 要素混ざった
// だけで同じ INSERT / UPDATE の全部が失われる。
func TestRemoteURIList(t *testing.T) {
	nul := string(rune(0))
	assert.Equal(t, "https://a/1,https://a/2",
		remoteURIList("https://a/u", "user.alsoKnownAs", []string{"https://a/1", "https://a/2"}))
	assert.Equal(t, "https://a/2",
		remoteURIList("https://a/u", "user.alsoKnownAs", []string{"https://a/" + nul + "1", "https://a/2"}),
		"NUL を含む要素だけ落とすこと")
	assert.Empty(t, remoteURIList("https://a/u", "user.alsoKnownAs", nil))
	// **切らない。** 切った URI は別物なので、一致判定 (移行の認可) に使えない。
	long := "https://a/" + strings.Repeat("x", 600)
	assert.Equal(t, long, remoteURIList("https://a/u", "user.alsoKnownAs", []string{long}))
}

// actorDocWith builds an actor document with the given extra top-level fields.
func actorDocWith(host, name, extra string) string {
	base := "https://" + host + "/users/" + name
	return `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + base +
		`","type":"Person","preferredUsername":"` + name + `","inbox":"` + base + `/inbox",` + extra +
		`"publicKey":{"id":"` + base + `#main-key","owner":"` + base +
		`","publicKeyPem":"-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"}}`
}

// 収まらない任意の URI は捨てて、actor 自体は取り込むこと (#2723)。
//
// 切らずに捨てるのは、途中で切った URI が別物になるから。捨てずに落とすと
// **actor が 1 行も作られず**、refresh も同じ失敗を繰り返す。
func TestResolveActor_DropsOversizedOptionalURIs(t *testing.T) {
	long := "https://remote.example/" + strings.Repeat("a", 512)
	r, _ := ingestCWEnv(t)
	f := r.fetcher.(*countingFetcher)
	f.docs["https://remote.example/users/big"] = actorDocWith("remote.example", "big",
		`"featured":"`+long+`","movedTo":"`+long+`",`)

	user, err := r.ResolveActor("https://remote.example/users/big")
	require.NoError(t, err, "収まらない任意 URI で actor ごと落ちている")
	require.NotNil(t, user)
	assert.Nil(t, user.Featured, "収まらない featured を保存している")
	assert.Nil(t, user.MovedToURI, "収まらない movedToUri を保存している")
	// 身元は残る。
	require.NotNil(t, user.URI)
	assert.Equal(t, "https://remote.example/users/big", *user.URI)
}

// 身元が列に入らない actor は拒否すること (#2723)。
//
// `uri` / `host` / `username` は切ると別人になるし、捨てるわけにもいかない
// (NOT NULL / lookup の鍵)。ここで弾かないと Create が落ちて同じ失敗を繰り返す。
func TestResolveActor_RejectsOversizedIdentity(t *testing.T) {
	r, _ := ingestCWEnv(t)
	f := r.fetcher.(*countingFetcher)
	// **username は既存の validRemoteUsername が 128 で弾く**ので、そちらでは
	// この gate に到達しない。uri が列 (varchar(512)) を超える形で確かめる。
	uri := "https://remote.example/users/u" + strings.Repeat("q", 520)
	f.docs[uri] = actorDocWith2(uri, "shortname")

	_, err := r.ResolveActor(uri)
	require.Error(t, err, "身元が列に入らない actor を受理している")
	assert.ErrorIs(t, err, ErrInvalidActor)

	// **host も見る。** uri が 512 に収まっていても host が 128 を超えることは
	// ありうる (DNS の上限は 253)。uri だけ見る実装だと素通りする。
	//
	// **実在しうる形にする。** 単一ラベル 200 文字は DNS のラベル上限 (63) を
	// 超えるので、正規化の実装が変わったときに「そもそも来ない入力」で通って
	// しまう。43 文字ラベル 3 つで 131 文字にする。
	label := strings.Repeat("h", 43)
	longHost := label + "." + label + "." + label + ".example"
	require.Greater(t, len([]rune(longHost)), userHostMaxRunes, "前提: host が列を超える")
	hostURI := "https://" + longHost + "/users/x"
	require.Less(t, len([]rune(hostURI)), userURIMaxRunes, "前提: uri は列に収まる")
	f.docs[hostURI] = actorDocWith2(hostURI, "x")

	_, err = r.ResolveActor(hostURI)
	require.Error(t, err, "host が列に入らない actor を受理している")
	assert.ErrorIs(t, err, ErrInvalidActor)
}

// actorDocWith2 builds an actor document with an explicit id.
func actorDocWith2(id, name string) string {
	return `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + id +
		`","type":"Person","preferredUsername":"` + name + `","inbox":"` + id + `/inbox",` +
		`"publicKey":{"id":"` + id + `#main-key","owner":"` + id +
		`","publicKeyPem":"-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"}}`
}

// actorDocWithInbox builds an actor document with an explicit inbox.
func actorDocWithInbox(host, name, inbox string) string {
	base := "https://" + host + "/users/" + name
	return `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + base +
		`","type":"Person","preferredUsername":"` + name + `","inbox":"` + inbox + `",` +
		`"publicKey":{"id":"` + base + `#main-key","owner":"` + base +
		`","publicKeyPem":"-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"}}`
}

// 収まらない inbox は捨てて actor は取り込むこと (#2723)。
//
// inbox が nil だとその actor への配送はできなくなるが、表示や mention の解決は
// 生きる。捨てずに書くと **actor が 1 行も作られない**。
func TestResolveActor_DropsOversizedInbox(t *testing.T) {
	r, _ := ingestCWEnv(t)
	f := r.fetcher.(*countingFetcher)
	// **host は actor と同じにする。** 別 host の inbox は sameDeliveryHost が
	// 先に弾くので、この経路に到達しない。
	longInbox := "https://remote.example/inbox/" + strings.Repeat("a", 512)
	f.docs["https://remote.example/users/bigin"] = actorDocWithInbox("remote.example", "bigin", longInbox)

	user, err := r.ResolveActor("https://remote.example/users/bigin")
	require.NoError(t, err, "収まらない inbox で actor ごと落ちている")
	require.NotNil(t, user)
	assert.Nil(t, user.Inbox, "収まらない inbox を保存している")
}

// NUL を含む alsoKnownAs の要素は落として actor は取り込むこと (#2723)。
//
// 列は text なので長さは効かないが、NUL は 22021 で書き込みごと落ちる。
func TestResolveActor_DropsAlsoKnownAsWithNUL(t *testing.T) {
	r, _ := ingestCWEnv(t)
	f := r.fetcher.(*countingFetcher)
	f.docs["https://remote.example/users/aka"] = actorDocWith("remote.example", "aka",
		`"alsoKnownAs":["https://old.example/users/a\u0000b","https://old.example/users/ok"],`)

	user, err := r.ResolveActor("https://remote.example/users/aka")
	require.NoError(t, err, "NUL を含む alsoKnownAs で actor ごと落ちている")
	require.NotNil(t, user)
	require.NotNil(t, user.AlsoKnownAs)
	assert.Equal(t, "https://old.example/users/ok", *user.AlsoKnownAs,
		"NUL を含む要素だけ落として残りは保存すること")
}

// refresh 経路でも収まらない URI を捨てること (#2723)。
//
// **create 側より重い。** refresh の UPDATE は `lastFetchedAt` を同じ文に載せて
// いるので、1 列でも溢れると更新が丸ごと失敗して TTL が進まず、inbound activity
// 1 件につき outbound fetch が 1 回走り続ける。
func TestRefreshActor_DropsOversizedOptionalURIs(t *testing.T) {
	r, _ := ingestCWEnv(t)
	users := r.userRepo.(*testutil.MockUserRepository)
	f := r.fetcher.(*countingFetcher)

	uri := "https://remote.example/users/refresh"
	host := "remote.example"
	oldInbox := uri + "/inbox"
	stale := r.clock().Add(-30 * 24 * time.Hour)
	users.Users["u_refresh"] = &model.User{
		ID: "u_refresh", Username: "refresh", UsernameLower: "refresh",
		URI: &uri, Host: &host, Inbox: &oldInbox, LastFetchedAt: &stale,
	}

	long := "https://remote.example/" + strings.Repeat("a", 512)
	f.docs[uri] = actorDocWith(host, "refresh",
		`"featured":"`+long+`","movedTo":"`+long+`","alsoKnownAs":["https://old.example/x"],`)

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Nil(t, user.Featured, "収まらない featured を保存している")
	assert.Nil(t, user.MovedToURI, "収まらない movedToUri を保存している")
	// **TTL が進んでいること。** ここが進まないと fetch が止まらない。
	require.NotNil(t, user.LastFetchedAt)
	assert.True(t, user.LastFetchedAt.After(stale), "lastFetchedAt が進んでいない")
	// 巻き添えを確認する: 同じ UPDATE に載る他の値は反映される。
	require.NotNil(t, user.AlsoKnownAs)
	assert.Equal(t, "https://old.example/x", *user.AlsoKnownAs)
}
