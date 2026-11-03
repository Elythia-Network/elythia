package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
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
// **Redis の購読はサービスごとに 1 本の接続だけ持ち、トピックへはメモリ上で配る。**
// 購読者のいるトピックだけを、その 1 本の接続で SUBSCRIBE し、届いたメッセージを
// チャネル名でトピックへ振り分ける。upstream も Redis を 1 本だけ subscribe し、
// 接続ごとの EventEmitter へ配る形になっている (StreamingApiServerService)。
//
// かつてはトピックごとに `client.Subscribe` していた。go-redis の PubSub は
// 1 つごとに専用の TCP 接続を張るので、購読者の数と種類に比例して Redis の接続が
// 増え、同じ Redis を使う rate limit・キュー・キャッシュの接続を圧迫しえた。
// 1 本の PubSub に SUBSCRIBE を足していく形なら、購読者が何をしても Redis 側の
// 接続数は増えない。
//
// **`<prefix>*` の PSUBSCRIBE はやめた (#3991)。** fanout はフォロワーごとに
// `stream:homeTimeline:<id>` へ PUBLISH するので、パターンで受けると、誰も
// ストリームに繋いでいない宛先の分まで Redis が全プロセスへ送り、各プロセスは
// 読んでから捨てていた (フォロワー 300 人の投稿 1 回で約 220KB)。トピックの
// 購読者の数を数え、最初の 1 人で SUBSCRIBE、最後の 1 人が抜けたら UNSUBSCRIBE する。
//
// 同じトピックを複数の接続が購読するのは普通の状態 (ローカル/グローバル
// タイムライン、ハッシュタグ、チャンネル、ノートのリアクション購読) なので、
// 解除は「自分のハンドラ」だけを外す (返り値の関数でしか解除できない)。
type PubSubService struct {
	client *redis.Client
	prefix string

	// startOnce は Redis の PubSub と受信 goroutine を最初の Subscribe で 1 回だけ
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
	// Redis の購読の状態。送るのは syncLoop だけ。
	//
	// **判断と送信を 1 つの goroutine にまとめる。** Subscribe と解除がそれぞれ
	// SUBSCRIBE / UNSUBSCRIBE を送ると、最後の購読者が抜けた直後に別の購読者が
	// 来たとき、判断は「UNSUBSCRIBE → SUBSCRIBE」でも送信が入れ替わり、購読者が
	// いるのに購読が外れたままになりうる。ここでは Subscribe と解除は dirty に
	// 印を付けて起こすだけで、syncLoop が subs (購読者のいるトピック) と
	// subscribed (Redis に購読を頼んだトピック) の差だけを送る。解除は Redis を
	// 待たない (ストリームの切断で止まらない)。付けたり外したりを繰り返した分は
	// 差にまとまり、最後の状態だけが送られる。
	subscribed map[string]struct{}
	dirty      map[string]struct{}
	// waiting は、作ったが SUBSCRIBE が効いたことをまだ確かめていないトピック。
	// syncLoop が確かめてから ready を閉じる。
	waiting []waiter
	wake    chan struct{}
	done    chan struct{}
	// barriers は印のチャネル名ごとに、Redis の応答が届いたら閉じるチャネルを持つ。
	barriers   map[string]chan struct{}
	barrierSeq uint64
	// acks は、syncLoop が確かめている最中の SUBSCRIBE のチャネルごとに、Redis が
	// 購読したと応答したかを持つ (receive が書く)。確かめていない間は nil。
	acks map[string]bool
	// unconfirmed は、送ったのに Redis が購読したと応答しなかったチャネル。
	// syncLoop が next の後に送り直す。
	unconfirmed map[string]unconfirmedChannel
	// degraded は、Redis が購読を確かめる応答を返さなくなった印。立っている間は、
	// Subscribe は確かめるのを待たずに戻る。次に確かめられたら下ろす。
	// confirmations は、syncLoop が確かめられた回数。
	degraded      bool
	confirmations uint64
	// confirmTimeout は確かめるのを待つ上限 (0 なら subscribeConfirmTimeout)。
	// テストで縮める。
	confirmTimeout time.Duration

	// overflowMinInterval は、溢れの関数を続けて呼ぶときの最小の間隔
	// (0 なら defaultOverflowMinInterval)。テストで縮める。
	overflowMinInterval time.Duration
	// beforeSync は、syncLoop が送る内容を決めてから送るまでの間に呼ぶ (テストが、
	// 判断と送信の間に別の購読と解除を割り込ませるのに使う)。最初の Subscribe より
	// 前に設定する。
	beforeSync func(subscribe, unsubscribe []string)
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
	// ready は、このトピックの SUBSCRIBE が Redis に効いたら (または確かめるのを
	// 諦めたら) 閉じる。Subscribe はこれを待ってから戻る。
	ready chan struct{}
}

// NewPubSubService creates a new PubSubService.
func NewPubSubService(client *redis.Client, prefix string) *PubSubService {
	return &PubSubService{
		client:     client,
		prefix:     prefix,
		subs:       make(map[string]*topicSubscription),
		subscribed: make(map[string]struct{}),
		dirty:      make(map[string]struct{}),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		barriers:   make(map[string]chan struct{}),

		unconfirmed: make(map[string]unconfirmedChannel),
	}
}

// unconfirmedChannel is a channel Redis did not confirm: how many times it was
// missing although Redis answered, and when to send it again.
type unconfirmedChannel struct {
	misses int
	next   time.Time
}

// waiter is a new topic whose subscription syncLoop has yet to confirm.
type waiter struct {
	ts      *topicSubscription
	channel string
}

func (p *PubSubService) channel(ch string) string {
	return p.prefix + ch
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

// **前提: Redis の ACL はチャネルを接頭辞の単位で許す (`&stream:*` と
// `&internal:*`、または allchannels。docs/configuration.md)。** チャネルの一部だけを
// 許す設定では、拒まれたチャネルを 1 つずつ送り直すたびに go-redis の受信が
// エラーごとに 100ms 眠るので、拒まれるチャネルが多いと受信全体が遅れ続ける。また
// go-redis は拒まれたチャネルも覚えていて、繋ぎ直すと全部を 1 つの SUBSCRIBE で
// 送るので、許されたチャネルまで張られなくなる (#3991 の敵対的レビュー 3 周目)。
// #3991 より前の PSUBSCRIBE `<prefix>*` は、その設定ではそもそも拒まれていた。
//
// start opens the single Redis subscription (with no channel yet), the
// goroutine that routes its messages to topics and the one that sends the
// SUBSCRIBE / UNSUBSCRIBE commands.
//
// go-redis の PubSub は SUBSCRIBE したチャネルを覚え、繋ぎ直すたびに新しい接続で
// まとめて SUBSCRIBE し直す (PubSub.resubscribe)。ただし覚えるのは送った後なので、
// 送った呼び出しの中で繋ぎ直すと、そのチャネルだけは張り直されない。syncLoop が
// Redis の応答で確かめて送り直す。繋ぎ直しの間に PUBLISH されたものが届かないのは、
// PSUBSCRIBE で受けていたときと同じ。
func (p *PubSubService) start() {
	p.startOnce.Do(func() {
		ps := p.client.Subscribe(context.Background())
		// SUBSCRIBE / UNSUBSCRIBE の応答も受ける (confirmSubscribed が使う)。
		msgs := ps.ChannelWithSubscriptions()
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = ps.Close()
			return
		}
		p.ps = ps
		p.mu.Unlock()
		go p.receive(msgs)
		go p.syncLoop(ps)
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
//
// Subscribe returns once Redis has applied the subscription of the channel, so
// a message published after it returns is delivered (or after
// subscribeConfirmTimeout when that could not be confirmed, e.g. while Redis
// is unreachable).
func (p *PubSubService) SubscribeWithOverflow(ctx context.Context, channel string, handler func([]byte), onOverflow func()) func() {
	p.start()
	prefixed := p.channel(channel)

	p.mu.Lock()
	ts, ok := p.subs[prefixed]
	if !ok {
		ts = &topicSubscription{handlers: make(map[uint64]func([]byte)), ready: make(chan struct{})}
		p.subs[prefixed] = ts
		if p.closed || p.ps == nil {
			close(ts.ready)
		} else {
			// **購読者を登録してから SUBSCRIBE を送らせる。** 逆にすると、応答より
			// 先に届いたメッセージを購読者のいないトピックとして捨てる。
			p.dirty[prefixed] = struct{}{}
			p.waiting = append(p.waiting, waiter{ts: ts, channel: prefixed})
			p.wakeSync()
		}
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
	ready := ts.ready
	wait := !p.degraded
	confirmations := p.confirmations
	p.mu.Unlock()

	// **SUBSCRIBE が効くまで戻らない。** PSUBSCRIBE で全部受けていたときは、張った
	// 後の Subscribe は戻った時点で全てを受けていた。トピックごとの SUBSCRIBE で
	// 戻りを待たないと、戻った直後の PUBLISH が SUBSCRIBE より先に実行され、届かない
	// ことがある (PUBLISH は別の接続から来るので、Redis での順序は決まらない)。
	// 2 人目以降も、1 人目の SUBSCRIBE が効くまで待つ。
	//
	// **Redis が応答しない間は待たない (degraded)。** 待つと、新しいトピックごとに
	// 上限まで止まり、ハッシュタグのように順に何本も購読する接続や、起動時の購読が
	// 本数の分だけ遅れる。応答しない間は待っても配信は無く、購読は syncLoop が
	// 送り続けるので、繋がり直せば効く (PSUBSCRIBE の頃と同じく、待たずに戻る)。
	if wait {
		timer := time.NewTimer(p.confirmWait())
		defer timer.Stop()
		select {
		case <-ready:
		case <-ctx.Done():
		case <-timer.C:
			// 待っている間に一度も確かめられなかったときだけ立てる。確かめられて
			// いるのに戻らないのは、このトピックだけが抜けている (送り直している)
			// ときで、Redis は応答している。立てると、Redis が正常なのに次に確かめ
			// られるまで待たない形に落ちる。
			p.markUnconfirmedSince(confirmations)
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() { p.removeHandler(prefixed, id) })
	}
}

// subscribeConfirmTimeout bounds how long Subscribe waits for Redis to confirm
// a new subscription, unless Redis already failed to confirm one (degraded).
//
// 平時は Redis の 1 往復で済む。届かないのは接続が切れているときで、そのときは
// 待っても配信は無い (繋ぎ直しで go-redis が SUBSCRIBE し直す)。ストリームの
// 接続の受付を長く止めないよう、上限を置いて諦める。
const subscribeConfirmTimeout = 2 * time.Second

// wakeSync makes syncLoop look at the dirty topics. Callers hold p.mu.
func (p *PubSubService) wakeSync() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// confirmWait returns how long to wait for Redis to confirm subscriptions.
func (p *PubSubService) confirmWait() time.Duration {
	if p.confirmTimeout > 0 {
		return p.confirmTimeout
	}
	return subscribeConfirmTimeout
}

// recordConfirmation records whether syncLoop's last confirmation went
// through, and logs when that changes whether Redis confirms subscriptions.
//
// degraded は syncLoop と、待ちきれなかった Subscribe の両方が書く。Subscribe の側は
// 「待ち始めてから一度も確かめられていない」ときだけ立てる (markUnconfirmedSince)。
// 確かめられた回数を同じ lock の中で比べるので、syncLoop が下ろした後に Subscribe が
// 古い判断で立て直すことはない。障害の間に購読のたびにログを出すと埋まるので、
// 状態が変わったときだけ出す。
func (p *PubSubService) recordConfirmation(confirmed bool) {
	p.mu.Lock()
	if confirmed {
		p.confirmations++
	}
	p.setDegradedLocked(!confirmed)
}

// markUnconfirmedSince sets degraded when syncLoop has not confirmed anything
// since confirmations was read.
func (p *PubSubService) markUnconfirmedSince(confirmations uint64) {
	p.mu.Lock()
	if p.confirmations != confirmations {
		p.mu.Unlock()
		return
	}
	p.setDegradedLocked(true)
}

// setDegradedLocked sets degraded and unlocks p.mu, logging if it changed.
func (p *PubSubService) setDegradedLocked(v bool) {
	if p.degraded == v || p.closed {
		p.mu.Unlock()
		return
	}
	p.degraded = v
	p.mu.Unlock()
	if v {
		slog.Warn("pubsub: Redis is not confirming subscriptions; Subscribe returns without waiting until it does", "prefix", p.prefix)
	} else {
		slog.Info("pubsub: Redis confirms subscriptions again", "prefix", p.prefix)
	}
}

// Retry intervals of syncLoop for the subscriptions Redis did not confirm.
// A channel missing although Redis answered (its SUBSCRIBE was lost while the
// connection was replaced, or Redis refused it) is sent again after
// resubscribeFirst, doubling each time it is still missing, up to
// resubscribeMax. After a round Redis did not answer, the next one waits at
// least resubscribeAfterTimeout.
//
// **拒まれ続けるチャネルを詰めて送り直さない。** ACL でチャネルごとに許す設定だと、
// 許されないチャネルの SUBSCRIBE は毎回拒まれる (go-redis は拒んだエラーを受信の
// 側で読み捨てるので、拒まれたことは区別できず「抜けた」と見える)。
const (
	resubscribeFirst        = 50 * time.Millisecond
	resubscribeMax          = 30 * time.Second
	resubscribeAfterTimeout = time.Second
	// resubscribeWarnAt は、Redis が応答したのに抜けていた回数がこれに届いたら
	// 1 回だけログに出す回数。繋ぎ直しで抜けただけなら 1 回目の送り直しで効く。
	resubscribeWarnAt = 3
)

// resubscribeDelay returns the interval before sending a channel again that
// was missing misses times although Redis answered.
func resubscribeDelay(misses int) time.Duration {
	d := resubscribeFirst
	for i := 1; i < misses && d < resubscribeMax; i++ {
		d *= 2
	}
	return min(d, resubscribeMax)
}

// syncLoop sends Redis the difference between the topics that have
// subscribers and the ones Redis was asked to subscribe to, and confirms each
// SUBSCRIBE it sent by Redis' reply for that channel. It is the only sender of
// SUBSCRIBE / UNSUBSCRIBE on ps.
//
// **送ったチャネルごとに、Redis の subscribe の応答を確かめる。** go-redis の
// PubSub.Subscribe は送ってからチャネルを覚える。送信が失敗する (または接続が
// 使えなくなっている) と、その中で繋ぎ直して覚えているチャネルを張り直すが、
// 送ろうとしたチャネルはまだ覚えていないので張り直されない。後に送る印の応答は
// 新しい接続で正常に返るので、印だけでは取り違える。印の応答までに subscribe の
// 応答が来なかったチャネルは送り直す (go-redis は覚え済みなので重ねても同じ)。
func (p *PubSubService) syncLoop(ps *redis.PubSub) {
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	for {
		select {
		case <-p.wake:
		case <-retry.C:
		case <-p.done:
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		var subscribe, resend, unsubscribe []string
		for ch := range p.dirty {
			_, want := p.subs[ch]
			_, have := p.subscribed[ch]
			switch {
			case want && !have:
				subscribe = append(subscribe, ch)
				p.subscribed[ch] = struct{}{}
			case !want && have:
				unsubscribe = append(unsubscribe, ch)
				delete(p.subscribed, ch)
			}
		}
		clear(p.dirty)
		// 送り直すのは間隔が過ぎたものだけ (別のトピックの購読で起きた周に、
		// 拒まれ続けるチャネルを毎回送らない)。
		now := time.Now()
		misses := map[string]int{}
		for ch, u := range p.unconfirmed {
			_, want := p.subs[ch]
			_, have := p.subscribed[ch]
			if !want || !have || slices.Contains(subscribe, ch) {
				delete(p.unconfirmed, ch)
				continue
			}
			if now.Before(u.next) {
				continue
			}
			misses[ch] = u.misses
			resend = append(resend, ch)
			delete(p.unconfirmed, ch)
		}
		waiting := p.waiting
		p.waiting = nil
		var acks map[string]bool
		if len(subscribe) > 0 || len(resend) > 0 || len(waiting) > 0 {
			// 送る前に用意する (応答が先に届いても数える)。
			acks = make(map[string]bool, len(subscribe)+len(resend))
			for _, ch := range subscribe {
				acks[ch] = false
			}
			for _, ch := range resend {
				acks[ch] = false
			}
			p.acks = acks
		}
		p.mu.Unlock()

		if p.beforeSync != nil {
			p.beforeSync(subscribe, unsubscribe)
		}
		ctx := context.Background()
		// 1 つのチャネルはどちらか一方にしか入らないので、2 つの間の順は問わない。
		// 失敗はここでは数えない。subscribe は下の応答で確かめ、unsubscribe は
		// go-redis が先に忘れるので、繋ぎ直した接続では張られない。
		if len(unsubscribe) > 0 {
			_ = ps.Unsubscribe(ctx, unsubscribe...)
		}
		if len(subscribe) > 0 {
			_ = ps.Subscribe(ctx, subscribe...)
		}
		// **送り直すチャネルは 1 つずつ送る。** Redis は拒むときコマンドごと拒むので、
		// 拒まれ続けるチャネルと同じコマンドに入れたチャネルまで張られなくなる。
		// 新しいチャネルの中に拒まれるものがあっても、一緒に抜けた分は次の周で
		// 1 つずつ送り直されて効く。
		for _, ch := range resend {
			_ = ps.Subscribe(ctx, ch)
		}
		if acks == nil {
			continue
		}

		confirmed := p.confirmSubscribed(ps)
		p.mu.Lock()
		p.acks = nil
		var warn []string
		for ch, ok := range acks {
			if ok {
				continue
			}
			// 回数は Redis が応答したのに抜けていたときだけ増やす。応答しなかった
			// 周で増やすと、障害の後の送り直しが無駄に遅れる。
			n := max(misses[ch], 1)
			if confirmed && misses[ch] > 0 {
				n = misses[ch] + 1
			}
			d := resubscribeDelay(n)
			if !confirmed {
				d = max(d, resubscribeAfterTimeout)
			}
			p.unconfirmed[ch] = unconfirmedChannel{misses: n, next: time.Now().Add(d)}
			if confirmed && n == resubscribeWarnAt {
				warn = append(warn, ch)
			}
		}
		var release []*topicSubscription
		for _, w := range waiting {
			// Redis が応答しているのに購読が抜けたトピックは、送り直して確かめる
			// まで待たせる (戻った直後の PUBLISH を取りこぼさない)。
			if confirmed && !p.closed && p.subs[w.channel] == w.ts {
				if _, lost := p.unconfirmed[w.channel]; lost {
					p.waiting = append(p.waiting, w)
					continue
				}
			}
			release = append(release, w.ts)
		}
		var next time.Time
		for _, u := range p.unconfirmed {
			if next.IsZero() || u.next.Before(next) {
				next = u.next
			}
		}
		p.mu.Unlock()
		for _, ts := range release {
			close(ts.ready)
		}
		for _, ch := range warn {
			slog.Warn("pubsub: Redis keeps not subscribing a channel (refused by an ACL?); sending it again less often", "channel", ch)
		}
		p.recordConfirmation(confirmed)
		if !next.IsZero() {
			retry.Reset(max(time.Until(next), 0))
		}
	}
}

// isClosed reports whether Close was called. 閉じた後の送信の失敗は、止める途中の
// 巡り合わせなのでログに出さない。
func (p *PubSubService) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// confirmSubscribed waits until Redis has applied every command sent before it
// on ps and reports whether it did within confirmWait.
//
// **印として、購読していない一意のチャネルを UNSUBSCRIBE する。** Redis は
// 1 本の接続のコマンドを順に実行して順に応答するので、印の応答が届いたなら、
// 先に送った SUBSCRIBE は実行済み。SUBSCRIBE 自体の応答を待つ形は、解除と
// 再購読を繰り返したときや、繋ぎ直しで go-redis が SUBSCRIBE し直したときの
// 応答と区別できない。購読していないチャネルの UNSUBSCRIBE は、何も変えずに
// 応答だけ返す。印を送る前に繋ぎ直しが起きても、go-redis は新しい接続で先に
// SUBSCRIBE し直してから印を送るので、印の応答は購読が効いたことを表す。
func (p *PubSubService) confirmSubscribed(ps *redis.PubSub) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	p.barrierSeq++
	// NUL を含む名前は、トピックの名前 (購読される実在のチャネル) と重ならない。
	token := p.prefix + "\x00barrier\x00" + strconv.FormatUint(p.barrierSeq, 10)
	ack := make(chan struct{})
	p.barriers[token] = ack
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.barriers, token)
		p.mu.Unlock()
	}()

	// ログは setDegraded が状態の変わり目に 1 回だけ出す。
	ctx, cancel := context.WithTimeout(context.Background(), p.confirmWait())
	defer cancel()
	if err := ps.Unsubscribe(ctx, token); err != nil {
		return false
	}
	select {
	case <-ack:
		return !p.isClosed()
	case <-ctx.Done():
		return false
	}
}

// receive routes every message of the subscription to its topic.
//
// **ここでハンドラを呼ばない。** 受信は全トピックで 1 本なので、ハンドラを
// 待つと遅いトピック 1 つで全体の配信が止まる。トピックの待ち行列へ積んで、
// 配るのはトピックごとの drain に任せる。
func (p *PubSubService) receive(msgs <-chan any) {
	for m := range msgs {
		switch m := m.(type) {
		case *redis.Message:
			p.enqueue(m.Channel, []byte(m.Payload))
		case *redis.Subscription:
			p.onSubscription(m)
		}
	}
	slog.Debug("pubsub subscription closed", "prefix", p.prefix)
}

// onSubscription records a reply of Redis to SUBSCRIBE / UNSUBSCRIBE: the
// subscription state of a channel syncLoop is confirming, or the reply to a
// barrier, which wakes the confirmSubscribed waiting for it.
//
// 同じチャネルの応答は送った順に届くので、最後に届いた種類が、印の時点での
// そのチャネルの状態になる (古い SUBSCRIBE の応答が遅れて届いても、その後の
// UNSUBSCRIBE の応答で打ち消される)。
//
// **既知の限界**: これは応答が 1 本の接続から来るときの話。印の待ちがタイムアウト
// した後、遅れていた古い接続の subscribe の応答が、繋ぎ直しと重なって次の周の
// 確認の間に届くと、新しい接続では張られていないチャネルを「購読した」と数えうる。
// 印のタイムアウトと繋ぎ直しが同時に起きたときだけの狭い経路で、次に繋ぎ直せば
// go-redis が張り直す。
func (p *PubSubService) onSubscription(m *redis.Subscription) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.acks[m.Channel]; ok {
		p.acks[m.Channel] = m.Kind == "subscribe"
		return
	}
	if m.Kind != "unsubscribe" {
		return
	}
	if ack, ok := p.barriers[m.Channel]; ok {
		close(ack)
		delete(p.barriers, m.Channel)
	}
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
// is gone; syncLoop then unsubscribes it from Redis.
//
// UNSUBSCRIBE が Redis に効くまでに届いた分は、enqueue がトピックの無い
// メッセージとして捨てる。
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
		if !p.closed {
			p.dirty[prefixed] = struct{}{}
			p.wakeSync()
		}
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
	close(p.done)
	ps := p.ps
	p.ps = nil
	for key, ts := range p.subs {
		ts.pending = nil
		delete(p.subs, key)
	}
	for token, ack := range p.barriers {
		close(ack)
		delete(p.barriers, token)
	}
	// syncLoop がまだ受け取っていない分。受け取った分は syncLoop が閉じる。
	for _, w := range p.waiting {
		close(w.ts.ready)
	}
	p.waiting = nil
	p.mu.Unlock()

	if ps == nil {
		return nil
	}
	if err := ps.Close(); err != nil {
		slog.Warn("failed to close pubsub", "prefix", p.prefix, "error", err)
	}
	return nil
}
