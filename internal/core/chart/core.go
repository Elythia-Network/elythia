package chart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// TickFunc returns the current major or minor "snapshot" values for a
// chart. Major ticks run rarely (typically once per resync to fix
// CASCADE-style drift) and overwrite columns with absolute totals;
// minor ticks run as part of every Save() and may be a no-op for charts
// that do all their bookkeeping via Commit(). Either function may be
// nil to indicate that span has nothing to compute.
type TickFunc func(ctx context.Context, group string, major bool) (map[string]int64, error)

// Chart aggregates Commit() / Tick() events into per-hour and per-day
// buckets. The struct is safe for concurrent Commit() calls; Save() and
// Tick() should not run concurrently with each other but may interleave
// freely with Commit() (the buffer is captured atomically).
//
// One Chart instance owns one Schema and one logical chart name. The
// concrete chart wrappers in `internal/core/chart/charts/*` embed (or
// hold) a *Chart and expose typed Update() / Read() / Write() helpers.
type Chart struct {
	schema Schema
	repo   Repository
	lock   Locker
	clock  Clock
	tick   TickFunc

	// logger は Chart を作った時点の slog.Default() を握る。
	//
	// **atomic にすること** (#2872)。warnDropped は ManagementService の
	// ticker goroutine から呼ばれるので、テストが出力先を張り替える形にすると
	// 素の field 書き換えでは `-race` が落ちる。**それは修正前には無かった
	// 競合で、sink の競合を潰した代わりにここへ移すことになる** (実測で確認)。
	//
	// **毎回 slog.Default() を引き直さない** (#2872)。warnDropped は
	// ManagementService の ticker goroutine からも呼ばれる (management.go の
	// loop → SaveAll)。引き直すと「そのとき default だった誰か」の出力先に
	// 書くので、`slog.SetDefault` でグローバルを差し替えて出力を数えるテストに
	// tick が紛れ込む。**件数がずれるだけでなく、sink が並行安全でなければ
	// データ競合になる。**
	//
	// production では `elythia serve` (internal/cli/serve) が起動時に `slog.SetDefault` を
	// 済ませてから chart を組み立てるので挙動は同じ。
	logger atomic.Pointer[slog.Logger]

	bufMu  sync.Mutex
	buffer map[string]*groupBuffer
}

// log returns the logger captured at construction.
//
// **nil へのフォールバックは到達不能だが残す。** `Chart` の composite literal は
// New の中だけで、フィールドは全て unexported なのでパッケージ外からも組み立て
// られない。それでも nil で panic するより既定へ落ちるほうが安全side。
func (c *Chart) log() *slog.Logger {
	if l := c.logger.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// setLogger redirects this chart's output. **テスト専用。** production は New で
// 握った logger を使い続ける。
func (c *Chart) setLogger(l *slog.Logger) { c.logger.Store(l) }

// groupBuffer holds the merged pending diff for one group. Commit() folds
// into it in place, so the retained memory is proportional to the number of
// distinct groups (and distinct unique-column keys) rather than to the number
// of Commit() calls.
//
// 以前は Commit のたびに 1 エントリを slice に append し、畳み込みは Save まで
// 遅延していた。Save は既定 20 分間隔 (ManagementService) なので、滞留量が
// 「20 分間のイベント数」に比例して上限が無かった。bench の heap profile では
// live heap 104MB のうち 44MB がこのバッファで、その大半が users/show ごとに
// Commit される per-user PV chart だった。
type groupBuffer struct {
	// ints は加算可能な列の累計。
	ints map[string]int64
	// uniques は uniqueIncrement 列の集合。[]string の連結ではなく set にする
	// ことで、同じ key が何度 Commit されても滞留量が distinct key 数で頭打ちに
	// なる。bakeUniqueAndIntersection は集合の濃度しか使わないので、重複を
	// 落としても DB に書く値は変わらない。
	uniques map[string]map[string]struct{}
	// attempts は、このバッファが Save に失敗して戻された回数。
	// maxSaveAttempts に達したら捨てる (Save の doc を参照)。
	attempts int
}

// newGroupBuffer returns an empty buffer for one group.
func newGroupBuffer() *groupBuffer {
	return &groupBuffer{ints: make(map[string]int64)}
}

// addInt folds a signed delta into the pending total for one column.
func (b *groupBuffer) addInt(name string, delta int64) {
	b.ints[name] += delta
}

// addUnique adds the given keys to the pending set for one unique column.
func (b *groupBuffer) addUnique(name string, keys []string) {
	set := b.uniques[name]
	if set == nil {
		if b.uniques == nil {
			b.uniques = make(map[string]map[string]struct{}, 1)
		}
		set = make(map[string]struct{}, len(keys))
		b.uniques[name] = set
	}
	for _, k := range keys {
		set[k] = struct{}{}
	}
}

// mergeFrom folds another buffer for the same group into this one. Used when a
// failed Save puts work back while concurrent Commit() calls have already
// started a fresh buffer for that group.
func (b *groupBuffer) mergeFrom(other *groupBuffer) {
	for name, v := range other.ints {
		b.ints[name] += v
	}
	for name, set := range other.uniques {
		if len(set) == 0 {
			continue
		}
		dst := b.uniques[name]
		if dst == nil {
			if b.uniques == nil {
				b.uniques = make(map[string]map[string]struct{}, len(other.uniques))
			}
			dst = make(map[string]struct{}, len(set))
			b.uniques[name] = dst
		}
		for k := range set {
			dst[k] = struct{}{}
		}
	}
}

// uniquesOnly returns a buffer carrying just this one's unique-column sets, or
// nil when there are none. The sets are moved, not copied: the receiver must
// not be used afterwards.
//
// hour 側の UPDATE が通った後に day 側が落ちた group を戻すのに使う。int の
// delta を戻すと次回 hour に二重計上されるが、unique 列は append が行の既存
// 要素を除いて積み、bake も集合から数え直すので**同じ行に対しては冪等**。
// day を取り戻すために unique だけ戻す。
//
// **行をまたぐと冪等ではない。** 再試行が hour 境界をまたぐと、そのキーは次の
// バケットにも計上される (filter も bake も「その行の集合」としか突き合わせない
// ため)。Save 間隔 20 分に対し hour 境界は 60 分ごとなので、再試行 1 回あたり
// 3 回に 1 回ほど踏む。実害は hour 1 バケットの過大計上で、day を丸ごと失うより
// 軽いと判断している。maxSaveAttempts を増やすときはこの確率も増える。
func (b *groupBuffer) uniquesOnly() *groupBuffer {
	if len(b.uniques) == 0 {
		return nil
	}
	out := newGroupBuffer()
	out.uniques = b.uniques
	out.attempts = b.attempts
	return out
}

// Config bundles the dependencies a Chart needs at construction time.
// Required fields: Schema, Repo, Lock. Clock defaults to SystemClock,
// Tick defaults to a no-op.
type Config struct {
	Schema Schema
	Repo   Repository
	Lock   Locker
	Clock  Clock
	Tick   TickFunc
}

// New constructs a Chart from Config.
func New(cfg Config) (*Chart, error) {
	if cfg.Schema.Name == "" {
		return nil, ErrSchemaName
	}
	if cfg.Repo == nil {
		return nil, errors.New("chart: Repo is required")
	}
	if cfg.Lock == nil {
		return nil, errors.New("chart: Lock is required")
	}
	clk := cfg.Clock
	if clk == nil {
		clk = SystemClock{}
	}
	c := &Chart{
		schema: cfg.Schema,
		repo:   cfg.Repo,
		lock:   cfg.Lock,
		clock:  clk,
		tick:   cfg.Tick,
	}
	c.logger.Store(slog.Default())
	return c, nil
}

// BufferedGroups reports how many groups still hold unsaved work. Used by the
// shutdown log: プロセス終了時の失敗は次の周期が無いぶん恒久的なので、
// 「まだ残っていた」ことだけは伝える。
func (c *Chart) BufferedGroups() int {
	c.bufMu.Lock()
	defer c.bufMu.Unlock()
	return len(c.buffer)
}

// Name returns the chart's camelCase name.
func (c *Chart) Name() string { return c.schema.Name }

// IsGrouped reports whether this chart aggregates data into per-group
// buckets (per-user, per-host etc.). Used by the chart cron processor
// to skip charts that cannot be ticked without an external enumeration
// source.
func (c *Chart) IsGrouped() bool { return c.schema.Grouped }

// Commit queues a diff to be merged into the current bucket on the next
// Save(). The diff values may be int, int64, or []string for
// uniqueIncrement columns; values of any other type (int32 / int16 / int8
// included) are silently dropped, as are keys the schema does not know.
//
// `group` must be non-empty when the schema is grouped; it must be
// empty otherwise. Commit returns an error in those mismatch cases so
// callers catch wiring mistakes early.
func (c *Chart) Commit(diff Diff, group string) error {
	if c == nil {
		return nil
	}
	if c.schema.Grouped && group == "" {
		return errors.New("chart: group is required for grouped chart")
	}
	if !c.schema.Grouped && group != "" {
		return errors.New("chart: group must be empty for ungrouped chart")
	}
	c.bufMu.Lock()
	defer c.bufMu.Unlock()

	// 0 や空配列のエントリは省く (本家と同じ挙動)。生き残るエントリが 1 つも
	// 無ければ group のバッファ自体を作らないので、no-op な Commit は何も
	// 確保しない。
	var g *groupBuffer
	for k, v := range diff {
		switch x := v.(type) {
		case int:
			if x == 0 {
				continue
			}
			if g == nil {
				g = c.groupBufferLocked(group)
			}
			g.addInt(k, int64(x))
		case int64:
			if x == 0 {
				continue
			}
			if g == nil {
				g = c.groupBufferLocked(group)
			}
			g.addInt(k, x)
		case []string:
			if len(x) == 0 {
				continue
			}
			if g == nil {
				g = c.groupBufferLocked(group)
			}
			g.addUnique(k, x)
		}
	}
	return nil
}

// groupBufferLocked returns the pending buffer for group, creating it if
// absent. Caller must hold bufMu.
func (c *Chart) groupBufferLocked(group string) *groupBuffer {
	if c.buffer == nil {
		c.buffer = make(map[string]*groupBuffer, 1)
	}
	g, ok := c.buffer[group]
	if !ok {
		g = newGroupBuffer()
		c.buffer[group] = g
	}
	return g
}

// maxSaveAttempts bounds how many consecutive Save cycles one group's buffer may
// fail before it is dropped.
//
// 恒久的に失敗する group がある (例: smallint の桁あふれ、group 名が
// varchar(128) を超える) と、戻し続ける限りそのバッファは伸び続ける。
// 一方で一時的な DB 障害を 1 回で諦めると、従来と同じ取りこぼしになる。
//
// ManagementService の既定間隔は 20 分なので、**最古のデータは最大 40 分粘る**。
// ただし戻したバッファに新しい Commit が合流すると attempts は大きい方を継ぐので、
// **捨てるときは最大 3 周期 (約 60 分) ぶんをまとめて捨てる**。周期 2 以降に
// 入ったデータは 1 回しか試されない。それでも復旧しない障害は、そもそも
// 取りこぼしが問題になる状態ではない。
const maxSaveAttempts = 3

// saveOutcome reports how far a group's save got, so Save knows what can be
// requeued without double counting.
type saveOutcome int

const (
	// saveNothingApplied: 1 行も書いていないので全部戻せる。
	saveNothingApplied saveOutcome = iota
	// saveHourApplied: hour 側の UPDATE が通った (かもしれない) 後で day 側が
	// 落ちた。int の delta を戻すと二重計上になる。
	saveHourApplied
	// saveAllApplied: 両 span とも書けた。
	saveAllApplied
)

// Save flushes the buffered Commit() entries to the database. Steps 1-3 mirror
// the upstream; step 4 and the retry limit do not (see below):
//
//  1. Take the pending per-group buffers.
//  2. For each group, claim the current hour and day rows.
//  3. Push the group's merged int deltas + unique-temp appends + bake
//     operations as two UPDATE statements (hour + day).
//  4. Drop only the work that was applied; a group that failed goes back into
//     the buffer so the next tick retries it.
//
// Save is safe to call concurrently with Commit; it captures the
// buffer under the mutex before processing.
//
// **1 つの group の失敗が他の group を止めない。** 全 group を試みてから
// 集約エラーを返す (upstream も `Promise.all(groups.map(...))` で group ごとに
// 独立して処理する)。以前は最初のエラーで return していたため、恒久的に失敗する
// group が 1 つあるだけで、ソート順で後続の group が二度と flush されなかった。
//
// 戻す範囲は失敗した位置で決まる。1 行も書いていなければ全部、hour だけ通って
// day で落ちたなら unique 列だけ (int の delta は二重計上になる)。戻したバッファは
// maxSaveAttempts 回まで再試行し、それを超えたら捨てて warn を出す。捨てないと
// 恒久的に失敗する group のバッファが際限なく伸びる。
//
// **int の delta は「高々 1 回」から「少なくとも 1 回」に変わる。** UPDATE が
// サーバー側で commit された後に応答を失う (接続断 / timeout) と、次の周期で
// 二重計上される。以前は失われるだけだった。上限は maxSaveAttempts 倍。
// chart は概算値なので、取りこぼしより二重計上を選んでいる。
//
// **maxSaveAttempts で諦めて捨てるのは upstream に無い挙動。** upstream は
// 失敗した group の buffer を保持し続け、上限を持たない。また upstream の
// Promise.all は最初の reject で待たずに返るので、「全 group を試みてから
// 集約エラーを返す」のも mk-go 固有。group ごとに独立して処理する点は同じ。
func (c *Chart) Save(ctx context.Context) error {
	c.bufMu.Lock()
	if len(c.buffer) == 0 {
		c.bufMu.Unlock()
		return nil
	}
	pending := c.buffer
	c.buffer = nil
	c.bufMu.Unlock()

	// map の反復順はランダムなので、処理順が実行ごとに変わらないよう group を
	// ソートしてから回す。空文字列 ("") は ungrouped charts のキー。
	groups := make([]string, 0, len(pending))
	for g := range pending {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	var errs []error
	var dropped []droppedWork
	for _, g := range groups {
		gb := pending[g]
		outcome, err := c.saveGroup(ctx, g, gb)
		if err == nil {
			continue
		}
		errs = append(errs, fmt.Errorf("%s: %w", c.qualify(g), err))
		dropped = append(dropped, c.requeueFailed(g, gb, outcome, err)...)
	}
	c.warnDropped(dropped)
	return joinBounded(errs)
}

// maxReportedGroupErrors bounds how many per-group errors Save folds into its
// returned error.
//
// DB 全断のような状況では失敗 group 数がそのまま件数になる。1 件あたり
// 100 バイト前後なので、perUserPv のように group 数が distinct owner 数まで
// 伸びる chart では**エラー文字列だけで数百 KB** になり、しかも slog の
// TextHandler は改行をエスケープするので 1 物理行に収まってしまう。
// journald (既定 LineMax 48K) でも docker json-file (16K 分割) でも、読みたい
// 代表例がその中に埋もれる。件数だけ残して打ち切る。
const maxReportedGroupErrors = 5

// joinBounded folds at most maxReportedGroupErrors errors, appending a count of
// the rest.
func joinBounded(errs []error) error {
	if len(errs) <= maxReportedGroupErrors {
		return errors.Join(errs...)
	}
	kept := make([]error, 0, maxReportedGroupErrors+1)
	kept = append(kept, errs[:maxReportedGroupErrors]...)
	kept = append(kept, fmt.Errorf("and %d more group(s) failed", len(errs)-maxReportedGroupErrors))
	return errors.Join(kept...)
}

// qualify formats a chart/group pair for error messages. ungrouped chart は
// group が空なので、区切りの "/" を出さない。
func (c *Chart) qualify(group string) string {
	if group == "" {
		return c.schema.Name
	}
	return c.schema.Name + "/" + group
}

// Reasons Save can give up on buffered work.
const (
	// droppedReasonRetryLimit: maxSaveAttempts 回失敗したので諦めた。多くは
	// 一過性の障害で、復旧すれば以後は流れる。
	droppedReasonRetryLimit = "retry limit reached"
	// droppedReasonNotRetryable: hour 側が適用済みなので int の delta を戻すと
	// 二重計上になる。恒久的な失敗 (smallint あふれ等) なら operator の対処が要る。
	droppedReasonNotRetryable = "hour applied, day delta not retryable"
)

// droppedWork records buffered work Save gave up on, for the summary warning.
type droppedWork struct {
	group  string
	reason string
	cause  error
	// ints は捨てた delta の絶対値の和。列数ではない (列数だと groups と並んだ
	// ときに「失ったイベント数」と誤読される)。
	ints int64
	// uniques は捨てた unique キーの数。
	uniques int
}

// droppedIntTotal sums the magnitude of the deltas that applyDiffs would have
// written. 素の和だと減算列 (dec 系) と打ち消し合って「何も失っていない」ように
// 見えるので絶対値で足す。
//
// uniqueIncrement / intersection 列に積まれた int は applyDiffs が deltas から
// 外す (bake した絶対値を SET する) ので、**そもそも書かれない**。捨てた量として
// 数えると過大報告になる。
func (c *Chart) droppedIntTotal(ints map[string]int64) int64 {
	var total int64
	for _, col := range c.schema.Columns {
		if col.UniqueIncrement || col.IntersectionOf != nil {
			continue
		}
		v := ints[col.Name]
		if v < 0 {
			v = -v
		}
		total += v
	}
	return total
}

// droppedUniqueTotal counts the unique keys that applyDiffs would have written.
//
// uniqueAppendsFor と bakeUniqueAndIntersection はどちらも schema の
// uniqueIncrement 列しか見るので、それ以外の名前に積まれたキーは書かれない。
// droppedIntTotal と同じ理由で、捨てた量として数えると過大報告になる。
func (c *Chart) droppedUniqueTotal(uniques map[string]map[string]struct{}) int {
	total := 0
	for _, col := range c.schema.Columns {
		if !col.UniqueIncrement {
			continue
		}
		total += len(uniques[col.Name])
	}
	return total
}

// newDroppedWork snapshots how much of gb is being thrown away.
func (c *Chart) newDroppedWork(group, reason string, cause error, gb *groupBuffer) droppedWork {
	return droppedWork{
		group:   group,
		reason:  reason,
		cause:   cause,
		ints:    c.droppedIntTotal(gb.ints),
		uniques: c.droppedUniqueTotal(gb.uniques),
	}
}

// newDroppedIntWork snapshots only the int deltas, for the case where the unique
// columns are being requeued but the deltas cannot be. ok=false when nothing
// that would have been written is being lost, so no warning is emitted for work
// that carried nothing.
func (c *Chart) newDroppedIntWork(group string, cause error, gb *groupBuffer) (droppedWork, bool) {
	total := c.droppedIntTotal(gb.ints)
	if total == 0 {
		return droppedWork{}, false
	}
	return droppedWork{group: group, reason: droppedReasonNotRetryable, cause: cause, ints: total}, true
}

// warnDropped emits one line per Save summarising the work that was discarded.
//
// **本番でこれが唯一の signal になりうる。** ManagementService の logger は
// router で slog に配線しているが、そちらは「Save がエラーを返した」ことしか
// 伝えず、諦めて捨てたかどうかは区別できない。
//
// group ごとに出さないのは、DB 全断のような状況では grouped chart (12 個中 6 個)
// の全 group が同じ周期で上限に達し、perUserPv なら distinct owner 数ぶんの行が
// 一斉に出るため。件数と代表例に畳む。
func (c *Chart) warnDropped(dropped []droppedWork) {
	if len(dropped) == 0 {
		return
	}
	var ints int64
	var uniques int
	counts := make(map[string]int, 2)
	examples := make(map[string]droppedWork, 2)
	// 1 group が 1 周期で 2 レコード (int の破棄 + retry limit) を出しうるので、
	// レコード数と group 数は一致しない。operator は「何人 / 何ホストが失ったか」
	// で影響範囲を測るので、group は distinct で数える。レコード数は
	// retryLimit + notRetryable で読める。
	seenGroups := make(map[string]struct{}, len(dropped))
	for _, d := range dropped {
		ints += d.ints
		uniques += d.uniques
		counts[d.reason]++
		seenGroups[d.group] = struct{}{}
		if _, seen := examples[d.reason]; !seen {
			examples[d.reason] = d
		}
	}

	// reason ごとに数え、**代表例も reason ごとに出す**。まとめて 1 件だけに
	// すると、DB 障害で 4999 件が retry limit に落ちたところへ smallint あふれが
	// 1 件混ざったときに、対処の要る方が代表例から漏れる。
	args := []any{
		"chart", c.schema.Name,
		"groups", len(seenGroups),
		"retryLimit", counts[droppedReasonRetryLimit],
		"notRetryable", counts[droppedReasonNotRetryable],
		"intDelta", ints,
		"uniqueKeys", uniques,
	}
	for _, r := range []struct{ reason, key string }{
		{droppedReasonRetryLimit, "retryLimitExample"},
		{droppedReasonNotRetryable, "notRetryableExample"},
	} {
		d, ok := examples[r.reason]
		if !ok {
			continue
		}
		args = append(args, r.key+"Group", d.group, r.key+"Error", d.cause)
	}
	c.log().Warn("chart: dropped buffered work that could not be saved", args...)
}

// saveGroup claims the current rows for one group and applies its buffer.
func (c *Chart) saveGroup(ctx context.Context, group string, gb *groupBuffer) (saveOutcome, error) {
	hour, err := c.claimCurrentLog(ctx, group, SpanHour)
	if err != nil {
		return saveNothingApplied, fmt.Errorf("claim hour: %w", err)
	}
	day, err := c.claimCurrentLog(ctx, group, SpanDay)
	if err != nil {
		return saveNothingApplied, fmt.Errorf("claim day: %w", err)
	}
	return c.applyDiffs(ctx, hour, day, gb)
}

// requeueFailed puts a failed group's work back into the live buffer, dropping
// it once it has failed maxSaveAttempts times in a row. The returned records
// describe what was thrown away; **Save aggregates them into a single warning**
// (see warnDropped). ここで直接ログを出さないこと: 1 group が 1 周期で
// 2 レコード (int の破棄 + retry limit) を出しうるうえ、grouped chart では
// group 数ぶんの行が一斉に出る。
func (c *Chart) requeueFailed(group string, gb *groupBuffer, outcome saveOutcome, cause error) []droppedWork {
	var dropped []droppedWork
	retry := gb
	if outcome == saveHourApplied {
		// int の delta は hour に適用済みなので戻せない。**戻すものが他に
		// あるかどうかに関わらず、捨てたことを記録する。** instance chart は
		// unique 列を持たず requests.received が smallint で、day 行は 1 日ぶんを
		// 積むぶん hour より 24 倍あふれやすい。perUserPv のように unique 列を
		// 持つ chart でも、pv.user / pv.visitor のような素のカウンタは同じように
		// day 側で消える。ここを黙って通すと、その日一杯 day が更新されない
		// ことに誰も気付けない。
		if d, ok := c.newDroppedIntWork(group, cause, gb); ok {
			dropped = append(dropped, d)
		}
		retry = gb.uniquesOnly()
		if retry == nil {
			return dropped
		}
	}
	retry.attempts++
	if retry.attempts >= maxSaveAttempts {
		return append(dropped, c.newDroppedWork(group, droppedReasonRetryLimit, cause, retry))
	}

	c.bufMu.Lock()
	defer c.bufMu.Unlock()
	if cur, ok := c.buffer[group]; ok {
		// Save 中に走った Commit が同じ group のバッファを作り直している。
		// 上書きせずマージし、attempts は大きい方を引き継ぐ (合流したあとは
		// 区別が付かないので、捨てる側に倒して滞留を止める)。
		cur.mergeFrom(retry)
		if retry.attempts > cur.attempts {
			cur.attempts = retry.attempts
		}
		return dropped
	}
	if c.buffer == nil {
		c.buffer = make(map[string]*groupBuffer, 1)
	}
	c.buffer[group] = retry
	return dropped
}

// Tick computes a snapshot via the configured TickFunc and writes the
// result directly into the current bucket. The major flag chooses
// between major and minor variants and is forwarded to TickFunc.
func (c *Chart) Tick(ctx context.Context, major bool, group string) error {
	if c.tick == nil {
		return nil
	}
	if c.schema.Grouped && group == "" {
		return errors.New("chart: group is required for grouped chart")
	}
	cols, err := c.tick(ctx, group, major)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	hour, err := c.claimCurrentLog(ctx, group, SpanHour)
	if err != nil {
		return err
	}
	day, err := c.claimCurrentLog(ctx, group, SpanDay)
	if err != nil {
		return err
	}
	if err := c.repo.SetColumns(ctx, SpanHour, hour.ID, cols); err != nil {
		return err
	}
	return c.repo.SetColumns(ctx, SpanDay, day.ID, cols)
}

// Resync runs a major Tick. Exposed for chart management endpoints.
func (c *Chart) Resync(ctx context.Context, group string) error {
	return c.Tick(ctx, true, group)
}

// Clean resets the unique-temp arrays of records older than 1 day but
// younger than 3 days. The intent is to free storage while preserving
// the baked cardinality columns. The bounds match the upstream impl.
func (c *Chart) Clean(ctx context.Context) error {
	cols := c.schema.uniqueColumnNames()
	if len(cols) == 0 {
		return nil
	}
	now := truncateToHour(c.clock.Now())
	gt := now.Unix() - 60*60*24*3
	lt := now.Unix() - 60*60*24
	if err := c.repo.ResetUniqueTempColumns(ctx, SpanHour, gt, lt, cols); err != nil {
		return err
	}
	return c.repo.ResetUniqueTempColumns(ctx, SpanDay, gt, lt, cols)
}

// claimCurrentLog returns the row for the bucket containing now(). If
// no row exists yet a new one is inserted, seeded from the most recent
// row's accumulate columns when present. The insert is wrapped in a
// distributed lock to prevent two writers from creating duplicate rows.
func (c *Chart) claimCurrentLog(ctx context.Context, group string, span Span) (*Row, error) {
	now := truncateToSpan(c.clock.Now(), span)
	ts := now.Unix()
	row, err := c.repo.FindCurrent(ctx, span, group, ts)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, ErrRowNotFound) {
		return nil, err
	}
	// 新規作成パスへ。最近のログから accumulate 列を引き継ぐ。
	latest, err := c.repo.FindLatest(ctx, span, group)
	if err != nil && !errors.Is(err, ErrRowNotFound) {
		return nil, err
	}
	seed := c.newLogValues(latest)

	lockKey := fmt.Sprintf("%s:%d:%s", c.schema.Name, ts, span)
	if group != "" {
		lockKey += ":" + group
	}
	release, err := c.lock.Acquire(ctx, lockKey, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer release()

	// ロック取得後にもう一度確認。レースで他のプロセスが作っていれば
	// その row を返す。
	if existing, err := c.repo.FindCurrent(ctx, span, group, ts); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrRowNotFound) {
		return nil, err
	}

	cols := make(map[string]any, len(seed))
	for k, v := range seed {
		cols[k] = v
	}
	return c.repo.Insert(ctx, span, group, ts, cols)
}

// newLogValues builds the seed map for a new bucket. Accumulate columns
// inherit the previous bucket's value; everything else starts at zero.
// uniqueIncrement columns also start at zero (their temp arrays start
// empty by default in the SQL DDL).
func (c *Chart) newLogValues(latest *Row) map[string]int64 {
	out := make(map[string]int64, len(c.schema.Columns))
	for _, col := range c.schema.Columns {
		if col.Accumulate && latest != nil {
			if v, ok := latest.Cols[col.Name]; ok {
				out[col.Name] = toInt64(v)
				continue
			}
		}
		out[col.Name] = 0
	}
	return out
}

// applyDiffs turns one group's merged buffer into a (deltas,
// uniqueAppends, setInts) tuple per span and pushes it to the
// repository. setInts is used for uniqueIncrement bake and
// intersection columns whose absolute value cannot be expressed as a
// delta.
func (c *Chart) applyDiffs(ctx context.Context, hour, day *Row, gb *groupBuffer) (saveOutcome, error) {
	deltas := make(map[string]int64)
	for _, col := range c.schema.Columns {
		if col.UniqueIncrement || col.IntersectionOf != nil {
			// unique / intersection 列は delta ではなく bake した絶対値を SET
			// する。ここで delta にも入れると 1 本の UPDATE で同じ列に 2 回
			// 代入することになり PostgreSQL が 42601 で落ちる。
			continue
		}
		if n := gb.ints[col.Name]; n != 0 {
			deltas[col.Name] = n
		}
	}

	// 行の unique-temp 集合は span ごとに列単位で 1 度だけ作って使い回す。
	// 同じ列を append の filter・濃度の bake・intersection が引くので、
	// 都度 slice から作り直すと同じ配列を何度も走査することになる。
	// day バケットの配列は本番で数万件になりうるので効いてくる。
	// コピーが要るのは intersection だけ (bake は unionSize で数える)。
	hourIdx := newRowUniqueIndex(hour)
	dayIdx := newRowUniqueIndex(day)

	// unique-temp への append は **span ごとに** 行の既存要素を除いてから積む
	// (upstream core.ts の `v.filter(item => !logHour[temp].includes(item))`)。
	// 行が既に持っているキーを積み直しても濃度は変わらないので、配列が
	// バケット内で伸び続けるだけになる。
	hourAppends := c.uniqueAppendsFor(hourIdx, gb)
	dayAppends := c.uniqueAppendsFor(dayIdx, gb)

	hourSetInts := bakeUniqueAndIntersection(c.schema, hourIdx, gb)
	daySetInts := bakeUniqueAndIntersection(c.schema, dayIdx, gb)

	if err := c.repo.ApplyDeltas(ctx, SpanHour, hour.ID, deltas, hourAppends, hourSetInts); err != nil {
		return saveNothingApplied, err
	}
	if err := c.repo.ApplyDeltas(ctx, SpanDay, day.ID, deltas, dayAppends, daySetInts); err != nil {
		return saveHourApplied, err
	}
	return saveAllApplied, nil
}

// uniqueAppendsFor returns, per uniqueIncrement column, the buffered keys that
// the row does not already carry in its unique-temp array.
func (c *Chart) uniqueAppendsFor(idx *rowUniqueIndex, gb *groupBuffer) map[string][]string {
	appends := make(map[string][]string)
	for _, col := range c.schema.Columns {
		if !col.UniqueIncrement {
			continue
		}
		set := gb.uniques[col.Name]
		if len(set) == 0 {
			continue
		}
		existing := idx.set(col.Name)
		items := make([]string, 0, len(set))
		for k := range set {
			if _, dup := existing[k]; dup {
				continue
			}
			items = append(items, k)
		}
		// 追加するものが無いなら array_cat を打たない (upstream も
		// `if (itemsForHour.length > 0)` で抑止する)。
		if len(items) == 0 {
			continue
		}
		// 生成される SQL を map の反復順に依存させない。
		sort.Strings(items)
		appends[col.Name] = items
	}
	return appends
}

// unionSize returns |existing ∪ buffered| without materialising the union.
func unionSize(existing, buffered map[string]struct{}) int {
	n := len(existing)
	for k := range buffered {
		if _, ok := existing[k]; !ok {
			n++
		}
	}
	return n
}

// rowUniqueIndex memoizes one row's per-column unique-temp sets. A single Save
// looks the same column up from the append filter, the cardinality bake and the
// intersection computation, and the day-bucket arrays can hold tens of
// thousands of keys, so the slice is turned into a set once per column.
type rowUniqueIndex struct {
	row  *Row
	sets map[string]map[string]struct{}
}

// newRowUniqueIndex builds an empty index over row. The backing map and each
// column's set are materialised lazily.
//
// chart schema 12 個のうち unique 列を持つのは activeUsers / federation /
// perUserPv の 3 つだけなので、残り 9 個では index が何も確保しない。
func newRowUniqueIndex(row *Row) *rowUniqueIndex {
	return &rowUniqueIndex{row: row}
}

// set returns the row's persisted unique-temp set for one column.
//
// **The returned map is shared across calls; callers must not modify it.**
// Use clone when the set has to be extended (bakeUniqueAndIntersection does).
//
// scanRow が driver の値を []string に正規化する (#2652) ので、ここでの型
// アサーションは通常成功する。失敗しうるのは driver の返す型が変わったときと、
// 配列リテラルとして読めない形で返ってきたときで、どちらも**自己修復しない**:
// 行側が常に空集合になり、bake が buffer だけの濃度を絶対値で SET し続けるので、
// 既存の正しい値を小さい値で壊す。#2652 と同じ壊れ方なので、toStringSlice 側が
// 両方の経路で warn を出す。
func (r *rowUniqueIndex) set(name string) map[string]struct{} {
	if s, ok := r.sets[name]; ok {
		return s
	}
	v, _ := r.row.Cols[name+":unique"].([]string)
	s := make(map[string]struct{}, len(v))
	for _, x := range v {
		s[x] = struct{}{}
	}
	if r.sets == nil {
		r.sets = make(map[string]map[string]struct{}, 1)
	}
	r.sets[name] = s
	return s
}

// clone returns a fresh mutable copy of the row's set for one column.
func (r *rowUniqueIndex) clone(name string) map[string]struct{} {
	src := r.set(name)
	out := make(map[string]struct{}, len(src))
	for k := range src {
		out[k] = struct{}{}
	}
	return out
}

// bakeUniqueAndIntersection computes the absolute cardinality value for
// each uniqueIncrement and intersection column based on the group's pending
// buffer and the current row's unique-temp arrays. The returned map contains
// only the columns the row should be SET to (not deltas).
func bakeUniqueAndIntersection(schema Schema, idx *rowUniqueIndex, gb *groupBuffer) map[string]int64 {
	out := make(map[string]int64)
	// union は row の unique-temp 配列と、まだ書き込んでいないバッファの集合の和。
	// intersection のループが結果を破壊的に絞り込むので、必ず新しい map を返す。
	// **intersection でしか使わない。** 濃度だけで足りる unique 側は
	// unionSize で数える。
	union := func(name string) map[string]struct{} {
		s := idx.clone(name)
		for x := range gb.uniques[name] {
			s[x] = struct{}{}
		}
		return s
	}
	for _, col := range schema.Columns {
		// **この window に差分があった列だけ** bake する (upstream core.ts の
		// `for (const [k, v] of Object.entries(finalDiffs))`)。差分の無い列まで
		// SET すると、その列に何も起きていない Save が既存の濃度を上書きする。
		//
		// ここで必要なのは濃度だけなので和集合を作らない。行の配列は本番で
		// 数万件になるため、コピーするかどうかが効く。
		if col.UniqueIncrement && len(gb.uniques[col.Name]) > 0 {
			out[col.Name] = int64(unionSize(idx.set(col.Name), gb.uniques[col.Name]))
		}
	}
	// intersection 列は差分の有無にかかわらず毎回 SET する (upstream も
	// `Object.entries(this.schema)` を回す)。row 側の集合が正しく読めていれば
	// 再計算しても同じ値になるので、上書きにはならない。
	for _, col := range schema.Columns {
		if col.IntersectionOf == nil {
			continue
		}
		if len(col.IntersectionOf) == 0 {
			out[col.Name] = 0
			continue
		}
		// intersection は集合演算: union(rowTemp, buffer) を各キーごとに
		// 求めて intersect する。
		current := union(col.IntersectionOf[0])
		for _, k := range col.IntersectionOf[1:] {
			target := union(k)
			for v := range current {
				if _, ok := target[v]; !ok {
					delete(current, v)
				}
			}
		}
		out[col.Name] = int64(len(current))
	}
	return out
}

// GetChart returns the result for the requested span/amount window.
// The semantics mirror upstream:
//
//   - The window is `amount` buckets long, ending at `cursor` if given
//     or now() otherwise.
//   - Missing buckets are interpolated from the most recent prior log
//     so accumulate columns appear continuous.
//   - The result is keyed by dot-notation column names; values are
//     ordered newest-first (index 0 = end, last index = oldest), matching
//     upstream Misskey TS so the frontend's `data.slice().reverse()` lines
//     up with its oldest-first axis labels.
func (c *Chart) GetChart(ctx context.Context, span Span, amount int, cursor *time.Time, group string) (Result, error) {
	if amount <= 0 {
		amount = 1
	}
	// upstream getChartRaw は window 末尾バケット (lt) を、cursor 指定時は
	// truncate(cursor + 1span - 1ms) = ceil(cursor)、未指定 (now) 時は
	// truncate(now) = floor(now) で求める。後者は getCurrentDate 相当。
	// 両者で丸め方向が違うため cursor の有無で分岐する (#1565)。
	var end time.Time
	// gtBase は DB lower-bound (gt) を amount-1 step back する前のアンカー。
	// upstream は cursor 指定時に floor(cursor) + 1span、未指定時は floor(now)
	// から引く。境界一致 cursor では gtBase が end (=ceil(cursor)=floor(cursor))
	// より 1span 新しくなり、gt が最古表示バケットより 1span 新しい位置に来る。
	// 結果 range query が最古バケットの実ログを取りこぼし、outdated-log
	// backfill (logs.at(-1).date == gt) も抑止されて最古バケットが 0/空に
	// 補間される。非境界 cursor / nil now path では gtBase == end なので
	// gt は最古表示バケットと一致し従来挙動を保つ (#1610 / #1565 follow-up)。
	var gtBase time.Time
	if cursor != nil {
		end = ceilToSpan(*cursor, span)
		gtBase = stepBack(truncateToSpan(*cursor, span), -1, span) // floor(cursor) + 1span
	} else {
		now := truncateToSpan(c.clock.Now(), span)
		end = now
		gtBase = now
	}
	gt := stepBack(gtBase, amount-1, span)

	rows, err := c.repo.FindRange(ctx, span, group, gt.Unix(), end.Unix())
	if err != nil {
		return nil, err
	}

	// 範囲外しか持っていなかった場合の補間用 fallback 行
	if len(rows) == 0 {
		fallback, err := c.repo.FindLatest(ctx, span, group)
		if err == nil {
			rows = []*Row{fallback}
		} else if !errors.Is(err, ErrRowNotFound) {
			return nil, err
		}
	} else if rows[len(rows)-1].Date != gt.Unix() {
		// gt バケットにログが無い (range 最古が gt と一致しない) → gt より古い
		// 最新ログを outdated-log として末尾に追加し補間アンカーにする。
		// 逆に gt バケットにログが在るときは backfill を抑止し、最古表示バケット
		// (gt より 1span 古い場合) を 0/空のまま残す (upstream と同じ挙動)。
		anchor, err := c.repo.FindBefore(ctx, span, group, gt.Unix())
		if err == nil {
			rows = append(rows, anchor)
		} else if !errors.Is(err, ErrRowNotFound) {
			return nil, err
		}
	}

	// バケット時刻 (=希望する時刻列) を生成。upstream Misskey TS の
	// `getChartRaw` は `for i := amount-1; i >= 0; i--` + `chart.unshift`
	// で **newest-first** (index 0 = 最新, 末尾 = 最古) の配列を組み立てる。
	// frontend (`MkChart.vue` の `data.slice().reverse()`) はその newest-first
	// 配列を反転して oldest-first labels に揃えるため、API レスポンスが
	// oldest-first で返ると最新値が左端 (90 日前側) に流れて X 軸末端
	// (現在時刻) に届かない症状になる (#470 / #473)。
	out := make(Result, len(c.schema.Columns))
	for _, col := range c.schema.Columns {
		out[col.Name] = make([]int64, amount)
	}
	for i := 0; i < amount; i++ {
		// i=0 → end (最新), i=amount-1 → end-(amount-1)*span (最古)
		bucket := stepBack(end, i, span)
		row := pickRowAt(rows, bucket.Unix())
		if row == nil {
			row = pickFallback(rows, bucket.Unix())
		}
		for _, col := range c.schema.Columns {
			var v int64
			if row != nil {
				if !col.Accumulate {
					// 補間中の bucket は accumulate 以外 0 にする (本家と同じ)
					if row.Date == bucket.Unix() {
						v = toInt64(row.Cols[col.Name])
					}
				} else {
					v = toInt64(row.Cols[col.Name])
				}
			}
			out[col.Name][i] = v
		}
	}
	return out, nil
}

// pickRowAt returns the row whose timestamp matches ts exactly, or nil.
func pickRowAt(rows []*Row, ts int64) *Row {
	for _, r := range rows {
		if r.Date == ts {
			return r
		}
	}
	return nil
}

// pickFallback returns the most recent row whose timestamp is strictly
// before ts. The slice is assumed to be ordered date DESC.
func pickFallback(rows []*Row, ts int64) *Row {
	for _, r := range rows {
		if r.Date < ts {
			return r
		}
	}
	return nil
}
