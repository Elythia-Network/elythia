package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/misc/redisglob"
)

// topicQueueLimit caps how many undelivered messages one topic may hold.
//
// **溢れたら捨てる。** 受信は 1 本の Redis 接続を全トピックで共有しているので、
// 遅いトピックで受信を待たせると、他のトピックの配信まで止まる。以前の
// 「トピックごとに 1 接続」の形でも、go-redis の受信チャネル (100 件) が 1 分
// 詰まれば捨てていた。違いは、時間ではなく件数で決めること (配っている最中の
// 1 件を除き、上限を超えた分はすぐ捨てる)。
const topicQueueLimit = 1024

// PubSubService provides Redis-backed pub/sub messaging.
//
// **Redis の購読はサービスごとに 1 本だけ持ち、トピックへはメモリ上で配る。**
// `<prefix>*` を PSUBSCRIBE し、届いたメッセージをチャネル名でトピックへ振り
// 分ける。upstream も Redis を 1 本だけ subscribe し、接続ごとの EventEmitter へ
// 配る形になっている (StreamingApiServerService)。
//
// かつてはトピックごとに `client.Subscribe` していた。go-redis の PubSub は
// 1 つごとに専用の TCP 接続を張るので、購読者の数と種類に比例して Redis の接続が
// 増え、同じ Redis を使う rate limit・キュー・キャッシュの接続を圧迫しえた。
// PSUBSCRIBE の 1 本なら、購読者が何をしても Redis 側の接続数は増えない。
//
// 同じトピックを複数の接続が購読するのは普通の状態 (ローカル/グローバル
// タイムライン、ハッシュタグ、チャンネル、ノートのリアクション購読) なので、
// 解除は「自分のハンドラ」だけを外す (返り値の関数でしか解除できない)。
type PubSubService struct {
	client *redis.Client
	prefix string

	// startOnce は PSUBSCRIBE と受信 goroutine を最初の Subscribe で 1 回だけ
	// 起こす。Publish しかしないプロセス (キュー専用など) は購読を張らない。
	startOnce sync.Once
	ps        *redis.PubSub

	mu     sync.Mutex
	closed bool
	subs   map[string]*topicSubscription
	// nextID はサービス全体で単調。**トピックごとに採番しない** — 購読の
	// 張り替えをまたいで同じ id が再利用されると、未呼び出しの解除関数が
	// 別人のハンドラを消す。
	nextID uint64

	// overflowMinInterval は、溢れの関数を続けて呼ぶときの最小の間隔
	// (0 なら defaultOverflowMinInterval)。テストで縮める。
	overflowMinInterval time.Duration
}

// topicSubscription is the in-process state of one topic: its handlers and the
// messages waiting to be handed to them.
type topicSubscription struct {
	handlers map[uint64]func([]byte)
	// pending は配り待ちのメッセージ。**トピックごとに 1 つの drain goroutine
	// だけが順に配る** (順序を保つ)。待ちが無い間は goroutine を持たないので、
	// 名前だけ購読されて何も届かないトピック (乱数のハッシュタグ) は map の
	// 1 項目分しか資源を使わない。
	pending  [][]byte
	draining bool
	dropped  uint64
	// overflow は待ち行列が溢れて捨てたときに呼ぶもの (SubscribeWithOverflow、
	// ハンドラの id ごと)。呼び出しはトピックごとに高々 1 つの goroutine
	// (runOverflow) が順に行う。overflowRunning はその goroutine が居る印、
	// overflowDirty は「最後に関数を呼び始めた後に捨てた」印で、立っていれば
	// 呼び終えた後にもう一周する。
	overflow        map[uint64]func()
	overflowRunning bool
	overflowDirty   bool
}

// NewPubSubService creates a new PubSubService.
func NewPubSubService(client *redis.Client, prefix string) *PubSubService {
	return &PubSubService{
		client: client,
		prefix: prefix,
		subs:   make(map[string]*topicSubscription),
	}
}

func (p *PubSubService) channel(ch string) string {
	return p.prefix + ch
}

// pattern returns the PSUBSCRIBE pattern that matches every channel of this
// service.
//
// **prefix の glob の特殊文字は逃がす。** 逃がさないと、prefix に `*` や `[` が
// あるとき別のサービスや別のアプリのチャネルまで受けてしまう。
func (p *PubSubService) pattern() string {
	return redisglob.Escape(p.prefix) + "*"
}

// Publish sends a message to a channel. The payload is JSON-encoded.
func (p *PubSubService) Publish(ctx context.Context, channel string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("pubsub marshal: %w", err)
	}
	return p.client.Publish(ctx, p.channel(channel), data).Err()
}

// PublishMany sends the same message to every channel. The payload is
// JSON-encoded once. The returned slice is nil when every publish succeeded;
// otherwise it has len(channels) entries and errs[i] is the error of
// channels[i].
//
// フォロワーの home へ配るとき (#3606)、宛先ごとに Publish を呼ぶと、同じ本文の
// json.Marshal と Redis の 1 往復を宛先の数だけ繰り返していた。PUBLISH を
// 1 本の pipeline にまとめる。Redis は 1 本の接続のコマンドを順に実行するので、
// 同じチャネルの中の順序は Publish を順に呼んだときと変わらない。
func (p *PubSubService) PublishMany(ctx context.Context, channels []string, payload any) []error {
	if len(channels) == 0 {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		err = fmt.Errorf("pubsub marshal: %w", err)
		errs := make([]error, len(channels))
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	pipe := p.client.Pipeline()
	cmds := make([]*redis.IntCmd, len(channels))
	for i, ch := range channels {
		cmds[i] = pipe.Publish(ctx, p.channel(ch), data)
	}
	// Exec は最初のエラーしか返さないので、成否はコマンドごとに取る。
	//
	// **既知の差: 送り直しで全チャネルに 2 回届きうる。** go-redis は読み取りの
	// EOF やタイムアウトで pipeline を丸ごと送り直す。Redis が実行済みなら
	// 購読者には同じ本文が 2 回届く。単発の Publish も同じ条件で送り直されて
	// いたので、違うのは重複の単位が 1 件から 1 ページになったこと (#3606)。
	_, _ = pipe.Exec(ctx)
	var errs []error
	for i, cmd := range cmds {
		if err := cmd.Err(); err != nil {
			if errs == nil {
				errs = make([]error, len(channels))
			}
			errs[i] = err
		}
	}
	return errs
}

// start opens the single pattern subscription and the goroutine that routes
// its messages to topics.
func (p *PubSubService) start(ctx context.Context) {
	p.startOnce.Do(func() {
		// go-redis は PSUBSCRIBE が失敗してもパターンを覚え、繋ぎ直しのたびに
		// 張り直す。ここで失敗しても購読は後から効く。
		ps := p.client.PSubscribe(ctx, p.pattern())
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = ps.Close()
			return
		}
		p.ps = ps
		p.mu.Unlock()
		go p.receive(ps)
	})
}

// Subscribe registers handler for the channel and returns the function that
// removes it again.
//
// **解除は返り値でしか行えない。** トピック名を渡して解除する API は、どの
// 購読者を外すのかを表現できず、他人の購読を閉じてしまう。戻り値は何度呼んで
// も安全。
func (p *PubSubService) Subscribe(ctx context.Context, channel string, handler func([]byte)) func() {
	return p.SubscribeWithOverflow(ctx, channel, handler, nil)
}

// SubscribeWithOverflow is Subscribe with a function called when messages of
// the channel were dropped because its queue was full. onOverflow runs on its
// own goroutine, possibly concurrently with handler, and never concurrently
// with itself: per topic one goroutine calls the functions, and drops that
// happen while it is calling them make it call them once more afterwards, at
// least defaultOverflowMinInterval after the previous call. A nil onOverflow
// is the same as Subscribe.
//
// 通常の購読者 (タイムラインなど) は落ちた通知を取り戻す手段が無いので、
// 捨てるだけ (ログは出す)。キャッシュの無効化のように「全部を捨てる」で
// 取りこぼしを埋められる購読者だけが使う (#3664)。
func (p *PubSubService) SubscribeWithOverflow(ctx context.Context, channel string, handler func([]byte), onOverflow func()) func() {
	p.start(ctx)
	prefixed := p.channel(channel)

	p.mu.Lock()
	ts, ok := p.subs[prefixed]
	if !ok {
		ts = &topicSubscription{handlers: make(map[uint64]func([]byte))}
		p.subs[prefixed] = ts
	}
	id := p.nextID
	p.nextID++
	ts.handlers[id] = handler
	if onOverflow != nil {
		if ts.overflow == nil {
			ts.overflow = make(map[uint64]func())
		}
		ts.overflow[id] = onOverflow
	}
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { p.removeHandler(prefixed, id) })
	}
}

// receive routes every message of the pattern subscription to its topic.
//
// **ここでハンドラを呼ばない。** 受信は全トピックで 1 本なので、ハンドラを
// 待つと遅いトピック 1 つで全体の配信が止まる。トピックの待ち行列へ積んで、
// 配るのはトピックごとの drain に任せる。
func (p *PubSubService) receive(ps *redis.PubSub) {
	for msg := range ps.Channel() {
		p.enqueue(msg.Channel, []byte(msg.Payload))
	}
	slog.Debug("pubsub subscription closed", "pattern", p.pattern())
}

func (p *PubSubService) enqueue(prefixed string, payload []byte) {
	p.mu.Lock()
	ts, ok := p.subs[prefixed]
	if !ok || p.closed {
		p.mu.Unlock()
		return
	}
	if len(ts.pending) >= topicQueueLimit {
		ts.dropped++
		n := ts.dropped
		if len(ts.overflow) > 0 {
			// 呼んでいる最中なら印を立てるだけ (終わった後にもう一周する)。
			// 捨てるたびに goroutine を起こすと、詰まっている間は捨てた件数だけ
			// 全体を捨てる呼び出しが並行に走る (#3664 のレビュー 2 周目)。
			ts.overflowDirty = true
			if !ts.overflowRunning {
				ts.overflowRunning = true
				go p.runOverflow(prefixed, ts)
			}
		}
		p.mu.Unlock()
		// 詰まっている間は毎回出さない (ログを埋めない)。
		if n == 1 || n%1000 == 0 {
			slog.Warn("pubsub: topic queue full, dropping messages", "channel", prefixed, "dropped", n)
		}
		return
	}
	ts.pending = append(ts.pending, payload)
	if !ts.draining {
		ts.draining = true
		go p.drain(prefixed, ts)
	}
	p.mu.Unlock()
}

// runOverflow calls the overflow functions of ts until no message was dropped
// since it last started calling them. It is the only caller for its topic
// (overflowRunning).
//
// 印 (overflowDirty) は関数を呼ぶ前に下ろす。呼んでいる間に捨てた分は印を
// 立て直すので、呼び終えた後のもう一周が受け持つ (取りこぼさない)。
func (p *PubSubService) runOverflow(prefixed string, ts *topicSubscription) {
	for {
		p.mu.Lock()
		cur, ok := p.subs[prefixed]
		if !ts.overflowDirty || p.closed || !ok || cur != ts {
			ts.overflowRunning = false
			ts.overflowDirty = false
			p.mu.Unlock()
			return
		}
		ts.overflowDirty = false
		fns := make([]func(), 0, len(ts.overflow))
		for _, fn := range ts.overflow {
			fns = append(fns, fn)
		}
		p.mu.Unlock()
		for _, fn := range fns {
			fn()
		}
		// 呼んだ後は間隔を置いてから、印を見直す。その間に捨てた分は印が残るので、
		// 間隔の後の 1 回が受け持つ (取りこぼさない)。間隔の間はこの goroutine が
		// 居続けるので、捨てるたびに新しい呼び出しが起きることもない。間隔を
		// 置かないと、溢れが続く間は捨てた通知とほぼ同じ回数だけ全体を捨て、その
		// プロセスのキャッシュがほぼ効かなくなる (#3664 のレビュー 3 周目)。
		time.Sleep(p.overflowInterval())
	}
}

// defaultOverflowMinInterval is the shortest time between two calls of the
// overflow functions of one topic while messages keep being dropped.
//
// 溢れは受け側が詰まっている異常時にしか起きず、その間に捨てた通知の分は、
// この間隔だけ遅れて全体を捨てることで埋まる (古い値が残る上限がこの分延びる)。
// 捨てる対象はメモリ上のキャッシュで、毎回作り直しに DB を引かせるので、
// 1 秒に数回で十分。短くしすぎると詰まっている間の負荷を増やすだけになる。
const defaultOverflowMinInterval = 200 * time.Millisecond

func (p *PubSubService) overflowInterval() time.Duration {
	if p.overflowMinInterval > 0 {
		return p.overflowMinInterval
	}
	return defaultOverflowMinInterval
}

// drain hands the pending messages of one topic to its handlers, in order, and
// exits when nothing is left.
//
// **`ts` を引数で受ける。** 解除と再購読でトピックが作り直されたあとは、
// 古い `ts` に残ったメッセージを新しい購読者へ渡さない。
//
// **1 件ずつ取り出し、そのたびにまだ購読中かを見る。** まとめて持ち出すと、
// 持ち出した分は待ち行列の上限に数えられず (上限の倍まで抱える)、解除や Close
// の後も持ち出した分を古いハンドラへ配り続けた。
func (p *PubSubService) drain(prefixed string, ts *topicSubscription) {
	for {
		p.mu.Lock()
		cur, ok := p.subs[prefixed]
		if len(ts.pending) == 0 || p.closed || !ok || cur != ts {
			ts.draining = false
			ts.pending = nil
			p.mu.Unlock()
			return
		}
		payload := ts.pending[0]
		ts.pending[0] = nil
		ts.pending = ts.pending[1:]
		// **ハンドラの呼び出しはロックの外で行う。** ハンドラが Subscribe /
		// 解除を呼ぶ形があり、ロックを持ったまま呼ぶと自己デッドロックする。
		handlers := make([]func([]byte), 0, len(ts.handlers))
		for _, h := range ts.handlers {
			handlers = append(handlers, h)
		}
		p.mu.Unlock()

		for _, h := range handlers {
			h(payload)
		}
	}
}

// removeHandler drops one handler and forgets the topic once its last handler
// is gone.
func (p *PubSubService) removeHandler(prefixed string, id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts, ok := p.subs[prefixed]
	if !ok {
		return
	}
	delete(ts.handlers, id)
	delete(ts.overflow, id)
	if len(ts.handlers) == 0 {
		delete(p.subs, prefixed)
		// 配り待ちは捨てる。drain は `cur == ts` で外れたことに気づく。
		ts.pending = nil
	}
}

// SubscriberCount reports how many handlers are attached to a channel.
// Exposed for tests and diagnostics.
func (p *PubSubService) SubscriberCount(channel string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts, ok := p.subs[p.channel(channel)]
	if !ok {
		return 0
	}
	return len(ts.handlers)
}

// Close closes the Redis subscription and forgets every topic.
func (p *PubSubService) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	ps := p.ps
	p.ps = nil
	for key, ts := range p.subs {
		ts.pending = nil
		delete(p.subs, key)
	}
	p.mu.Unlock()

	if ps == nil {
		return nil
	}
	if err := ps.Close(); err != nil {
		slog.Warn("failed to close pubsub", "pattern", p.pattern(), "error", err)
	}
	return nil
}
