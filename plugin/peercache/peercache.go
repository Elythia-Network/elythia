// Package peercache caches data fetched from other instances so a plugin can
// answer a view-time read without waiting for the network.
//
// **peer の応答は同期の描画には間に合わない (#2820)。** [plugin.Peer.Send] は
// 積んでから (成功なら秒、失敗を繰り返すと最大 4 回・105 秒かけて)
// [plugin.Peer.OnReply] に返るので、プロフィール
// を開いた瞬間に答えは出ない。「初回は空で返し、届いた分を次から出す」という型が
// 要る。
//
// **空振りも覚える。** 覚えないと、そのプラグインを使っていない利用者の
// プロフィールを開くたびに相手へ問い合わせることになる。
//
// キャッシュはプラグイン自身の schema に置く ([plugin.Storage] の DB)。本体には
// 持たせない — プラグインを外したときに、消し忘れが本体側に残らないようにする
// (mk-go は外した schema を自動では消さず、孤児として運営者に知らせる)。
package peercache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/plugin"
)

// Default TTLs used when Options leaves them zero.
//
// 肯定側を短くしすぎると相手に負荷をかけ、長くしすぎると古い情報が残る。
// 否定側はそれより短くする — 相手が後から使い始めたときに気付けなくなるため。
const (
	DefaultTTL         = 30 * time.Minute
	DefaultNegativeTTL = 10 * time.Minute
)

// askInterval bounds how often the same key is asked for while no reply has
// arrived yet.
//
// **同じ相手に群がらない。** 否定 TTL は応答が届いてから効くので、届く前に
// 大勢が同じプロフィールを開くと問い合わせが重なる。
const askInterval = time.Minute

// Options configures a Cache.
type Options struct {
	// Context is the plugin's own context. Peer / Logger / Go を使う。
	Context plugin.Context
	// DB is the plugin's schema.
	DB *sql.DB
	// Request builds the peer payload for one key. Required.
	Request func(key string) any
	//
	// host と key は**渡された文字列のまま** PK になる。大文字小文字を含めて
	// 揺れると別のエントリになるので、呼び出し側で揃えること (mk-go の Peer は
	// 正規化済みの host を扱うので、そこから取った値ならそのままでよい)。
	// TTL is how long a positive answer is reused. 0 は DefaultTTL。
	//
	// **秒未満は切り捨てで 0 になる** (SQL に秒で渡すため)。1 秒未満を渡すと
	// 常に期限切れとして扱われる。
	TTL time.Duration
	// NegativeTTL is how long "the other side has nothing" is remembered.
	// 0 は DefaultNegativeTTL。
	NegativeTTL time.Duration
}

// Cache is a view-time cache of data fetched from other instances.
type Cache struct {
	ctx         plugin.Context
	db          *sql.DB
	request     func(key string) any
	ttl         time.Duration
	negativeTTL time.Duration
}

// New returns a Cache backed by the plugin's own schema.
func New(o Options) (*Cache, error) {
	if o.Context == nil {
		return nil, fmt.Errorf("peercache: Context が nil です")
	}
	if o.DB == nil {
		return nil, fmt.Errorf("peercache: DB が nil です")
	}
	if o.Request == nil {
		return nil, fmt.Errorf("peercache: Request が nil です")
	}
	c := &Cache{
		ctx:         o.Context,
		db:          o.DB,
		request:     o.Request,
		ttl:         o.TTL,
		negativeTTL: o.NegativeTTL,
	}
	if c.ttl <= 0 {
		c.ttl = DefaultTTL
	}
	if c.negativeTTL <= 0 {
		c.negativeTTL = DefaultNegativeTTL
	}
	return c, nil
}

// Migrations returns the schema this cache needs, numbered from `from`.
//
// **番号はプラグインが決める。** プラグイン自身の migration と衝突しないよう、
// 空いている番号を渡すこと。テーブル名 (`peer_cache` / `peer_cache_pending` /
// `peer_cache_ask`) はこのパッケージの予約。
//
// **消費するのは常に 1 つ。** 返る要素は 1 個で、version は `from` ちょうど。
// 次に使えるのは `from+1`。ここが増えると、後ろに自前の migration を置いた
// プラグインが version の重複で起動しなくなるので、増やさない。
func Migrations(from int) []plugin.Migration {
	return []plugin.Migration{{
		Version: from,
		SQL: `
			CREATE TABLE peer_cache (
				host       text NOT NULL,
				key        text NOT NULL,
				payload    jsonb NOT NULL,
				fetched_at timestamptz NOT NULL DEFAULT now(),
				expires_at timestamptz NOT NULL,
				PRIMARY KEY (host, key)
			);
			CREATE TABLE peer_cache_pending (
				id         text PRIMARY KEY,
				host       text NOT NULL,
				key        text NOT NULL,
				created_at timestamptz NOT NULL DEFAULT now()
			);
			CREATE TABLE peer_cache_ask (
				host     text NOT NULL,
				key      text NOT NULL,
				asked_at timestamptz NOT NULL DEFAULT now(),
				PRIMARY KEY (host, key)
			);
		`,
	}}
}

// Lookup returns the cached payload for host/key.
//
// **無ければ nil を返し、取り寄せは裏で走らせる。** 描画を待たせないため。
// 期限切れでも**古いものは返す** — 空で出すより古い方が読み手には有用で、
// 取り直しは次の表示に間に合う。
//
// 相手がそのプラグインを持っていない場合は、静かに何もしない (普通のこと)。
func (c *Cache) Lookup(ctx context.Context, host, key string) (json.RawMessage, error) {
	payload, stale, err := c.read(ctx, host, key)
	if err != nil {
		return nil, err
	}
	if stale {
		c.askAsync(host, key)
	}
	return payload, nil
}

// Store records the reply for one exchange.
//
// sendID は [plugin.Peer.OnReply] が受け取る id。found が false なら「相手は
// 持っていない」として否定 TTL で覚える。
//
// **知らない id は静かに捨てる。** 期限切れの pending を掃除した後に応答が
// 届くことがあり、それはエラーではない。
func (c *Cache) Store(ctx context.Context, sendID string, payload any, found bool) error {
	host, key, err := c.takePending(ctx, sendID)
	if err != nil {
		return err
	}
	if host == "" {
		return nil
	}

	// **中身が無ければ否定として覚える。** 相手が「持っている」と答えつつ
	// payload が空のことがあるので、found だけでは決めない。
	ttl := c.negativeTTL
	body := json.RawMessage(`null`)
	if found {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("peercache: payload を JSON 化できません: %w", err)
		}
		if string(encoded) != "null" {
			ttl = c.ttl
			body = encoded
		}
	}

	_, err = c.db.ExecContext(ctx, `
		INSERT INTO peer_cache (host, key, payload, fetched_at, expires_at)
		VALUES ($1, $2, $3, now(), now() + make_interval(secs => $4))
		ON CONFLICT (host, key) DO UPDATE SET
			payload = EXCLUDED.payload,
			fetched_at = EXCLUDED.fetched_at,
			expires_at = EXCLUDED.expires_at
	`, host, key, []byte(body), int(ttl.Seconds()))
	if err != nil {
		return fmt.Errorf("peercache: 保存できません: %w", err)
	}
	return nil
}

// Sweep drops rows nobody will read again. Call it from a cron job.
func (c *Cache) Sweep(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM peer_cache WHERE fetched_at < now() - interval '7 days'`); err != nil {
		return fmt.Errorf("peercache: 掃除できません: %w", err)
	}
	// 応答が来なかった pending。**遅れて届いた応答の相関に要る**ので、すぐには
	// 消さない。表が伸び続けるのを止めるためだけの掃除なので 1 日で切る
	// (問い合わせの抑止は peer_cache_ask 側の仕事で、こちらとは無関係)。
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM peer_cache_pending WHERE created_at < now() - interval '1 day'`); err != nil {
		return fmt.Errorf("peercache: 掃除できません: %w", err)
	}
	// 抑止の印。askInterval を過ぎたものは判定に影響しないので落としてよい。
	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM peer_cache_ask WHERE asked_at < now() - interval '1 day'`); err != nil {
		return fmt.Errorf("peercache: 掃除できません: %w", err)
	}
	return nil
}

// read returns the cached payload and whether it should be refetched.
func (c *Cache) read(ctx context.Context, host, key string) (json.RawMessage, bool, error) {
	var payload []byte
	var expired bool
	err := c.db.QueryRowContext(ctx, `
		SELECT payload, expires_at <= now() FROM peer_cache WHERE host = $1 AND key = $2
	`, host, key).Scan(&payload, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("peercache: 読めません: %w", err)
	}
	if string(payload) == "null" {
		// 否定を覚えている。期限内なら問い合わせ直さない。
		return nil, expired, nil
	}
	return payload, expired, nil
}

// askAsync sends the request without blocking the caller.
//
// **描画の途中で待たない。** Peer.Has は nodeinfo をキャッシュしていないと
// 最大 10 秒待つので、view-time の読み取りから同期で呼ぶと画面が固まる。
func (c *Cache) askAsync(host, key string) {
	c.ctx.Go(func() {
		// 呼び出し元の ctx はハンドラが返ると切れるので引き継がない。
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.ask(ctx, host, key)
	})
}

func (c *Cache) ask(ctx context.Context, host, key string) {
	// **1 文で取る。** 判定と記録を分けると、その間に Has (最大 10 秒) と Send が
	// 入るので、同時に開いた view の数だけ問い合わせが並ぶ (実測で 20 件中 20 件が
	// 通った)。ON CONFLICT ... WHERE は行を書き換えられた側にだけ RETURNING が
	// 返るので、勝った 1 つだけが先へ進む。
	//
	// **失敗しても印は残る。** ここで取ってから Has / Send を試すので、相手が
	// 落ちていて毎回失敗する場合も view のたびには飛ばない。
	won, err := c.claimAsk(ctx, host, key)
	if err != nil {
		c.ctx.Logger().Warn("peercache: 問い合わせの権利を取れません", "host", host, "err", err)
		return
	}
	if !won {
		return
	}

	peer := c.ctx.Peer()
	ok, err := peer.Has(ctx, host)
	if err != nil || !ok {
		// 相手が同じプラグインを持っていない。**普通のこと**なので黙って諦める。
		return
	}
	id, err := peer.Send(ctx, host, c.request(key))
	if err != nil {
		c.ctx.Logger().Debug("peercache: 問い合わせを出せませんでした", "host", host, "err", err)
		return
	}
	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO peer_cache_pending (id, host, key, created_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (id) DO NOTHING
	`, id, host, key); err != nil {
		c.ctx.Logger().Warn("peercache: 問い合わせの記録に失敗しました", "id", id, "err", err)
	}
}

// claimAsk reserves the right to ask for host/key, reporting whether it won.
//
// **1 文で完結させる。** ON CONFLICT の WHERE が偽なら行は書き換わらず、
// RETURNING も返らない = 負け。真なら asked_at が進んで勝ち。**同時に走っても
// 勝つのは 1 つだけ。**
func (c *Cache) claimAsk(ctx context.Context, host, key string) (bool, error) {
	var claimed int
	err := c.db.QueryRowContext(ctx, `
		INSERT INTO peer_cache_ask (host, key, asked_at) VALUES ($1, $2, now())
		ON CONFLICT (host, key) DO UPDATE SET asked_at = now()
		WHERE peer_cache_ask.asked_at <= now() - make_interval(secs => $3)
		RETURNING 1
	`, host, key, int(askInterval.Seconds())).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// takePending consumes the pending row for sendID.
func (c *Cache) takePending(ctx context.Context, sendID string) (host, key string, err error) {
	// **一度で消す。** 同じ交換の応答が複数回届くことがある (送信はキューに
	// 載るので、worker が途中で落ちれば積み直される) ので、2 回目以降は
	// 知らない id として静かに捨てる。
	err = c.db.QueryRowContext(ctx,
		`DELETE FROM peer_cache_pending WHERE id = $1 RETURNING host, key`, sendID).Scan(&host, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("peercache: 問い合わせの記録を読めません: %w", err)
	}
	return host, key, nil
}
