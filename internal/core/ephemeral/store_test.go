package ephemeral_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/core/ephemeral"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("failed to setup redis: %v", err)
	}
	code := m.Run()
	testRedis.Teardown(ctx)
	os.Exit(code)
}

func newStore(t *testing.T, ttl time.Duration) *ephemeral.Store {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	testRedis.FlushAll(context.Background())
	return ephemeral.NewStore(testRedis.Client, "example.com:", func() ephemeral.Settings {
		return ephemeral.Settings{Enabled: true, TTL: ttl}
	})
}

func strptr(s string) *string { return &s }

func sampleNote(id, uri, userID string) *model.Note {
	host := "remote.example"
	return &model.Note{
		ID: id, UserID: userID, UserHost: &host, URI: strptr(uri),
		Text: strptr("hello"), Visibility: model.NoteVisibilityPublic,
	}
}

func sampleUser(id, uri string) *model.User {
	host := "remote.example"
	return &model.User{
		ID: id, Username: "alice", UsernameLower: "alice",
		Host: &host, URI: strptr(uri),
	}
}

func TestStore_PutAndGetNote(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()

	n := sampleNote("n1", "https://remote.example/notes/1", "u1")
	u := sampleUser("u1", "https://remote.example/users/alice")
	require.NoError(t, s.PutNote(ctx, n, u))

	got, err := s.GetNote(ctx, "n1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "n1", got.ID)
	assert.Equal(t, "hello", *got.Text)
	// 下流の ApplyFilter / PackNotes は note.User を見るので詰まっていること。
	require.NotNil(t, got.User)
	assert.Equal(t, "u1", got.User.ID)
	assert.Equal(t, "alice", got.User.Username)
}

func TestStore_GetNote_Missing(t *testing.T) {
	s := newStore(t, time.Minute)
	got, err := s.GetNote(context.Background(), "nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_GetNotes_SkipsMissingAndKeepsOrder(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	u := sampleUser("u1", "https://remote.example/users/alice")
	for _, id := range []string{"n1", "n3"} {
		require.NoError(t, s.PutNote(ctx, sampleNote(id, "https://remote.example/notes/"+id, "u1"), u))
	}

	got, err := s.GetNotes(ctx, []string{"n1", "n2", "n3"})
	require.NoError(t, err)
	require.Len(t, got, 2, "存在しない ID は詰めて返す")
	assert.Equal(t, "n1", got[0].ID)
	assert.Equal(t, "n3", got[1].ID)
}

// 同じ著者の連投で著者 JSON が重複保存されないこと (note 側から User を外す)。
func TestStore_SharesAuthorAcrossNotes(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	u := sampleUser("u1", "https://remote.example/users/alice")
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"), u))
	require.NoError(t, s.PutNote(ctx, sampleNote("n2", "https://remote.example/notes/2", "u1"), u))

	got, err := s.GetNotes(ctx, []string{"n1", "n2"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, n := range got {
		require.NotNil(t, n.User)
		assert.Equal(t, "u1", n.User.ID)
	}
}

// URI から ID を引けること。同じ著者の 2 件目で ID を再利用するために要る。
func TestStore_LookupIDByURI(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	noteURI := "https://remote.example/notes/1"
	userURI := "https://remote.example/users/alice"
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", noteURI, "u1"), sampleUser("u1", userURI)))

	nid, err := s.NoteIDByURI(ctx, noteURI)
	require.NoError(t, err)
	assert.Equal(t, "n1", nid)

	uid, err := s.UserIDByURI(ctx, userURI)
	require.NoError(t, err)
	assert.Equal(t, "u1", uid)
}

func TestStore_LookupIDByURI_Missing(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	nid, err := s.NoteIDByURI(ctx, "https://remote.example/notes/ghost")
	require.NoError(t, err)
	assert.Empty(t, nid)
	uid, err := s.UserIDByURI(ctx, "https://remote.example/users/ghost")
	require.NoError(t, err)
	assert.Empty(t, uid)
}

func TestStore_GetUser(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))

	got, err := s.GetUser(ctx, "u1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "alice", got.Username)

	missing, err := s.GetUser(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

// materialize / 直接配送で DB に入ったときに ephemeral を落とす経路。
func TestStore_DropNote(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	uri := "https://remote.example/notes/1"
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", uri, "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))

	require.NoError(t, s.DropNote(ctx, "n1", uri))

	got, err := s.GetNote(ctx, "n1")
	require.NoError(t, err)
	assert.Nil(t, got)
	nid, err := s.NoteIDByURI(ctx, uri)
	require.NoError(t, err)
	assert.Empty(t, nid, "URI 逆引きも消えること (二重取り込みの防止に使うため)")

	// 著者は他のノートが参照しうるので消さない (TTL に任せる)。
	u, err := s.GetUser(ctx, "u1")
	require.NoError(t, err)
	assert.NotNil(t, u)
}

func TestStore_Expires(t *testing.T) {
	s := newStore(t, 300*time.Millisecond)
	ctx := context.Background()
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))

	require.Eventually(t, func() bool {
		got, err := s.GetNote(ctx, "n1")
		return err == nil && got == nil
	}, 3*time.Second, 100*time.Millisecond, "TTL 切れで消えること")
}

// 閲覧では materialize しないので、読み取り時に TTL を延ばして
// 「開いた直後に期限切れ」を防ぐ。
func TestStore_TouchExtendsTTL(t *testing.T) {
	s := newStore(t, 700*time.Millisecond)
	ctx := context.Background()
	n := sampleNote("n1", "https://remote.example/notes/1", "u1")
	require.NoError(t, s.PutNote(ctx, n, sampleUser("u1", "https://remote.example/users/alice")))

	// TTL の半分ほど待ってから打ち直す。延長が効かなければ元の期限で消える。
	time.Sleep(400 * time.Millisecond)
	require.NoError(t, s.Touch(ctx, n))
	time.Sleep(500 * time.Millisecond)

	got, err := s.GetNote(ctx, "n1")
	require.NoError(t, err)
	assert.NotNil(t, got, "Touch 後は元の TTL を過ぎても生きていること")
}

func TestStore_PutNote_RequiresIDAndURI(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	u := sampleUser("u1", "https://remote.example/users/alice")

	assert.Error(t, s.PutNote(ctx, &model.Note{UserID: "u1", URI: strptr("x")}, u), "id 必須")
	assert.Error(t, s.PutNote(ctx, &model.Note{ID: "n1", UserID: "u1"}, u), "uri 必須")
}

// nil レシーバ / nil 引数で落ちないこと (機能無効時は nil を配線するため)。
func TestStore_NilSafe(t *testing.T) {
	var s *ephemeral.Store
	ctx := context.Background()
	assert.NoError(t, s.PutNote(ctx, nil, nil))
	n, err := s.GetNote(ctx, "n1")
	assert.NoError(t, err)
	assert.Nil(t, n)
	assert.NoError(t, s.DropNote(ctx, "n1", "uri"))
	assert.NoError(t, s.Touch(ctx, nil))
}

// --- 設定と縮退経路 ---

// TTL 未設定 / 非正の値では既定値にフォールバックすること。
func TestStore_TTLFallback(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		ttl  func() ephemeral.Settings
	}{
		{"settings 関数が nil", nil},
		{"ttl が 0", func() ephemeral.Settings { return ephemeral.Settings{Enabled: true} }},
		{"ttl が負", func() ephemeral.Settings { return ephemeral.Settings{Enabled: true, TTL: -time.Minute} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRedis.FlushAll(ctx)
			s := ephemeral.NewStore(testRedis.Client, "example.com:", tc.ttl)
			require.NoError(t, s.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
				sampleUser("u1", "https://remote.example/users/alice")))
			got, err := s.GetNote(ctx, "n1")
			require.NoError(t, err)
			assert.NotNil(t, got, "既定 TTL で保存されていること")
		})
	}
}

// 著者 URI が無い actor でもノート自体は保存できること (逆引きだけ張らない)。
func TestStore_PutNote_AuthorWithoutURI(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	u := sampleUser("u1", "")
	u.URI = nil

	require.NoError(t, s.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"), u))
	got, err := s.GetNote(ctx, "n1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.User)
}

// 著者が TTL 切れで引けなくてもノートは落とさない。
// timeline 全体が欠けるより、著者 nil で返す方がまし。
func TestStore_GetNotes_AuthorMissingKeepsNote(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	uri := "https://remote.example/notes/1"
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", uri, "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))
	// 著者キーだけ落とす
	require.NoError(t, testRedis.Client.Del(ctx, "example.com:ephUser:u1").Err())

	got, err := s.GetNotes(ctx, []string{"n1"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].User)
}

// 壊れた JSON が入っていても panic せず、その ID だけ落として返す。
func TestStore_GetNotes_MalformedEntry(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))
	require.NoError(t, testRedis.Client.Set(ctx, "example.com:ephNote:broken", "{not json", time.Minute).Err())

	got, err := s.GetNotes(ctx, []string{"broken", "n1"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "n1", got[0].ID)
}

func TestStore_GetUser_Malformed(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	require.NoError(t, testRedis.Client.Set(ctx, "example.com:ephUser:u1", "{not json", time.Minute).Err())

	got, err := s.GetUser(ctx, "u1")
	assert.Error(t, err)
	assert.Nil(t, got)
}

func TestStore_GetUser_EmptyID(t *testing.T) {
	s := newStore(t, time.Minute)
	got, err := s.GetUser(context.Background(), "")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_LookupID_EmptyURI(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	nid, err := s.NoteIDByURI(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, nid)
}

func TestStore_GetNotes_EmptyInput(t *testing.T) {
	s := newStore(t, time.Minute)
	got, err := s.GetNotes(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// URI 無しでも DropNote が動くこと (逆引きキーを消さないだけ)。
func TestStore_DropNote_WithoutURI(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	require.NoError(t, s.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))

	require.NoError(t, s.DropNote(ctx, "n1", ""))
	got, err := s.GetNote(ctx, "n1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_DropNote_EmptyID(t *testing.T) {
	s := newStore(t, time.Minute)
	assert.NoError(t, s.DropNote(context.Background(), "", "uri"))
}

// URI / UserID を持たないノートでも Touch が落ちないこと。
func TestStore_Touch_MinimalNote(t *testing.T) {
	s := newStore(t, time.Minute)
	assert.NoError(t, s.Touch(context.Background(), &model.Note{ID: "n1"}))
}

// --- 既定無効の担保 ---
//
// sink は機能の有効・無効に関わらず配線されるので、Enabled() が実際に
// 効かないと「既定では挙動が変わらない」という約束が破れる。

func TestStore_Enabled(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	for _, tc := range []struct {
		name     string
		settings func() ephemeral.Settings
		want     bool
	}{
		{"有効", func() ephemeral.Settings { return ephemeral.Settings{Enabled: true} }, true},
		{"無効 (既定)", func() ephemeral.Settings { return ephemeral.Settings{} }, false},
		{"settings 未配線", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ephemeral.NewStore(testRedis.Client, "example.com:", tc.settings)
			assert.Equal(t, tc.want, s.Enabled())
		})
	}
}

func TestStore_Enabled_NilReceiverAndClient(t *testing.T) {
	var s *ephemeral.Store
	assert.False(t, s.Enabled())
	assert.False(t, ephemeral.NewStore(nil, "", func() ephemeral.Settings {
		return ephemeral.Settings{Enabled: true}
	}).Enabled(), "client 未配線なら有効化されていても false")
}

// 添付の印は #2722 の掃除が唯一頼りにする生存判定。note と同じ TTL で立ち、
// Touch で打ち直され、切れたら消える。
func TestStore_LiveFileIDs(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()

	n := sampleNote("n_file", "https://remote.example/notes/file", "u_file")
	n.FileIDs = model.StringArray{"f1", "f2"}
	require.NoError(t, s.PutNote(ctx, n, sampleUser("u_file", "https://remote.example/users/alice")))

	live, err := s.LiveFileIDs(ctx, []string{"f1", "f2", "f3"})
	require.NoError(t, err)
	assert.True(t, live["f1"])
	assert.True(t, live["f2"])
	assert.False(t, live["f3"], "他のノートの添付は印が立たない")

	empty, err := s.LiveFileIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// **Touch は TTL をスライドさせる。** note だけ延ばして印を放置すると、
// 閲覧され続けているノートの添付が掃除の対象になる。
func TestStore_TouchExtendsFileMarks(t *testing.T) {
	s := newStore(t, 2*time.Second)
	ctx := context.Background()

	n := sampleNote("n_touch_file", "https://remote.example/notes/touchfile", "u_touch_file")
	n.FileIDs = model.StringArray{"tf1"}
	require.NoError(t, s.PutNote(ctx, n, sampleUser("u_touch_file", "https://remote.example/users/alice")))

	before := testRedis.Client.TTL(ctx, "example.com:ephFile:tf1").Val()
	require.Greater(t, before, time.Duration(0))

	time.Sleep(1100 * time.Millisecond)
	// 打ち直す前は必ず減っている (この時点で 2 秒に戻っていたら以下の assert が
	// 無意味になる)。
	require.Less(t, testRedis.Client.TTL(ctx, "example.com:ephFile:tf1").Val(), 2*time.Second)

	require.NoError(t, s.Touch(ctx, n))

	// **相対比較 (after > before-1s) にしない。** Redis の TTL は秒粒度なので
	// sleep のブレで変異版でも通ってしまう。打ち直し後の絶対値で見る。
	after := testRedis.Client.TTL(ctx, "example.com:ephFile:tf1").Val()
	assert.Equal(t, 2*time.Second, after, "Touch で印の TTL も打ち直される")
}

// 印が切れたら生存扱いしない (掃除が永久に空振りしないこと)。
func TestStore_LiveFileIDs_ExpiredMarkIsNotLive(t *testing.T) {
	s := newStore(t, time.Second)
	ctx := context.Background()

	n := sampleNote("n_exp_file", "https://remote.example/notes/expfile", "u_exp_file")
	n.FileIDs = model.StringArray{"xf1"}
	require.NoError(t, s.PutNote(ctx, n, sampleUser("u_exp_file", "https://remote.example/users/alice")))

	time.Sleep(1200 * time.Millisecond)
	live, err := s.LiveFileIDs(ctx, []string{"xf1"})
	require.NoError(t, err)
	assert.False(t, live["xf1"])
}

// **Touch は失効した印を復活させない。** Set で打ち直すと、掃除済みの行に
// 対して「生きている」と嘘をつく印が立つ。
func TestStore_TouchDoesNotResurrectExpiredMark(t *testing.T) {
	s := newStore(t, time.Second)
	ctx := context.Background()

	n := sampleNote("n_res_file", "https://remote.example/notes/resfile", "u_res_file")
	n.FileIDs = model.StringArray{"rf1"}
	require.NoError(t, s.PutNote(ctx, n, sampleUser("u_res_file", "https://remote.example/users/alice")))

	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, s.Touch(ctx, n))

	live, err := s.LiveFileIDs(ctx, []string{"rf1"})
	require.NoError(t, err)
	assert.False(t, live["rf1"])
}

// **未配線は error。** 空の live map を返すと、掃除側が候補を全部ゴミと
// 見なして表示中の添付を消す。fail-closed であることを固定する。
func TestStore_LiveFileIDs_NilStore(t *testing.T) {
	var s *ephemeral.Store
	live, err := s.LiveFileIDs(context.Background(), []string{"f1"})
	assert.Error(t, err)
	assert.Nil(t, live)

	// 入力が空でも同じ (呼び出し側が「0 件なら安全」と誤解しないように、
	// 早期 return は未配線判定より前に置かない)。
	live, err = s.LiveFileIDs(context.Background(), nil)
	assert.NoError(t, err, "問い合わせる ID が無いなら Redis を引く必要も無い")
	assert.Empty(t, live)
}

// Redis を引けないときは error を返し、「印が無い」と答えない (#2722)。
// ここが空 map を返すと、Redis 障害中の実行で表示中の添付を全消しする。
func TestStore_LiveFileIDs_RedisFailureIsError(t *testing.T) {
	// 誰も listen していないアドレスへ向けて MGet を失敗させる。
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	defer client.Close()
	s := ephemeral.NewStore(client, "example.com:", func() ephemeral.Settings {
		return ephemeral.Settings{Enabled: true, TTL: time.Minute}
	})

	live, err := s.LiveFileIDs(context.Background(), []string{"f1"})
	assert.Error(t, err)
	assert.Nil(t, live, "失敗時に「印が無い」と答えない")
}
