package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/labstack/echo/v4"
)

// operatorProtectedTaskTypes are job types the queue-wide admin operations
// must not touch.
//
// **deliver キューには利用者の不可逆な予定が同居している。**
//
//   - `postScheduledNote`: 促進すると**全利用者の未公開の予約投稿が即時公開**
//     される。消すと二度と発火せず、失敗通知も出ない (下書き行だけが残る)
//   - `deleteAccount`: 消すと「削除フラグだけ立ってノート・ドライブ・フォローが
//     残るアカウント」が生まれる。しかも `WithUnique(24h)` の dedup キーが
//     Redis に残るので、24 時間は再実行しても黙って無視される
//
// upstream はこれらを `postScheduledNote` / `db` の別キューに置いているので、
// deliver の詰まりを流す操作が波及しない。mk-go は deliver に相乗りさせる
// 構成を意図的に選んでいる (docs/divergence.md) ので、代わりにここで守る。
// **キュー構成を変えないのは、管理画面のタブが fork 側の定数から生成されて
// いて、名前を増やすと片側更新になるため。**
var operatorProtectedTaskTypes = map[string]struct{}{
	queue.TaskTypePostScheduledNote: {},
	queue.TaskTypeDeleteAccount:     {},
}

// isOperatorProtectedTask reports whether a queue-wide operation must skip t.
func isOperatorProtectedTask(t *QueueTaskSummary) bool {
	if t == nil {
		return false
	}
	_, protected := operatorProtectedTaskTypes[t.Type]
	return protected
}

// QueueClear handles POST /api/admin/queue/clear.
func (h *Handler) QueueClear(c echo.Context) error {
	// upstream Misskey TS は paramDef で queue + state を required にしている (#929)。
	// mk-go も同 shape に揃え、permissive な「全 queue 一括 clear」を弾く。
	var req struct {
		Queue string `json:"queue"`
		State string `json:"state"`
	}
	if err := c.Bind(&req); err != nil || req.Queue == "" || req.State == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue and state are required."))
	}
	if h.queueInspector == nil {
		return c.NoContent(http.StatusNoContent)
	}
	h.clearQueueState(req.Queue, req.State)
	// **監査に残す。** 連合の配送が丸ごと落ちる操作。
	h.logModeration(c, moderationlog.LogClearQueue, map[string]any{
		"queue": req.Queue,
		"state": req.State,
	})
	return c.NoContent(http.StatusNoContent)
}

// clearQueueState removes jobs of the given Bull state from queue, mirroring
// upstream queue.clean(0,0,state). state '*' clears all clearable states.
// mkq には state 単位の bulk-delete API が無いため、pending は DrainPending、
// それ以外は state 別の list → DeleteTask で消す (best-effort)。
//
// 注意点 (mkq 制約 / upstream との差):
//
// (1) **"wait" は wait バケットしか掃かない。** かつては `DrainPending` を呼んで
// おり、あれは wait に加え paused / prioritized も drain したが、job type を見ない
// ので**予約投稿やアカウント削除まで巻き込んだ** (#3130)。列挙経路
// (`ListPendingTasks`) へ移して保護対象を除けるようにした。あちらは
// `JobBucketWait` だけを見るが、mkq v1.1.0 (BullMQ 6) からは pause してもジョブは
// wait に残るので、pause 中のキューでも掃ける。**v5 時代に paused list へ退避
// されたジョブだけは残る** (Resume が wait へ戻した後なら掃ける)。mk-go は
// deliver/inbox で per-job priority を使わないので prioritized は常に空。
//
// (2) active / paused / prioritized を単独 state で指定した場合は対応する
// bulk-clear 経路が無いため no-op。
//
// (3) cron (repeat) 由来の delayed job は RemoveJob が拒否するため
// clear('delayed') で消えない。
//
// (4) **1 リクエストで消し切らないことがある** (再実行で継続)。上限 1000 反復 ×
// 100 件で約 100k。(1) の変更で wait もこの制限に入った (`DrainPending` は
// 1 回で全部消していた)。
func (h *Handler) clearQueueState(queue, state string) {
	// deleteAll はリストが空になるまで先頭ページを引いて消す。削除で件数が
	// 減るため page=1 を繰り返す。delete が 1 件も進まなければ無限ループを
	// 避けて打ち切る。上限 1000 反復 (= 約 100k tasks) の安全弁付き。
	// **保護を掛けるのは「これから動く」バケットだけ。** completed / failed は
	// 既に終わった記録なので、消せなくすると**終了済みの予約投稿 /
	// アカウント削除が永久に残る** (掃除の目的そのものが果たせない)。
	deleteAll := func(lister func(string, int, int) ([]*QueueTaskSummary, error), protect bool) {
		for i := 0; i < 1000; i++ {
			rows, err := lister(queue, 1, 100)
			if err != nil || len(rows) == 0 {
				return
			}
			progressed := false
			skipped := 0
			for _, t := range rows {
				if protect && isOperatorProtectedTask(t) {
					skipped++
					continue
				}
				if h.queueInspector.DeleteTask(queue, t.ID) == nil {
					progressed = true
				}
			}
			// **保護対象しか残っていなければ打ち切る。** 進捗ゼロで回り続けない。
			if !progressed || skipped == len(rows) {
				return
			}
		}
	}
	// **pending も一括削除ではなく個別に消す。** `DeleteAllPendingTasks` は
	// job type を見ないので、wait へ昇格した予約投稿やアカウント削除まで
	// 巻き込む。保護対象を除いて消すために列挙経路を通す。
	switch state {
	case "*":
		deleteAll(h.queueInspector.ListPendingTasks, true)
		deleteAll(h.queueInspector.ListScheduledTasks, true)
		deleteAll(h.queueInspector.ListRetryTasks, true)
		deleteAll(h.queueInspector.ListFailedTasks, false)
		deleteAll(h.queueInspector.ListCompletedTasks, false)
	case "wait", "waiting", "pending":
		deleteAll(h.queueInspector.ListPendingTasks, true)
	case "delayed":
		deleteAll(h.queueInspector.ListScheduledTasks, true)
		deleteAll(h.queueInspector.ListRetryTasks, true)
	case "failed":
		deleteAll(h.queueInspector.ListFailedTasks, false)
	case "completed":
		deleteAll(h.queueInspector.ListCompletedTasks, false)
	default:
		// active / paused / prioritized は単独 state での bulk-clear 経路が
		// 無いため no-op。pause 中のジョブは wait にあるので "wait" で掃ける
		// (上の注意点 (1))。
	}
}

// QueueDeliverDelayed handles POST /api/admin/queue/deliver-delayed.
//
// Response shape: `[[host, count], ...]` の tuple 配列 (count desc で sort)。
// host は recipient inbox URL (`DeliverPayload.Inbox`) から抽出する。
// upstream Misskey TS spec (`admin/queue/deliver-delayed.ts`) と互換。
//
// admin/control panel の "Errored Instances" panel は b[1] で count に
// アクセスするので、object 配列 (旧 mk-go shape) では NaN 表示になる (#1179)。
func (h *Handler) QueueDeliverDelayed(c echo.Context) error {
	tasks := h.fetchAllDelayedTasks("deliver")
	return c.JSON(http.StatusOK, aggregateByHost(tasks, deliverHostFromPayload))
}

// QueueInboxDelayed handles POST /api/admin/queue/inbox-delayed.
//
// Response shape: `[[host, count], ...]` の tuple 配列 (count desc で sort)。
// host は HTTP Signature header の `keyId` (絶対 URL) から抽出する。
// upstream Misskey TS は `new URL(job.data.signature.keyId).host` と
// 同等の処理。mk-go の `InboxPayload.Host` は enqueue 時に未設定なので
// signature header 経由でしか引けない (#1179)。
func (h *Handler) QueueInboxDelayed(c echo.Context) error {
	tasks := h.fetchAllDelayedTasks("inbox")
	return c.JSON(http.StatusOK, aggregateByHost(tasks, inboxHostFromPayload))
}

// delayedTasksFetchPageSize は scheduled / retry を page 走査する際の 1 page
// 当たり件数。100 は driver 側の page size 上限 (mkq は 100 超を 30 に丸める)
// に収まる大きさ。
const delayedTasksFetchPageSize = 100

// delayedTasksMaxPages は scheduled / retry それぞれの page 走査の上限。
// = 100K tasks per state, 200K total。これは defense-in-depth の二重目的:
//
//   - inspector が常に full page を返すような misbehavior 時の無限ループ防御
//   - 連合先長期 down 等の pathological case で、admin panel が backend
//     memory を食い潰さない soft cap
//
// 通常の運用では数千件で頭打ちになるはずなので、この上限を踏むのは異常状態。
// 到達時は slog.Warn で diag を残し、収集できた分だけで集計を進める
// (= panel に top hosts は出るので、admin はその情報を元に対処できる)。
//
// var にしているのは test seam: cap 防御の挙動を確認する test が小さい
// cap を一時設定するため。production code から書き換えてはならない。
var delayedTasksMaxPages = 1000

// fetchAllDelayedTasks fetches every scheduled+retry task for the queue by
// iterating pages of size delayedTasksFetchPageSize until each list is
// exhausted or delayedTasksMaxPages の上限に達するまで。結果は downstream の
// host aggregation の union として返す。
//
// Misskey TS の BullMQ getJobs(['delayed']) は全件取得が前提なので、通常
// page が空になるまで走査するが、defense-in-depth として上限を設けている
// (`delayedTasksMaxPages` のコメント参照)。
func (h *Handler) fetchAllDelayedTasks(queueName string) []*QueueTaskSummary {
	if h.queueInspector == nil {
		return nil
	}
	var all []*QueueTaskSummary
	for _, spec := range []struct {
		state string
		fetch func(string, int, int) ([]*QueueTaskSummary, error)
	}{
		{"scheduled", h.queueInspector.ListScheduledTasks},
		{"retry", h.queueInspector.ListRetryTasks},
	} {
		for page := 1; page <= delayedTasksMaxPages; page++ {
			rows, err := spec.fetch(queueName, page, delayedTasksFetchPageSize)
			if err != nil || len(rows) == 0 {
				break
			}
			all = append(all, rows...)
			// partial page を取った時点で正常にデータ尽きと判断、warn せず
			// 抜ける。full page が続いたまま cap に到達した時だけ warn する
			// (= データがまだ残っている可能性が高い異常状態)。
			if len(rows) < delayedTasksFetchPageSize {
				break
			}
			if page == delayedTasksMaxPages {
				slog.Warn("admin/queue/delayed: page cap reached, truncating aggregation",
					"queue", queueName, "state", spec.state,
					"maxPages", delayedTasksMaxPages,
					"pageSize", delayedTasksFetchPageSize,
					"collected", len(all))
			}
		}
	}
	return all
}

// aggregateByHost reduces tasks to (host, count) tuples sorted by count
// descending, then host ascending for stable output. hostFn は task 1 件から
// host 文字列を抽出する関数で、空文字列を返した task は skip される
// (= 不正 payload / 集計対象外を弾く)。
//
// 戻り値は []any でなく [][]any なので、frontend が `b[1]` でアクセスする
// JSON tuple 表現 (`[["a", 1], ...]`) になる。
func aggregateByHost(tasks []*QueueTaskSummary, hostFn func(*QueueTaskSummary) string) [][]any {
	counts := make(map[string]int)
	for _, t := range tasks {
		host := hostFn(t)
		if host == "" {
			continue
		}
		counts[host]++
	}
	hosts := make([]string, 0, len(counts))
	for h := range counts {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool {
		if counts[hosts[i]] != counts[hosts[j]] {
			return counts[hosts[i]] > counts[hosts[j]]
		}
		return hosts[i] < hosts[j]
	})
	out := make([][]any, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, []any{h, counts[h]})
	}
	return out
}

// deliverHostFromPayload extracts the recipient host from a deliver queue
// task. DeliverPayload.Inbox は recipient の AP inbox URL なので、その host
// が即 federation 先 host になる。
//
// 注意: Go の `url.Parse` は JS の `new URL()` より permissive で、ほとんどの
// malformed input に対して err=nil + Host="" の `*url.URL` を返す (err は
// `::scheme::` 形式の壊れた scheme prefix 等、限定的な case でのみ返る)。
// caller (`aggregateByHost`) は host == "" を skip するので、err 分岐と
// 空 Host のどちらも同じ「集計から外す」挙動になり、両方 cover している。
func deliverHostFromPayload(t *QueueTaskSummary) string {
	if t == nil || len(t.Payload) == 0 {
		return ""
	}
	var p queue.DeliverPayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		return ""
	}
	u, err := url.Parse(p.Inbox)
	if err != nil {
		return ""
	}
	return u.Host
}

// inboxHostFromPayload extracts the sender host from an inbox queue task.
// InboxPayload.Host は enqueue 時に未設定 (api/inbox/handler.go) なので、
// `Headers["Signature"]` から HTTP Signature の keyId URL を取って host を
// 抽出する。これは upstream Misskey TS の
// `new URL(job.data.signature.keyId).host` と一致する。
func inboxHostFromPayload(t *QueueTaskSummary) string {
	if t == nil || len(t.Payload) == 0 {
		return ""
	}
	var p queue.InboxPayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		return ""
	}
	sig := p.Headers["Signature"]
	if sig == "" {
		return ""
	}
	parsed, err := activitypub.ParseSignatureHeader(sig)
	if err != nil || parsed == nil {
		return ""
	}
	u, err := url.Parse(parsed.KeyID)
	if err != nil {
		return ""
	}
	return u.Host
}

// QueueJobs handles POST /api/admin/queue/jobs.
//
// frontend admin/job-queue.vue は state を Bull の state 名配列で送る
// (`['completed', 'failed', 'active', 'delayed', 'wait']` など)。
// mk-go は Bull state 名を driver の list 呼び出しにマッピングする。
// 合計 limit を超えないよう走査中に切り詰める。
func (h *Handler) QueueJobs(c echo.Context) error {
	// state は string でも string[] でも受け取れるようにする (frontend は
	// 配列、既存テストや admin CLI からは単一文字列でくる可能性がある)。
	var req struct {
		Queue  string          `json:"queue"`
		State  json.RawMessage `json:"state"`
		Limit  int             `json:"limit"`
		Page   int             `json:"page"`
		Search string          `json:"search"`
	}
	if err := c.Bind(&req); err != nil || req.Queue == "" || len(req.State) == 0 {
		// upstream Misskey TS は paramDef で queue + state を required にしている (#929)。
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue and state are required."))
	}
	if h.queueInspector == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	states := parseStateField(req.State)
	terms := searchTerms(req.Search)
	if len(terms) > 0 {
		// upstream queueGetJobs の search 経路は paramDef に limit/page を持たず、
		// 1000 件取得→filter→最大 100 件返す。専用ページング経路に委譲する。
		return c.JSON(http.StatusOK, h.searchQueueJobs(req.Queue, states, terms))
	}
	// **limit / page は mk-go の拡張で、upstream の paramDef には無い。** 同梱
	// フロントは渡さないので、未指定のときは upstream と同じ形で返す (#3167)。
	// 以前は既定の 30 件が全 state 合計に効き、state を順に詰めるので「すべて」
	// タブが完了済みだけで埋まって失敗が 1 件も見えなかった。
	if req.Limit == 0 && req.Page == 0 {
		return c.JSON(http.StatusOK, h.upstreamQueueJobs(req.Queue, states))
	}
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 30
	}
	if req.Page < 1 {
		req.Page = 1
	}

	seen := make(map[string]struct{}, req.Limit)
	out := make([]map[string]any, 0, req.Limit)
outer:
	for _, state := range states {
		rows, err := h.listTasksForState(req.Queue, state, req.Page, req.Limit)
		if err != nil {
			continue
		}
		for _, t := range rows {
			if len(out) >= req.Limit {
				break outer
			}
			// state指定が複数重なる (例: all タブ) とき同じ task が
			// 重複しないよう ID で de-dup する。
			if _, dup := seen[t.ID]; dup {
				continue
			}
			seen[t.ID] = struct{}{}
			out = append(out, packTaskSummary(t))
		}
	}
	return c.JSON(http.StatusOK, out)
}

// upstreamPerStateLimit is how many jobs upstream returns per state:
// `queue.getJobs(types, 0, 100)` is end-inclusive, so up to 101.
const upstreamPerStateLimit = 101

// upstreamQueueJobs reproduces upstream queueGetJobs の search 無し経路。
// BullMQ の getJobs は state ごとに新しい順で [0, 100] を取り、state の指定順に
// 連結して ID で重複を除く。**合計では切らない** — 切ると先頭の state だけで
// 埋まり、後ろの state (「すべて」タブの failed など) が見えなくなる。
func (h *Handler) upstreamQueueJobs(queue string, states []string) []map[string]any {
	seen := make(map[string]struct{})
	out := make([]map[string]any, 0)
	for _, state := range states {
		for _, t := range h.listTasksUpTo(queue, state, upstreamPerStateLimit) {
			if _, dup := seen[t.ID]; dup {
				continue
			}
			seen[t.ID] = struct{}{}
			out = append(out, packTaskSummary(t))
		}
	}
	return out
}

// listTasksUpTo collects up to n tasks of one state, newest first, paging
// past the driver's page size cap (100).
func (h *Handler) listTasksUpTo(queue, state string, n int) []*QueueTaskSummary {
	const pageSize = 100 // driver の page size 上限
	out := make([]*QueueTaskSummary, 0, min(n, pageSize))
	for page := 1; len(out) < n; page++ {
		rows, err := h.listTasksForState(queue, state, page, pageSize)
		if err != nil || len(rows) == 0 {
			break
		}
		out = append(out, rows[:min(len(rows), n-len(out))]...)
		if len(rows) < pageSize {
			break
		}
	}
	return out
}

// searchQueueJobs reproduces upstream queueGetJobs の search 経路。各 state を
// 100 件 (driver の page size 上限) ずつ最大 searchMaxPages ページ取得して候補
// プールを ~1000 件まで広げ、JSON 表現に全 term を含む job だけを最大
// searchReturnLimit (=100) 件返す。driver の pageSize>100 clamp を跨ぐため
// 単一呼び出しでなくページングで候補を集める。
func (h *Handler) searchQueueJobs(queue string, states, terms []string) []map[string]any {
	const searchReturnLimit = 100 // upstream RETURN_LIMIT
	// upstream は getJobs(types, 0, 1000) = state ごとに新しい順で最大 1001 件
	const searchPoolPerState = 1001
	seen := make(map[string]struct{}, searchReturnLimit)
	out := make([]map[string]any, 0, searchReturnLimit)
	for _, state := range states {
		for _, t := range h.listTasksUpTo(queue, state, searchPoolPerState) {
			if len(out) >= searchReturnLimit {
				return out
			}
			if _, dup := seen[t.ID]; dup {
				continue
			}
			packed := packTaskSummary(t)
			if !jobMatchesSearch(packed, terms) {
				continue
			}
			seen[t.ID] = struct{}{}
			out = append(out, packed)
		}
	}
	return out
}

// searchTerms lowercases and splits a search query into whitespace-separated
// terms (upstream splits on space and requires every term to match).
func searchTerms(search string) []string {
	return strings.Fields(strings.ToLower(search))
}

// jobMatchesSearch reports whether the packed job's JSON representation contains
// every search term (upstream matches against JSON.stringify(job).toLowerCase()).
func jobMatchesSearch(packed map[string]any, terms []string) bool {
	b, err := json.Marshal(packed)
	if err != nil {
		return false
	}
	hay := strings.ToLower(string(b))
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// parseStateField normalizes the `state` request field which can be a single
// string or an array of strings (Misskey frontend sends array). Empty input
// defaults to "wait" (Bull wording) = driver の pending バケット。
func parseStateField(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return []string{"wait"}
	}
	// try as array first
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil && single != "" {
		return []string{single}
	}
	return []string{"wait"}
}

// listTasksForState maps a Bull state name to the matching inspector list call.
// completed / failed は mkq の finished-job 保持から引く。"paused" は queue 状態
// であってジョブ一覧ではないため空で返す (admin tab は空表示)。
func (h *Handler) listTasksForState(queue, state string, page, limit int) ([]*QueueTaskSummary, error) {
	switch state {
	case "active":
		return h.queueInspector.ListActiveTasks(queue, page, limit)
	case "scheduled":
		return h.queueInspector.ListScheduledTasks(queue, page, limit)
	case "retry":
		return h.queueInspector.ListRetryTasks(queue, page, limit)
	// Bull と driver の用語対応
	case "wait", "pending":
		return h.queueInspector.ListPendingTasks(queue, page, limit)
	case "delayed":
		// delayed は Bull 用語で driver の scheduled + retry に対応する。
		// **2 つを後から混ぜない (#3167)。** 別々に page を取って連結すると
		// 1 ページが最大 2 倍になり、並びも delayed 全体の新しい順にならない。
		return h.queueInspector.ListDelayedTasks(queue, page, limit)
	case "completed":
		// mkq は WithKeepCompleted retention で完了ジョブを保持する。frontend
		// の All / Latest / Completed タブはこれを引く (#1396)。
		return h.queueInspector.ListCompletedTasks(queue, page, limit)
	case "failed":
		return h.queueInspector.ListFailedTasks(queue, page, limit)
	case "paused":
		// "paused" は queue 状態であってジョブ一覧ではない。**upstream 2026.9.0 は
		// bullmq v6 化に伴い paramDef.state から 'paused' 自体を外し、Paused タブも
		// 削除した**ので、空返しのまま upstream に揃う (mk-go は #2069 で pause/resume
		// を実装済みだが、それとは別の話)。
		return nil, nil
	default:
		return h.queueInspector.ListPendingTasks(queue, page, limit)
	}
}

// QueuePromoteJobs handles POST /api/admin/queue/promote-jobs.
func (h *Handler) QueuePromoteJobs(c echo.Context) error {
	// driver に bulk promote API が無いため、対象 queue の scheduled/retry を
	// 1 ページずつ拾って RunTask で逐次 promote する。大量投入時は後続の
	// ページを クライアント側で再呼び出しする運用。
	// upstream Misskey TS は paramDef で queue を required にしている (#929)。
	var req struct {
		Queue string `json:"queue"`
	}
	if err := c.Bind(&req); err != nil || req.Queue == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue is required."))
	}
	if h.queueInspector == nil {
		return c.NoContent(http.StatusNoContent)
	}
	promoted := 0
	for _, state := range []string{"scheduled", "retry"} {
		var rows []*QueueTaskSummary
		if state == "scheduled" {
			rows, _ = h.queueInspector.ListScheduledTasks(req.Queue, 1, 100)
		} else {
			rows, _ = h.queueInspector.ListRetryTasks(req.Queue, 1, 100)
		}
		for _, t := range rows {
			// **利用者の予定は促進しない。** 促進は「詰まった配送を流す」操作で、
			// 予約投稿を前倒しで公開する操作ではない。
			if isOperatorProtectedTask(t) {
				continue
			}
			if err := h.queueInspector.RunTask(req.Queue, t.ID); err == nil {
				promoted++
			}
		}
	}
	h.logModeration(c, moderationlog.LogPromoteQueue, map[string]any{
		"queue":    req.Queue,
		"promoted": promoted,
	})
	return c.JSON(http.StatusOK, map[string]any{"promoted": promoted})
}

// upstreamQueueTypes is upstream QueueService.QUEUE_TYPES (admin/queue/
// pause・resume の param enum、#17436)。
//
// **drop-in で純正 frontend が来る場合のために残す。** 純正はこの 10 種の
// タブを出すので、mk-go に無い名前でも 400 ではなく no-op 204 で返したい
// (下の queueIsManaged)。
var upstreamQueueTypes = []string{
	"system", "endedPollNotification", "postScheduledNote", "deliver",
	"inbox", "db", "relationship", "objectStorage",
	"userWebhookDeliver", "systemWebhookDeliver",
}

// queuePauseResumeTypes is the accepted `queue` param for pause / resume:
// mk-go が実際に持つ queue と upstream の enum の和集合。
//
// **mk-go 側は queue.AllQueueNames() から引く (#2690)。** 以前は upstream の
// 10 種を手書きしていただけだったので、重なるのは deliver / inbox /
// objectStorage / relationship の 4 つだけ。fork frontend は mk-go の 8 種の
// タブを出すため、**push / export / webhook / maintenance では一時停止・再開が
// 400 で失敗していた**。
var queuePauseResumeTypes = func() map[string]struct{} {
	m := make(map[string]struct{}, len(upstreamQueueTypes)+len(queue.AllQueueNames()))
	for _, q := range upstreamQueueTypes {
		m[q] = struct{}{}
	}
	for _, q := range queue.AllQueueNames() {
		m[q] = struct{}{}
	}
	return m
}()

// QueuePause handles POST /api/admin/queue/pause (upstream #17436)。
func (h *Handler) QueuePause(c echo.Context) error { return h.queuePauseResume(c, true) }

// QueueResume handles POST /api/admin/queue/resume (upstream #17436)。
func (h *Handler) QueueResume(c echo.Context) error { return h.queuePauseResume(c, false) }

// queuePauseResume は pause / resume 共通処理。queue param を QUEUE_TYPES で検証し、
// queueInspector の PauseQueue/UnpauseQueue を呼んで moderationLog に記録する。
// upstream pause.ts / resume.ts と同じく res は無し (204)。
func (h *Handler) queuePauseResume(c echo.Context, pause bool) error {
	var req struct {
		Queue string `json:"queue"`
	}
	if err := c.Bind(&req); err != nil || req.Queue == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue is required."))
	}
	// プラグイン専用のキュー (#2818) は名前が動的なので、静的な一覧では
	// 判定できない。**接頭辞で通したうえで queueIsManaged に実在を確かめ
	// させる** — 未運用の名前はその先で no-op 204 になる。
	if _, ok := queuePauseResumeTypes[req.Queue]; !ok && !strings.HasPrefix(req.Queue, queue.PluginQueuePrefix) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("invalid queue."))
	}
	if h.queueInspector == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// mk-go が運用しない queue (QUEUE_TYPES enum だが worker 無し) は no-op 204。
	// upstream は全 queue が実在するが mk-go は subset のみ運用するため、frontend が
	// 全 QUEUE_TYPES のタブを出して未運用 queue を pause しようとしても、GetQueueInfo /
	// queue-stats の zero-fill 同様 graceful に成功させる (500 を返さない)。
	if !h.queueIsManaged(req.Queue) {
		return c.NoContent(http.StatusNoContent)
	}
	var err error
	logType := moderationlog.LogResumeQueue
	if pause {
		err = h.queueInspector.PauseQueue(req.Queue)
		logType = moderationlog.LogPauseQueue
	} else {
		err = h.queueInspector.UnpauseQueue(req.Queue)
	}
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream は moderationLog.log(me, 'pauseQueue') を detail 無し (空 object) で
	// 記録するため、info も空にして parity を保つ。
	h.logModeration(c, logType, map[string]any{})
	return c.NoContent(http.StatusNoContent)
}

// queueIsManaged reports whether qname is a queue mk-go actually runs (= present
// in Inspector.Queues())。判定不能 (Queues error) 時は通常経路へ倒す。
func (h *Handler) queueIsManaged(qname string) bool {
	queues, err := h.queueInspector.Queues()
	if err != nil {
		return true
	}
	for _, q := range queues {
		if q == qname {
			return true
		}
	}
	return false
}

// shapeQueueForFrontend adapts a QueueInfoResult to the Misskey Bull-shaped
// JSON expected by the admin/job-queue.vue page.
//
// counts / db は mkq driver から取得した実値で埋まる (db は job-queue Redis の
// INFO 由来。queueDBStats 参照)。metrics.{completed,failed}.data は mkq の
// per-queue time-series で、job-metrics retention が有効な queue でのみ履歴を
// 持つ (無効なら data 空 + cumulative count にフォールバック)。
//
// 残る非互換: frontend は Misskey の queue 名 (system / endedPollNotification /
// postScheduledNote 等) を列挙しうるが、mk-go が実行しない queue は GetQueueInfo
// が空を返すため 0 表示になる (#1393 で別途整理予定)。
func shapeQueueForFrontend(info *QueueInfoResult, completed, failed *QueueMetricsResult, db map[string]any, runtime *QueueRuntime) map[string]any {
	// delayed は Misskey 用語で scheduled + retry の合計に相当する
	// (どちらも「すぐには実行されない」状態)。
	delayed := info.Scheduled + info.Retry

	// metrics は driver から渡された QueueMetricsResult を採用し、
	// nil/欠損時は info.Completed / info.Failed の累積値で count を埋め、
	// data は空配列にフォールバックする。mkq driver で WithJobMetrics を
	// 無効にした構成や、time-series を持たない driver を想定。
	completedData, completedCount := metricsToFrontend(completed, int64(info.Completed))
	failedData, failedCount := metricsToFrontend(failed, int64(info.Failed))

	out := map[string]any{
		"name": info.Queue,
		// BullMQ の Queue.qualifiedName は `<prefix>:<name>` (queue-keys.js の
		// getQueueQualifiedName)。frontend は job-queue.vue の caption に
		// そのまま出すので、名前だけ返すと upstream と表示が変わる (#2689)。
		// prefix は driver 設定なので driver に組ませる。
		"qualifiedName": qualifiedQueueName(info),
		"isPaused":      info.IsPaused,
		"counts": map[string]any{
			"active":    info.Active,
			"delayed":   delayed,
			"waiting":   info.Pending,
			"completed": info.Completed,
			"failed":    info.Failed,
		},
		// metrics は upstream packedQueueMetricsSchema 互換: {meta, data, count}。
		// meta は required object {count, prevTS, prevCount}。mk-go は前回 sample の
		// prevTS/prevCount を保持しないため 0 埋め (data 末尾が現在値)。
		"metrics": map[string]any{
			"completed": queueMetricObject(completedData, completedCount),
			"failed":    queueMetricObject(failedData, failedCount),
		},
		// db は job-queue Redis の INFO 由来 (memory / clients / uptime 等)。
		// caller が queueDBStats() で一度引いて全 queue に渡す (INFO は接続単位
		// で全 queue 同値)。provider 未配線時は defaultQueueDB() の 0 埋め。
		"db": db,
	}
	// runtime は mk-go 独自の additive block (#2277)。upstream Misskey は
	// worker 数を静的 config でしか持たないので該当情報が無い。純正 frontend は
	// 未知 field として無視する。
	if runtime != nil {
		out["runtime"] = runtime
	}
	return out
}

// qualifiedQueueName returns the driver-reported BullMQ qualified name,
// falling back to the bare queue name for drivers that do not report one.
func qualifiedQueueName(info *QueueInfoResult) string {
	if info.QualifiedName != "" {
		return info.QualifiedName
	}
	return info.Queue
}

// queueMetricObject builds the upstream QueueMetrics shape ({meta, data, count}).
// meta.count mirrors the headline count; prevTS / prevCount are 0 because mkq
// does not retain the previous sample window.
func queueMetricObject(data []int64, count int64) map[string]any {
	return map[string]any{
		"meta":  map[string]any{"count": count, "prevTS": 0, "prevCount": 0},
		"data":  data,
		"count": count,
	}
}

// metricsToFrontend normalises a driver-supplied QueueMetricsResult
// into the (data, count) pair the BullMQ-shaped frontend expects.
// nil m or empty m.Data both map to an empty []int64 (frontend's
// XChart treats nil and []) for stable JSON.
//
// fallbackCount kicks in when the driver returned no Count (e.g.
// metrics writer disabled) — admin UI then shows the cumulative
// Completed/Failed from GetQueueInfo as the headline number even
// though the chart line stays flat.
func metricsToFrontend(m *QueueMetricsResult, fallbackCount int64) ([]int64, int64) {
	if m == nil {
		return []int64{}, fallbackCount
	}
	data := m.Data
	if data == nil {
		data = []int64{}
	}
	count := m.Count
	if count == 0 {
		count = fallbackCount
	}
	return data, count
}

// QueueQueueStats handles POST /api/admin/queue/queue-stats.
// frontend admin/job-queue.vue の `fetchCurrentQueue` は単一 queue 名を
// 受けて 1 queue の shape を返す設計になっているので、req.Queue が来たら
// その queue だけ、来なければ全 queue を返すという両対応にする。
//
// frontend は Misskey Bull の queue 名を hardcode (`Misskey.queueTypes`) で
// 列挙しており、mk-go の queue 名 (deliver/push/maintenance/webhook/
// export) と完全には一致しない。存在しない queue を叩かれたときは 500 で
// はなくゼロ埋めの shape を返して、フロント側の queueInfo が stale に
// ならないようにする。
func (h *Handler) QueueQueueStats(c echo.Context) error {
	// upstream Misskey TS は paramDef で queue を required にしている (#929)。
	var req struct {
		Queue string `json:"queue"`
	}
	if err := c.Bind(&req); err != nil || req.Queue == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue is required."))
	}
	if h.queueInspector == nil {
		return c.JSON(http.StatusOK, map[string]any{})
	}
	db := h.queueDBStats(c.Request().Context())
	info, err := h.queueInspector.GetQueueInfo(req.Queue)
	if err != nil || info == nil {
		completed, failed := h.fetchQueueMetrics(req.Queue)
		return c.JSON(http.StatusOK, shapeQueueForFrontend(&QueueInfoResult{Queue: req.Queue}, completed, failed, db, h.queueRuntimeFor(req.Queue)))
	}
	completed, failed := h.fetchQueueMetrics(req.Queue)
	return c.JSON(http.StatusOK, shapeQueueForFrontend(info, completed, failed, db, h.queueRuntimeFor(info.Queue)))
}

// fetchQueueMetrics queries the inspector for completed / failed
// per-minute history. Errors are absorbed and returned as nil so the
// admin endpoints stay 200 OK — partial chart data is preferable to
// a hard failure when the metrics writer is opt-in (mkq driver) or
// absent altogether.
func (h *Handler) fetchQueueMetrics(qname string) (completed, failed *QueueMetricsResult) {
	if h.queueInspector == nil {
		return nil, nil
	}
	completed, _ = h.queueInspector.QueueMetrics(qname, queue.MetricsKindCompleted)
	failed, _ = h.queueInspector.QueueMetrics(qname, queue.MetricsKindFailed)
	return
}

// QueueQueues handles POST /api/admin/queue/queues.
func (h *Handler) QueueQueues(c echo.Context) error {
	if h.queueInspector == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	queues, err := h.queueInspector.Queues()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	// db (Redis INFO) は全 queue で同値なので一度だけ引いて使い回す。
	db := h.queueDBStats(c.Request().Context())
	result := make([]map[string]any, 0, len(queues))
	for _, q := range queues {
		info, err := h.queueInspector.GetQueueInfo(q)
		if err != nil {
			continue
		}
		completed, failed := h.fetchQueueMetrics(q)
		result = append(result, shapeQueueForFrontend(info, completed, failed, db, h.queueRuntimeFor(q)))
	}
	return c.JSON(http.StatusOK, result)
}

// QueueRemoveJob handles POST /api/admin/queue/remove-job.
func (h *Handler) QueueRemoveJob(c echo.Context) error {
	queue, id, ok := bindQueueJobReq(c)
	if !ok {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue and jobId are required."))
	}
	if h.queueInspector == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// driver の DeleteTask は不明 task id でも nil error を返しうる
	// (= idempotent) が、upstream Misskey TS は 4xx を返す。drop-in 互換の
	// ため事前に GetTaskInfo で存在確認して、無ければ 404 を返す (#929)。
	if _, err := h.queueInspector.GetTaskInfo(queue, id); err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	if err := h.queueInspector.DeleteTask(queue, id); err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	return c.NoContent(http.StatusNoContent)
}

// bindQueueJobReq binds the shared {queue, jobId} param of remove-job /
// retry-job / show-job / show-job-logs. upstream の required は (queue, jobId)。
// 旧 mk-go の `id` キーも後方互換で受ける (jobId 優先)。queue と job id が
// 揃わなければ ok=false。
func bindQueueJobReq(c echo.Context) (queue, id string, ok bool) {
	var req struct {
		Queue string `json:"queue"`
		JobID string `json:"jobId"`
		ID    string `json:"id"`
	}
	if err := c.Bind(&req); err != nil {
		return "", "", false
	}
	id = req.JobID
	if id == "" {
		id = req.ID
	}
	if req.Queue == "" || id == "" {
		return "", "", false
	}
	return req.Queue, id, true
}

// QueueRetryJob handles POST /api/admin/queue/retry-job.
func (h *Handler) QueueRetryJob(c echo.Context) error {
	queue, id, ok := bindQueueJobReq(c)
	if !ok {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue and jobId are required."))
	}
	if h.queueInspector == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// RunTask も不明 id で nil を返しうる idempotent 挙動。drop-in 互換の
	// ため事前に GetTaskInfo で存在確認 (#929)。
	if _, err := h.queueInspector.GetTaskInfo(queue, id); err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	if err := h.queueInspector.RunTask(queue, id); err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	return c.NoContent(http.StatusNoContent)
}

// QueueShowJob handles POST /api/admin/queue/show-job.
func (h *Handler) QueueShowJob(c echo.Context) error {
	queue, id, ok := bindQueueJobReq(c)
	if !ok {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue and jobId are required."))
	}
	if h.queueInspector == nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	t, err := h.queueInspector.GetTaskInfo(queue, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	return c.JSON(http.StatusOK, packTaskSummary(t))
}

// QueueShowJobLogs handles POST /api/admin/queue/show-job-logs.
//
// upstream は `queue.getJobLogs(jobId).logs` をそのまま返す。mk-go 自身は
// 現状 log を書かないが、**drop-in で TS が書いた job** や、将来 processor が
// Job.Log を使った場合にそのまま読める (#2689)。持たない driver は空を
// 返す。存在しない job と log 0 件の job は BullMQ 同様に区別しない。
func (h *Handler) QueueShowJobLogs(c echo.Context) error {
	queue, id, ok := bindQueueJobReq(c)
	if !ok {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("queue and jobId are required."))
	}
	if h.queueInspector == nil {
		return c.JSON(http.StatusOK, []string{})
	}
	logs, _, err := h.queueInspector.GetTaskLogs(queue, id, 0, -1)
	if err != nil {
		// **エラーでも 200 で空を返す。** upstream は log の有無で job 詳細を
		// 落とさない。ここで 500 にすると log タブを開いただけで画面が壊れる。
		// ただし**握り潰さない** — 未知 queue (fork のタブには mk-go が回して
		// いない queue も並ぶ) や Redis 障害が、無言の空リストになるため。
		slog.Warn("admin: failed to read job logs", "queue", queue, "jobId", id, "err", err)
		return c.JSON(http.StatusOK, []string{})
	}
	if logs == nil {
		return c.JSON(http.StatusOK, []string{})
	}
	return c.JSON(http.StatusOK, logs)
}

// packTaskSummary renders a QueueTaskSummary as the JSON that upstream's
// QueueService.packJobData produces, so admin/job-queue.vue and
// job-queue.job.vue behave the same against mk-go.
//
// **保存されている値をそのまま出す (#2689)。** 以前はここで opts / stacktrace /
// returnValue / progress / delay を組み立て直していたが、それをやると
// mk-go が知らない key が黙って消える。実際、BullMQ が持つ
// `{"backoff":…,"removeOnComplete":…,"removeOnFail":…,"attempts":8}` が
// `{attempts:0,delay:0,repeat:null}` に化けて Options タブが空になり、
// `opts.attempts` が 0 なので Attempts 行も出なくなっていた。
//
// **json schema ではなく frontend の条件に合わせる。** upstream の
// packedQueueJobSchema は failedReason / returnValue / progress を
// `optional: false` と書いているが、**upstream の実装自身がそれを満たして
// いない** (Bull の job は失敗するまで failedReason を持たないので undefined に
// なる)。schema に寄せて空文字を常に出すと、`v-if="job.failedReason != null"` が
// 空文字でも真になり、**成功した job にも赤い警告アイコン付きの空行**が出る。
// 出さないほうが upstream と同じ描画になる。
func packTaskSummary(t *QueueTaskSummary) map[string]any {
	if t == nil {
		return nil
	}
	// upstream は stacktrace を「空要素を除いて逆順」にする (新しい試行が先)。
	stacktrace := make([]string, 0, len(t.Stacktrace))
	for i := len(t.Stacktrace) - 1; i >= 0; i-- {
		if t.Stacktrace[i] != "" {
			stacktrace = append(stacktrace, t.Stacktrace[i])
		}
	}
	// upstream: isFailed = !!failedReason || stacktrace.length > 0。
	isFailed := t.LastErr != "" || len(stacktrace) > 0

	pack := map[string]any{
		"id":   t.ID,
		"name": t.Type,
		// timestamp = job 作成時刻 (Bull job.timestamp)。常に present。
		"timestamp":  formatUnixMillisOrZero(t.EnqueuedAt),
		"attempts":   t.Retried,
		"isFailed":   isFailed,
		"delay":      t.Delay,
		"stacktrace": stacktrace,
		"data":       packJobData(t),
		"opts":       rawJSONOrObject(t.Opts),
		// progress は Bull だと既定 0 (数値)。frontend は数値のときだけ
		// パーセント表示する。
		"progress": rawJSONOr(t.Progress, float64(0)),
		// attemptsAt は upstream に無い mk-go 独自 field (#2692)。job 詳細の
		// Timeline が再試行を実時刻で並べるのに使う。BullMQ は per-attempt の
		// 時刻を残さないので upstream の frontend は試行を `at ?` と出している。
		// **記録が無い job では空配列**になる (mkq v1.0.8 より前に失敗したもの、
		// TS が書いたもの)。frontend はその場合に従来どおり折りたたむ。
		"attemptsAt": attemptsAtOrEmpty(t.AttemptsAt),
		// mk-go 独自 field (既存 admin tool 互換のために残す)。
		"queue":   t.Queue,
		"type":    t.Type,
		"state":   t.State,
		"payload": redactPayloadSecrets(t.Payload),
		"retried": t.Retried,
		// maxRetry は mk-go 独自 field。mkq driver は TaskSummary.MaxRetry を
		// 埋めないので、opts.attempts から拾えるならそちらを使う (0 のまま出すと
		// 「リトライしない設定」に見える、#2689 review)。
		"maxRetry":     maxRetryFor(t),
		"attemptsMade": t.Retried,
	}
	// **失敗していない job には出さない。** 空文字を出すと frontend の
	// `!= null` 判定を通ってしまう (上の doc 参照)。
	if t.LastErr != "" {
		pack["failedReason"] = t.LastErr
		pack["lastErr"] = t.LastErr
	}
	// returnValue も同じ。値が無いときに {} を出すと "Return value" タブが
	// 常に表示される。upstream は Bull の null をそのまま渡してタブを消す。
	if v := rawJSONOr(t.ReturnValue, nil); v != nil {
		pack["returnValue"] = v
	}
	// processedOn / finishedOn は値があるときだけ number で出す。未処理 /
	// 未完了は key 省略 → frontend の `!= null` で行が非表示になる (#1398)。
	if !t.ProcessedAt.IsZero() {
		ms := t.ProcessedAt.UnixMilli()
		pack["processedOn"] = ms
		pack["processedAt"] = ms
	}
	if t.ProcessedBy != "" {
		pack["processedBy"] = t.ProcessedBy
	}
	// 完了時刻は成功 (CompletedAt) / 失敗 (LastFailedAt) いずれかの finish 時刻。
	if finished := t.CompletedAt; !finished.IsZero() {
		pack["finishedOn"] = finished.UnixMilli()
	} else if !t.LastFailedAt.IsZero() {
		pack["finishedOn"] = t.LastFailedAt.UnixMilli()
	}
	if !t.LastFailedAt.IsZero() {
		pack["lastFailedAt"] = t.LastFailedAt.UTC().Format(time.RFC3339Nano)
	}
	if !t.NextProcessAt.IsZero() {
		pack["nextProcessAt"] = t.NextProcessAt.UTC().Format(time.RFC3339Nano)
	}
	if !t.CompletedAt.IsZero() {
		pack["completedAt"] = t.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	return pack
}

// packJobData renders the job payload for the admin Data tab.
//
// **upstream と 1 点だけ意図的に違う。** upstream は Bull の `job.data` を
// そのまま返すが、mk-go は payload を `{"type": …, "body": <base64>}` で包んで
// 保存しているので、そのまま返すと Data タブが base64 の塊になって読めない。
// 包みの形は保ったまま body だけ decode して返す (docs/divergence.md)。
//
// 以前は body だけを返していたため **type が落ちていた** (#2689)。
func packJobData(t *QueueTaskSummary) map[string]any {
	body := rawJSONOrString(t.Payload)
	if body == nil {
		body = map[string]any{}
	}
	body = redactJobSecrets(body)
	// Type が空になることは無い。mkq driver は framing が無ければ BullMQ の
	// job.name に落とし、その既定は queue 名。
	return map[string]any{"type": t.Type, "body": body}
}

// attemptsAtOrEmpty normalises the attempt-start list for JSON.
//
// **nil を出さない。** null にすると配列を期待する frontend が
// `job.attemptsAt.length` で落ちる。
func attemptsAtOrEmpty(v []int64) []int64 {
	if v == nil {
		return []int64{}
	}
	return v
}

// maxRetryFor reports the job's configured attempt limit, preferring the
// BullMQ opts value over the driver-reported one.
//
// mkq driver は TaskSummary.MaxRetry を埋めない (attempts は opts 側にある)。
// どちらが埋まっていても意味のある値になるようにする。
func maxRetryFor(t *QueueTaskSummary) int {
	if t.MaxRetry > 0 {
		return t.MaxRetry
	}
	opts, ok := rawJSONOr(t.Opts, nil).(map[string]any)
	if !ok {
		return t.MaxRetry
	}
	if n, ok := opts["attempts"].(float64); ok {
		return int(n)
	}
	return t.MaxRetry
}

// rawJSONOrObject decodes raw as JSON, falling back to an empty object.
// golden QueueJob.opts は object 必須なので null にはしない。
func rawJSONOrObject(raw json.RawMessage) any {
	if v := rawJSONOr(raw, nil); v != nil {
		return v
	}
	return map[string]any{}
}

// rawJSONOr decodes raw as JSON, returning fallback when raw is empty or
// undecodable.
func rawJSONOr(raw json.RawMessage, fallback any) any {
	if len(raw) == 0 {
		return fallback
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fallback
	}
	if decoded == nil {
		return fallback
	}
	return decoded
}

// formatUnixMillisOrZero returns the unix-ms representation of t, or 0 when
// t is the zero time. Bull's timestamps are unix milliseconds.
func formatUnixMillisOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// rawJSONOrString attempts to decode payload as JSON so the admin UI can
// render structured data. Falls back to a string representation for payloads
// that are not valid JSON.
func rawJSONOrString(payload []byte) any {
	if len(payload) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err == nil {
		return decoded
	}
	return string(payload)
}

// QueueStats handles POST /api/admin/queue/stats.
//
// upstream res は {deliver, inbox, db, objectStorage} の 4 キーで、各値は
// QueueCount = {waiting, active, completed, failed, delayed} (全 required)。
// mk-go は db / objectStorage を独立 queue として運用しないため、その 2 つは
// ゼロ埋め QueueCount を返して shape parity を満たす。deliver / inbox は実値。
func (h *Handler) QueueStats(c echo.Context) error {
	zero := func() map[string]any {
		return map[string]any{"waiting": 0, "active": 0, "completed": 0, "failed": 0, "delayed": 0}
	}
	result := map[string]any{
		"deliver":       zero(),
		"inbox":         zero(),
		"db":            zero(),
		"objectStorage": zero(),
	}
	if h.queueInspector == nil {
		return c.JSON(http.StatusOK, result)
	}
	for _, qname := range []string{"deliver", "inbox"} {
		info, err := h.queueInspector.GetQueueInfo(qname)
		if err != nil {
			continue
		}
		result[qname] = map[string]any{
			"waiting":   info.Pending,
			"active":    info.Active,
			"completed": info.Completed,
			"failed":    info.Failed,
			// Bull の delayed は Scheduled (未来実行予定) と Retry
			// (失敗後再試行待ち) の両方を含む (#654)。
			"delayed": info.Scheduled + info.Retry,
		}
	}
	return c.JSON(http.StatusOK, result)
}

// queueRuntimeFor returns the runtime block for qname, or nil when the
// provider is unwired (= plain drop-in build) or does not know the queue.
func (h *Handler) queueRuntimeFor(qname string) *QueueRuntime {
	if h.queueRuntime == nil {
		return nil
	}
	rt, ok := h.queueRuntime.QueueRuntime(qname)
	if !ok {
		return nil
	}
	return &rt
}

// jobSecretKeys are payload fields that must never reach the API response.
//
// **deliver job の署名鍵。** 現在の producer は payload に載せないが、この
// 変更より前に積まれた job は持っている。取得できれば任意のローカルユーザーと
// して署名付き連合リクエストを偽造できるので、**到達点でも伏せる**
// (moderator + `read:admin:queue` で届く)。
var jobSecretKeys = map[string]struct{}{
	// deliver job の署名鍵 (`queue.DeliverPayload`)。
	"keyPem":         {},
	"ed25519PrivPem": {},
	// webhook job の上書き secret (`queue.WebhookPayload`)。`i/webhooks/test` の
	// 送信内容で、**他人のものも同じ endpoint から読める** (queue 名に
	// allowlist が無い)。upstream も job data に secret を入れているので
	// parity ではあるが、読める必要は無い。
	"overrideSecret": {},
	// プラグイン peer の送信本文 (`peerJob.Envelope`、#3037)。**中身を決めるのは
	// プラグイン**で、本体はそれが何かを知らない。実際の同梱プラグインは
	// 利用者のノートや外部サービスの応答を載せうるし、peer は本来「その
	// プラグイン同士」の通信で、モデレーション画面に出す前提のものではない。
	// Redis に再送のあいだ残り、`admin/queue/jobs` から (moderator +
	// `read:admin:queue` で) 読めていた。**upstream に peer は無い**ので
	// mk-go 独自の判断。
	//
	// 伏せても運用は困らない — 同じ job に `host` / `sendId` / 試行回数 /
	// 失敗理由が出るので、送信の追跡はできる。
	"envelope": {},
}

const redactedPlaceholder = "[redacted]"

// redactJobSecrets replaces secret-bearing fields in a decoded job body.
func redactJobSecrets(body any) any {
	m, ok := body.(map[string]any)
	if !ok {
		return body
	}
	for k := range m {
		if _, secret := jobSecretKeys[k]; secret {
			m[k] = redactedPlaceholder
		}
	}
	return m
}

// redactPayloadSecrets returns the raw payload with secret fields removed.
//
// JSON として読めないものはそのまま返す (非 JSON payload の job がありうる)。
// 伏せ損ねるより読めるほうを優先する形だが、この経路は `data` 側の redact と
// 二重になっており、かつ producer は既に鍵を載せていないので実害は無い。
func redactPayloadSecrets(raw []byte) string {
	if len(raw) == 0 {
		return string(raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}
	var found bool
	for k := range m {
		if _, secret := jobSecretKeys[k]; secret {
			m[k] = redactedPlaceholder
			found = true
		}
	}
	if !found {
		return string(raw)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return string(raw)
	}
	return string(out)
}
