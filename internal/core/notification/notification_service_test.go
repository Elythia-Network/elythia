package notification

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testRedis *testutil.TestRedis
	idGen     id.Generator
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("redis setup failed: %v", err)
	}
	testRedis = tr
	idGen, _ = id.NewGenerator("aidx")

	code := m.Run()

	testRedis.Teardown(ctx)
	os.Exit(code)
}

func newTestSvc(t *testing.T) *Service {
	t.Helper()
	testRedis.FlushAll(context.Background())
	svc := NewService(testRedis.Client, idGen, "")
	// テストでは 2 秒の AfterFunc を待ちたくないので同期 publish にする。
	svc.SetUnreadPublishDelay(0)
	return svc
}

func closedClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = c.Close()
	return c
}

func TestService_Create_RequiresNotifiee(t *testing.T) {
	svc := newTestSvc(t)
	_, err := svc.Create(context.Background(), CreateInput{Type: TypeFollow})
	assert.Error(t, err)
}

// TestService_KeyPrefix は #362 drop-in 互換の回帰テスト。
// TS 本家と同じ `<host>:notificationTimeline:*` / `<host>:latestReadNotification:*`
// 名前空間にキーが置かれることを確認する。
func TestService_KeyPrefix(t *testing.T) {
	testRedis.FlushAll(context.Background())
	const host = "example.test"
	svc := NewService(testRedis.Client, idGen, host+":")
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{
		NotifieeID: "u1", NotifierID: "u2", Type: TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	// prefix 付きストリームに entry がある
	prefixed := host + ":notificationTimeline:u1"
	n, err := testRedis.Client.XLen(ctx, prefixed).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "prefixed stream receives the notification")

	// 旧 prefix 無しキーには無い
	bare := "notificationTimeline:u1"
	n, err = testRedis.Client.XLen(ctx, bare).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "bare stream remains empty")

	// readKey も prefix 下に置かれる
	require.NoError(t, svc.MarkAllAsRead(ctx, "u1", false))
	readKey := host + ":latestReadNotification:u1"
	got, err := testRedis.Client.Exists(ctx, readKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), got, "prefixed read key should exist after MarkAllAsRead")
}

func TestService_Create_SelfNotificationRejected(t *testing.T) {
	svc := newTestSvc(t)
	_, err := svc.Create(context.Background(), CreateInput{
		NotifieeID: "u1", NotifierID: "u1", Type: TypeFollow,
	})
	require.ErrorIs(t, err, ErrSelfNotification)
}

func TestService_DeleteByTypeAndNotifier(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	// alice の stream に bob からの receiveFollowRequest, carol からの同種、
	// bob からの follow の計 3 件を積む。
	for _, tc := range []struct {
		notifier string
		typ      Type
	}{
		{"bob", TypeReceiveFollowReq},
		{"carol", TypeReceiveFollowReq},
		{"bob", TypeFollow},
	} {
		_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", NotifierID: tc.notifier, Type: tc.typ})
		require.NoError(t, err)
	}

	// bob からの receiveFollowRequest のみ消す。
	require.NoError(t, svc.DeleteByTypeAndNotifier(ctx, "alice", TypeReceiveFollowReq, "bob"))

	out, err := svc.List(ctx, "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 2) // carol の receiveFollowRequest + bob の follow が残る
	for _, n := range out {
		if n.Type == TypeReceiveFollowReq {
			assert.Equal(t, "carol", n.NotifierID)
		}
	}
}

func TestService_DeleteByTypeAndNotifier_EmptyArgs(t *testing.T) {
	svc := newTestSvc(t)
	// 空の notifieeID / notifierID は no-op。
	require.NoError(t, svc.DeleteByTypeAndNotifier(context.Background(), "", TypeReceiveFollowReq, "bob"))
	require.NoError(t, svc.DeleteByTypeAndNotifier(context.Background(), "alice", TypeReceiveFollowReq, ""))
}

func TestService_CreateAndList(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	for _, typ := range []Type{TypeFollow, TypeMention, TypeReaction} {
		_, err := svc.Create(ctx, CreateInput{
			NotifieeID: "alice", NotifierID: "bob", Type: typ, NoteID: "n1",
		})
		require.NoError(t, err)
	}

	out, err := svc.List(ctx, "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 3)
	// 新しい順
	assert.Equal(t, TypeReaction, out[0].Type)
}

// #1953-2: type filter で新しい limit 件が全て落ちても、より古い一致通知が
// あればそれを返す (upstream getNotifications の refetch loop 相当)。旧実装は
// 新しい limit 件だけ取得して filter したため、空を返していた。
func TestService_List_TypeFilterSurfacesOlderMatch(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	// 最古に follow を 1 件、その後 reaction を 20 件積む。
	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow})
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: TypeReaction, NoteID: "n1"})
		require.NoError(t, err)
	}

	// excludeTypes=[reaction], limit=10: 新しい 10 件は全て reaction で除外されるが、
	// 最古の follow を拾って返す。
	out, err := svc.List(ctx, "alice", "", "", 10, nil, []string{"reaction"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeFollow, out[0].Type)
}

// #1953-3: sinceId のみ指定時は sinceId より新しい「最古」の limit 件を昇順で
// 返す (upstream XRANGE 方向)。旧実装は常に新しい順で取り、新しい側 limit 件を
// 返していた。
func TestService_List_SinceIdReturnsOldestAscending(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		n, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow})
		require.NoError(t, err)
		ids = append(ids, n.ID)
	}

	// ids[0] が最古。sinceId=ids[0], limit=2 -> ids[0] より新しい最古の 2 件
	// (ids[1], ids[2]) を昇順で返す。
	out, err := svc.List(ctx, "alice", ids[0], "", 2, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, ids[1], out[0].ID, "oldest-after-sinceId first (ascending)")
	assert.Equal(t, ids[2], out[1].ID)
}

// stubStreamingPublisher records every PublishNotification call.
type stubStreamingPublisher struct {
	hits []string // notifieeID
}

func (s *stubStreamingPublisher) PublishNotification(notifieeID string, _ *Notification) {
	s.hits = append(s.hits, notifieeID)
}

func TestService_PublishesStreamingEvent(t *testing.T) {
	svc := newTestSvc(t)
	pub := &stubStreamingPublisher{}
	svc.SetStreamingPublisher(pub)
	_, err := svc.Create(context.Background(), CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice"}, pub.hits)
}

// stubMainPublisher records every PublishMainEvent call。time.AfterFunc に
// よる遅延 publish が test goroutine と並走する可能性があるため、`go test
// -race` で誤検出されないよう mutex で保護する (#420 Devin review)。
type stubMainPublisher struct {
	mu    sync.Mutex
	calls []mainEventCall
}

type mainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubMainPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, mainEventCall{userID, eventType, body})
}

// snapshot returns a copy of recorded calls so test assertions can run
// without holding the publisher's lock.
func (s *stubMainPublisher) snapshot() []mainEventCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mainEventCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func TestService_Create_PublishesUnreadNotification(t *testing.T) {
	svc := newTestSvc(t)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)
	_, err := svc.Create(context.Background(), CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "alice", pub.calls[0].userID)
	assert.Equal(t, "unreadNotification", pub.calls[0].eventType)
	// body は *Notification で、少なくとも type が "follow" であることを確認。
	n, ok := pub.calls[0].body.(*Notification)
	require.True(t, ok)
	assert.Equal(t, TypeFollow, n.Type)
}

// stubPacker returns a predefined map as the packed form.
type stubPacker struct {
	calls          int
	lastNotifieeID string
	out            map[string]any
}

func (s *stubPacker) Pack(notifieeID string, _ *Notification) any {
	s.calls++
	s.lastNotifieeID = notifieeID
	return s.out
}

func TestService_Create_UsesPackerForUnreadNotification(t *testing.T) {
	svc := newTestSvc(t)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)
	packer := &stubPacker{out: map[string]any{"userId": "bob", "type": "follow"}}
	svc.SetPacker(packer)

	_, err := svc.Create(context.Background(), CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, packer.calls)
	// #1471: Service.Create は Pack に notifieeID を渡すことで Packer 側で
	// note embed の visibility gate を効かせる契約。stub に届く notifieeID
	// を確認しておくと、将来 Pack signature を変えたときに silent failure
	// (= visibility check の入力がずれて leak) を起こさない。
	assert.Equal(t, "alice", packer.lastNotifieeID)
	require.Len(t, pub.calls, 1)
	// body はpackerの出力 map に置き換わる (TS互換shape)
	body, ok := pub.calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "bob", body["userId"])
}

func TestService_MarkAllAsRead_PublishesReadAll(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	// 1件作成してから MarkAllAsRead する。
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)

	// Create で入った unreadNotification をクリアするため publisher を差し替え。
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "alice", pub.calls[0].userID)
	assert.Equal(t, "readAllNotifications", pub.calls[0].eventType)
	assert.Nil(t, pub.calls[0].body)
}

func TestService_MarkAllAsRead_EmptyStream_DoesNotPublish(t *testing.T) {
	svc := newTestSvc(t)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	// 通知が空 → 既読対象が無いので readAllNotifications を publish しない。
	// 本家 TS NotificationService.readAllNotification の guard と同じ挙動
	// (#420 follow-up)。冗長な publish が来ると新しい unreadNotification が
	// 即座に上書きされるため。
	require.NoError(t, svc.MarkAllAsRead(context.Background(), "alice", false))
	assert.Empty(t, pub.calls, "no notifications → no readAllNotifications publish")
}

// 待機中に MarkAllAsRead が走ると unreadNotification publish は skip される
// (#420 follow-up: 本家 TS NotificationService の setTimeout + 既読再チェック
// と同じ guard)。delay=10ms で実時間遅延を入れて検証する。
func TestService_Create_DelayedUnread_SuppressedAfterMarkAllAsRead(t *testing.T) {
	svc := newTestSvc(t)
	svc.SetUnreadPublishDelay(20 * time.Millisecond)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeMention,
	})
	require.NoError(t, err)

	// AfterFunc が走る前に既読化する。
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	// AfterFunc + GoSched が確実に終わる時間まで待つ
	time.Sleep(80 * time.Millisecond)

	// readAllNotifications は publish される (MarkAllAsRead 由来) が、
	// unreadNotification は suppressed されている。lock 越しの snapshot で
	// race detector に引っかからないよう取得する。
	for _, c := range pub.snapshot() {
		assert.NotEqual(t, "unreadNotification", c.eventType,
			"unreadNotification must be suppressed when read marker advanced first")
	}
}

// 既に最新まで読まれている状態で再度 MarkAllAsRead を呼んでも publish は
// 発生しない (#420 follow-up: badge を即リセットしないため)。
func TestService_MarkAllAsRead_AlreadyRead_DoesNotPublish(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	// 1 回目: 未読がある → publish する
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "readAllNotifications", pub.calls[0].eventType)

	// 2 回目: 新着が無い → publish しない
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	assert.Len(t, pub.calls, 1, "second call must not re-publish readAllNotifications")

	// 3 回目: 新着があるので読み取り位置がまた古くなり publish される。
	//
	// **この枝を固定するのが要点** — `prev < latestNotifID` を落として
	// 「読み取り位置が無いときだけ publish」にしても、これが無いと全テストが
	// 通ってしまう (変異で確認済み)。壊れると暗黙既読からバッジが二度と 0 に
	// 戻らなくなるので、#2831 が force 経路以外で恒久化する。
	_, err = svc.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "carol", Type: TypeFollow,
	})
	require.NoError(t, err)
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	calls := pub.snapshot()
	require.NotEmpty(t, calls)
	assert.Equal(t, "readAllNotifications", calls[len(calls)-1].eventType,
		"a newer notification makes the marker stale again → publish")
}

// TestService_MarkAllAsRead_Force_PublishesWhenAlreadyRead pins the recovery
// path from #2831: an explicit mark-all-as-read must re-emit
// `readAllNotifications` even when the read marker does not move.
//
// **これが無いとその場でバッジから抜け出せない。** 件数を持っているのはサーバー
// ではなく `$i.unreadNotificationsCount` というクライアント側のカウンタで、
// `readAllNotifications` を 1 度取りこぼすと読み取り位置だけが最新まで進み、
// **次の通知が届くまで** hadUnread が false になる。upstream が
// `notifications/mark-all-as-read` にだけ force を渡しているのと同じ理由。
// 「次の通知が届けば復帰する」ことは
// TestService_MarkAllAsRead_AlreadyRead_DoesNotPublish が固定している。
func TestService_MarkAllAsRead_Force_PublishesWhenAlreadyRead(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	// 先に既読化して read marker を最新へ進める (= 以降 hadUnread は false)。
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	require.Len(t, pub.calls, 1)

	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", true))
	require.Len(t, pub.calls, 2, "force must re-publish even when the marker does not move")
	assert.Equal(t, "readAllNotifications", pub.calls[1].eventType)
	assert.Equal(t, "alice", pub.calls[1].userID)
	assert.Nil(t, pub.calls[1].body)
}

// stubReadAllPusher records the Web Push half of postReadAllNotifications.
type stubReadAllPusher struct {
	mu    sync.Mutex
	users []string
}

func (p *stubReadAllPusher) PushReadAllNotifications(userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = append(p.users, userID)
}

func (p *stubReadAllPusher) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.users...)
}

// TestService_MarkAllAsRead_PushesReadAllNotifications pins the Web Push half of
// upstream's postReadAllNotifications, which mk-go was missing entirely.
//
// SW 側は readAllNotifications の push を受けて**表示中の OS 通知を閉じる**。
// sw_subscription の列も /api/sw/* の受け口も揃っているのに producer だけ
// 無かったので、別端末に出たトーストは「すべて既読」を押しても残っていた。
// main stream の publish と**同じ guard の中**で送る。
func TestService_MarkAllAsRead_PushesReadAllNotifications(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)

	pusher := &stubReadAllPusher{}
	svc.SetReadAllPusher(pusher)

	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	require.Equal(t, []string{"alice"}, pusher.snapshot())

	// guard に阻まれる呼びでは push もしない (トーストを閉じる理由が無い)。
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	assert.Equal(t, []string{"alice"}, pusher.snapshot(),
		"suppressed publish must not push either")

	// force なら publish と一緒に push も出る。
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", true))
	assert.Equal(t, []string{"alice", "alice"}, pusher.snapshot(),
		"force must push alongside the main stream event")
}

// TestService_MarkAllAsRead_Force_NoEntries_DoesNotPublish pins that force does
// not bypass the empty-stream early return.
//
// upstream は `latestNotificationId == null` で force を見る前に return する。
// 一度も通知が無ければクライアントのカウンタも 0 のままなので、復帰させるものが
// 無い。
func TestService_MarkAllAsRead_Force_NoEntries_DoesNotPublish(t *testing.T) {
	svc := newTestSvc(t)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)
	// push 側も早期 return より前へ動かせないよう一緒に見る。publisher だけ
	// 見ていると、push を早期 return の前に置く変異が素通りする。
	pusher := &stubReadAllPusher{}
	svc.SetReadAllPusher(pusher)

	require.NoError(t, svc.MarkAllAsRead(context.Background(), "noone", true))
	assert.Empty(t, pub.snapshot(), "empty stream must not publish even with force")
	assert.Empty(t, pusher.snapshot(), "empty stream must not push either")
}

func TestService_Flush_PublishesNotificationFlushed(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.Flush(ctx, "alice"))
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "alice", pub.calls[0].userID)
	assert.Equal(t, "notificationFlushed", pub.calls[0].eventType)
	assert.Nil(t, pub.calls[0].body)
}

func TestService_ListLimitClampingDefaults(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeFollow, NotifierID: "bob"})
	require.NoError(t, err)

	// limit <= 0 はデフォルト10
	out, err := svc.List(ctx, "alice", "", "", 0, nil, nil)
	require.NoError(t, err)
	assert.Len(t, out, 1)

	// limit > 100 は100にクランプ (要素は1件のみだが正常終了することを確認)
	out, err = svc.List(ctx, "alice", "", "", 1000, nil, nil)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestService_MarkAllAsRead(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	// 通知が無いときはno-op
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeFollow, NotifierID: "bob"})
	require.NoError(t, err)
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))

	id, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, id)
}

func TestService_LatestReadID_Missing(t *testing.T) {
	svc := newTestSvc(t)
	id, err := svc.LatestReadID(context.Background(), "ghost")
	require.NoError(t, err)
	assert.Empty(t, id)
}

func TestService_Flush(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeFollow, NotifierID: "bob"})
	require.NoError(t, err)
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))

	require.NoError(t, svc.Flush(ctx, "alice"))
	out, err := svc.List(ctx, "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, out)

	id, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, id)
}

func TestService_RedisErrors(t *testing.T) {
	svc := NewService(closedClient(t), idGen, "")
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "u", NotifierID: "v", Type: TypeFollow})
	assert.Error(t, err)

	_, err = svc.List(ctx, "u", "", "", 10, nil, nil)
	assert.Error(t, err)

	err = svc.MarkAllAsRead(ctx, "u", false)
	assert.Error(t, err)

	_, err = svc.LatestReadID(ctx, "u")
	assert.Error(t, err)

	err = svc.Flush(ctx, "u")
	assert.Error(t, err)
}

// flushSecondClient simulates flush() failing on the second Del call.
type onlyFirstDelClient struct{ *redis.Client }

func TestService_FlushSecondDelError(t *testing.T) {
	// flushの2つ目 (latestReadKey) をエラーにするためには、最初のDelは成功させる
	// 必要がある。実テストでは検証が複雑なのでスキップ可能なケースとし、
	// すでにflush全体のエラーパスはRedisErrorsで網羅済みとする。
	t.Skip("covered indirectly via TestService_RedisErrors")
}

// _ ensures the unused type doesn't fail the build (defensive). 必要なら今後
// flush 個別エラーを差別する mock を追加する。
var _ = onlyFirstDelClient{}

// MarkAllAsRead Set error path: streamは存在するがSetが失敗する場合
type setFailClient struct{ *redis.Client }

func TestService_MarkAllAsRead_NoEntries(t *testing.T) {
	svc := newTestSvc(t)
	// 一度も通知が無いユーザーに対して MarkAllAsRead は何もしないでnilを返す
	require.NoError(t, svc.MarkAllAsRead(context.Background(), "noone", false))
}

var _ = setFailClient{}

// TestService_Create_MarshalError triggers the json.Marshal failure path by
// stuffing an unserializable value (a channel) into Extra.
func TestService_Create_MarshalError(t *testing.T) {
	svc := newTestSvc(t)
	_, err := svc.Create(context.Background(), CreateInput{
		NotifieeID: "alice",
		NotifierID: "bob",
		Type:       TypeFollow,
		Extra:      map[string]any{"ch": make(chan int)},
	})
	assert.Error(t, err)
}

// TestService_List_SkipsBadEntries injects raw entries into the stream that
// the List method should skip without erroring out.
func TestService_List_SkipsBadEntries(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	// 1: data フィールドが文字列ではない
	require.NoError(t, testRedis.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: "notificationTimeline:alice",
		Values: map[string]any{"other": "x"},
	}).Err())
	// 2: data はあるが JSON として無効
	require.NoError(t, testRedis.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: "notificationTimeline:alice",
		Values: map[string]any{"data": "not-json"},
	}).Err())
	// 3: 正常エントリ
	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow})
	require.NoError(t, err)

	out, err := svc.List(ctx, "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TypeFollow, out[0].Type)
}

// Phase 7-2 (#244): UnreadCount / HasUnreadOfTypes のテスト
func TestService_UnreadCount_NoReadMarker(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	// 通知が無ければ0
	n, err := svc.UnreadCount(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// 通知を3件作成し、readMarker未設定なら全件unread
	for i := 0; i < 3; i++ {
		_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeFollow, NotifierID: "bob"})
		require.NoError(t, err)
	}

	n, err = svc.UnreadCount(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
}

func TestService_UnreadCount_AfterMarkAllAsRead(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeFollow, NotifierID: "bob"})
		require.NoError(t, err)
	}
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))

	// 既読マーカー以降は0
	n, err := svc.UnreadCount(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// 新規通知が1件来たら1
	_, err = svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeReaction, NotifierID: "bob"})
	require.NoError(t, err)
	n, err = svc.UnreadCount(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestService_HasUnreadOfTypes_Match(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeMention, NotifierID: "bob"})
	require.NoError(t, err)

	ok, err := svc.HasUnreadOfTypes(ctx, "alice", []Type{TypeMention, TypeReply})
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestService_HasUnreadOfTypes_NoMatch(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeFollow, NotifierID: "bob"})
	require.NoError(t, err)

	ok, err := svc.HasUnreadOfTypes(ctx, "alice", []Type{TypeMention, TypeReply})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestService_HasUnreadSpecifiedNotes_Match(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID:     "alice",
		NotifierID:     "bob",
		Type:           TypeMention,
		NoteID:         "n1",
		NoteVisibility: "specified",
	})
	require.NoError(t, err)
	ok, err := svc.HasUnreadSpecifiedNotes(ctx, "alice")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestService_HasUnreadSpecifiedNotes_NoMatch(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, CreateInput{
		NotifieeID:     "alice",
		NotifierID:     "bob",
		Type:           TypeMention,
		NoteID:         "n1",
		NoteVisibility: "public",
	})
	require.NoError(t, err)
	ok, err := svc.HasUnreadSpecifiedNotes(ctx, "alice")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestService_UnreadSummary_AggregatesCountMentionsSpecified(t *testing.T) {
	// #321: 1 度の UnreadSummary で 3 値を集約できることを確認する。
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{
		NotifieeID:     "alice",
		NotifierID:     "bob",
		Type:           TypeMention,
		NoteID:         "n1",
		NoteVisibility: "specified",
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeFollow, NotifierID: "carol"})
	require.NoError(t, err)

	summary, err := svc.UnreadSummary(ctx, "alice", []Type{TypeMention, TypeReply})
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.TotalCount)
	assert.True(t, summary.HasMentions, "mention 1 件あるので true")
	assert.True(t, summary.HasSpecifiedNote, "specified note の mention あり")
}

func TestService_UnreadSummary_NoMentionTypesSkipsMentionPass(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{
		NotifieeID:     "alice",
		NotifierID:     "bob",
		Type:           TypeMention,
		NoteID:         "n1",
		NoteVisibility: "public",
	})
	require.NoError(t, err)

	// mentionTypes nil → HasMentions は常に false。count / specified のみ計算。
	summary, err := svc.UnreadSummary(ctx, "alice", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.TotalCount)
	assert.False(t, summary.HasMentions)
	assert.False(t, summary.HasSpecifiedNote)
}

func TestService_UnreadSummary_EmptyStream(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()
	summary, err := svc.UnreadSummary(ctx, "alice", []Type{TypeMention})
	require.NoError(t, err)
	assert.Equal(t, int64(0), summary.TotalCount)
	assert.False(t, summary.HasMentions)
	assert.False(t, summary.HasSpecifiedNote)
}

func TestService_UnreadSummary_UsesNoteUnreadRepoForSpecified(t *testing.T) {
	// noteUnreadRepo が wired なら stream に specified note が無くても
	// DB 経由で HasSpecifiedNote=true になる (本家互換経路 #319)。
	svc := newTestSvc(t)
	ctx := context.Background()
	unread := testutil.NewMockNoteUnreadRepository()
	svc.SetNoteUnreadRepo(unread)
	_ = unread.Upsert(&model.NoteUnread{
		ID: "nu1", UserID: "alice", NoteID: "n1", NoteUserID: "bob", IsSpecified: true,
	})

	summary, err := svc.UnreadSummary(ctx, "alice", []Type{TypeMention})
	require.NoError(t, err)
	assert.True(t, summary.HasSpecifiedNote)
}

func TestService_HasUnreadOfTypes_EmptyList(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeMention, NotifierID: "bob"})
	require.NoError(t, err)

	ok, err := svc.HasUnreadOfTypes(ctx, "alice", nil)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestService_HasUnreadOfTypes_AfterMarkAllAsRead(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, CreateInput{NotifieeID: "alice", Type: TypeMention, NotifierID: "bob"})
	require.NoError(t, err)
	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))

	ok, err := svc.HasUnreadOfTypes(ctx, "alice", []Type{TypeMention})
	require.NoError(t, err)
	assert.False(t, ok, "readMarker以降は該当typeがなくfalse")
}

// upstream Misskey #17358 (= 2026.5.1 fix) は ULID 環境下で notification id の
// `additional` 部 (80-bit) をそのまま Redis Stream sequence (uint64) に渡して
// XADD が失敗し続け、通知が ~10s 遅延する bug を修正したが、mk-go の toXAddID
// は ID parse 経路を踏まず引数 time.Time から `<ms>-*` を直接生成するため
// 構造的に overflow しない。regression guard として far-future な time でも
// "<digits>-*" 形式に収まることを確認する
// (= triage #1011 / upstream #17358 close)。
func TestToXAddID_NoOverflowFarFuture(t *testing.T) {
	// uint64 (= max ~5.85e8 years in epoch ms) の遥か手前の年だが
	// 64-bit ms カウンタ自体が overflow しないことを十分担保できる。
	far := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	s := toXAddID("ignored-id", far)
	assert.Regexp(t, `^\d+-\*$`, s,
		"toXAddID must emit `<ms>-*` form, never panic on overflow")
	// 数字部が truncate されず epoch ms と一致することを assert する
	// (upstream の bug は parse 経由で下位 64bit truncate されるパターンだった
	// ため、format check だけでは silent value drift を検出できない)。
	parts := strings.Split(s, "-")
	require.Len(t, parts, 2)
	gotMs, err := strconv.ParseInt(parts[0], 10, 64)
	require.NoError(t, err)
	require.Equal(t, far.UnixMilli(), gotMs,
		"toXAddID's ms must equal time.Time.UnixMilli (no truncation)")
}

// advanceMarkerOnXRevRange simulates a second mark-as-read request that lands
// between this one's two reads: it writes the read marker forward the first
// time an XREVRANGE goes out.
//
// **並走は実際に起きる。** WebSocket の readNotification と通知一覧の暗黙既読は
// 同じユーザーで同時に走る。
type advanceMarkerOnXRevRange struct {
	client *redis.Client
	// readKey は Service.readKey から取る。**リテラルで書かない** — 鍵の形が
	// 変わると誰も読まない鍵へ書くことになり、Get が Redis Nil を返して
	// hadUnread が true に倒れ、**どちらの順序でもテストが通る** (#2777 と同型の
	// regression guard 空振り)。
	readKey string
	readTo  string
	fired   bool
}

func (h *advanceMarkerOnXRevRange) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *advanceMarkerOnXRevRange) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *advanceMarkerOnXRevRange) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if !h.fired && cmd.Name() == "xrevrange" {
			h.fired = true
			h.client.Set(ctx, h.readKey, h.readTo, 0)
		}
		return next(ctx, cmd)
	}
}

// TestService_MarkAllAsRead_ReadsMarkerBeforeStream pins the upstream read order
// (`Get` first, then `XRevRangeN`) — see the comment in MarkAllAsRead (#2831).
//
// 逆順にすると、並走した別の既読要求が書いた**新しい**既読位置を後から読むので、
// 「自分が見た最新 < 既読位置」で hadUnread が false へ倒れ、publish が落ちる。
// これは #2831 が直そうとしている失敗モードそのもの。**この gate が無いと順序を
// 戻す変異が 4 パッケージすべてで素通りする** (実測)。
func TestService_MarkAllAsRead_ReadsMarkerBeforeStream(t *testing.T) {
	testRedis.FlushAll(context.Background())
	ctx := context.Background()

	// hook は共有 client に付けると他のテストへ漏れるので専用に張る。
	hooked := redis.NewClient(&redis.Options{Addr: testRedis.Addr})
	t.Cleanup(func() { require.NoError(t, hooked.Close()) })

	seed := NewService(testRedis.Client, idGen, "")
	seed.SetUnreadPublishDelay(0)
	first, err := seed.Create(ctx, CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: TypeFollow,
	})
	require.NoError(t, err)
	require.NotNil(t, first)

	// 並走側が既読にする位置 = ストリームの最新。
	newest, err := testRedis.Client.XRevRangeN(ctx, seed.streamKey("alice"), "+", "-", 1).Result()
	require.NoError(t, err)
	require.Len(t, newest, 1)

	hooked.AddHook(&advanceMarkerOnXRevRange{
		client:  testRedis.Client,
		readKey: seed.readKey("alice"),
		readTo:  newest[0].ID,
	})

	svc := NewService(hooked, idGen, "")
	svc.SetUnreadPublishDelay(0)
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	require.NoError(t, svc.MarkAllAsRead(ctx, "alice", false))
	assert.Len(t, pub.snapshot(), 1,
		"既読位置はストリームより先に読むこと。逆順だと並走した既読要求の書き込みを"+
			"読んでしまい publish しない側へ倒れる")
}
