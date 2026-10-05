package admin

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/iplog"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/ipnorm"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// SetIPSearchRepo wires the IP → accounts lookup (#3104)。
func (h *Handler) SetIPSearchRepo(r repository.UserIPSearchRepository) {
	h.ipSearchRepo = r
}

// ipSearchRetentionDays is how long `user_ip` rows survive, in days.
//
// **`core/iplog` の値から導く。** 画面は「これより前の接続は残っていない」と
// 説明するので、掃除の実際の基準と違う数字を出すと嘘になる。
var (
	ipSearchRetentionDays = retentionDays(iplog.Retention)
	// ipSearchDefaultSinceDays は既定の窓。**保持期間そのもの** — 既定では
	// 「残っている記録を全部見る」が欲しい答えで、それより狭めると
	// 「一致なし」が「窓の外だっただけ」なのか区別できない。
	ipSearchDefaultSinceDays = clampSinceDays(ipSearchRetentionDays)
)

// retentionDays converts the retention duration to whole days, floored at 1.
//
// **0 にしない。** 0 を返すと `sinceDays: 0` をエコーすることになり、それを
// そのまま送り返した利用者が 400 を受け取る (`< 1` 判定)。
func retentionDays(d time.Duration) int {
	return max(1, int(d/(24*time.Hour)))
}

// clampSinceDays keeps the default window inside the range the endpoint accepts.
//
// **上端も要る。** 下端だけ守ると、保持期間を `ipSearchMaxSinceDays` より長く
// した構成で**サーバーが返した `sinceDays` をそのまま送り返すと 400 になる** —
// 下端で潰したのと同じ形が逆側に残る。
func clampSinceDays(days int) int {
	return min(ipSearchMaxSinceDays, max(1, days))
}

const (
	// ipSearchMaxSinceDays caps the window. 保持期間より広い指定を弾かないのは、
	// 保持期間を延ばした構成でそのまま効くようにするため。
	ipSearchMaxSinceDays = 3650
	// ipSearchDefaultLimit / ipSearchMaxLimit bound one page.
	ipSearchDefaultLimit = 30
	ipSearchMaxLimit     = 100
	// ipSearchMaxOffset bounds paging. **無制限にしない** — 深いページほど
	// PostgreSQL は捨てる行を走査するので、負荷を外から決められることになる。
	ipSearchMaxOffset = 10000
)

// IPAccounts handles POST /api/admin/ip/accounts.
//
// **mk-go 独自 endpoint** (#3104、親 #3066)。指定した IP を使用したローカル
// アカウントを返す。upstream は逆向き (`admin/get-user-ips`、利用者 → IP) しか持たない。
//
// **同じ IP を使ったことは同一人物であることを意味しない。** 家庭・会社・学校・
// 共有 Wi-Fi・携帯回線の CGNAT・VPN で IP は共有される。返すのは調査の候補と
// その根拠で、自動判定や自動処分には使わない (#3066)。
func (h *Handler) IPAccounts(c echo.Context) error {
	if h.ipSearchRepo == nil || h.metaRepo == nil || h.userRepo == nil {
		// **空の結果を返さない。** 「その IP を使ったアカウントは無い」という
		// 誤った事実になる (#2792)。
		slog.Error("admin/ip/accounts: dependencies are not wired")
		return apierr.JSONInternalError(c)
	}
	var req struct {
		IP        string `json:"ip"`
		SinceDays *int   `json:"sinceDays"`
		Limit     *int   `json:"limit"`
		Offset    *int   `json:"offset"`
	}
	// **Bind のエラーを捨てない。** `encoding/json` は `*int` のポインタを
	// **デコードする前に**確保するので、`{"offset":"99"}` のような型違いでも
	// `req.Offset` は非 nil の `*0` になる。捨てると範囲検査を素通りして
	// `offset = 0` が採用され、**利用者の指定と無関係なページを 200 で返す**
	// (#3025 のカーソルと同じ形)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}

	// **正規化して完全一致で引く。** 記録側は正規形しか書かないので、揺れたまま
	// 引くと当たらない。IP として読めない値はここで弾く (NUL も通らない)。
	ip, ok := ipnorm.Normalize(req.IP)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, ipSearchDefaultLimit, ipSearchMaxLimit)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	sinceDays := ipSearchDefaultSinceDays
	if req.SinceDays != nil {
		if *req.SinceDays < 1 || *req.SinceDays > ipSearchMaxSinceDays {
			return apierr.JSONInvalidParam(c)
		}
		sinceDays = *req.SinceDays
	}
	offset := 0
	if req.Offset != nil {
		if *req.Offset < 0 || *req.Offset > ipSearchMaxOffset {
			return apierr.JSONInvalidParam(c)
		}
		offset = *req.Offset
	}

	// **記録が有効かと、記録が 1 件でもあるかを別々に返す。** #3066 §7 は
	// 「現在の IP 記録が無効でも、残っている過去の記録を検索できる場合は、その
	// 記録範囲に限定した結果であることを表示する」と要求しており、これは
	// 「記録が無効」と「結果がある」が**同時に成り立つ**ケース。単一の状態に
	// 潰すと表現できない。
	//
	// **meta を読めなければ 500 にする。** 読めないまま `false` を返すと
	// 「記録は無効です」という**確かめていない事実**を画面に出すことになり、
	// 空の結果を「記録が止まっていたせい」と誤って説明させる (#2792 と同じ形)。
	m, err := h.metaRepo.Fetch()
	if err != nil || m == nil {
		slog.Error("admin/ip/accounts: meta fetch failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	loggingEnabled := m.EnableIPLogging
	// **+1 件引いて「次がある」を判断する。** 総件数を数えると深いページで
	// 走査が増えるうえ、この画面が要るのは「まだ続きがある」だけ。
	// **暦日で引く。** 掃除は `90 * 24h` の実時間なので、DST を持つ TZ では
	// 境界が 1 時間ずれる。窓は利用者が選ぶ概算なので揃えていない。
	since := time.Now().AddDate(0, 0, -sinceDays)
	rows, err := h.ipSearchRepo.ListAccountsByIP(ip, since, limit+1, offset)
	if err != nil {
		slog.Error("admin/ip/accounts: lookup failed", "error", err)
		return apierr.JSONInternalError(c)
	}

	// **引けた行があるなら記録はある。** そのときは probe を投げない —
	// `SELECT EXISTS` はテーブルの走査になり、90 日の掃除が大量に消した直後の
	// (autovacuum が追いつく前の) テーブルでは dead tuple を全部読む。
	hasAnyHistory := len(rows) > 0
	if !hasAnyHistory {
		hasAnyHistory, err = h.ipSearchRepo.HasAnyHistory()
		if err != nil {
			// **DB 障害を「記録が無い」にしない** (#3066 §7 / #2792) —
			// 「関連アカウントなし」と断定する根拠に使われる値なので、
			// 確かめられなかったことを false で表すと調査の結論が反転する。
			slog.Error("admin/ip/accounts: history probe failed", "error", err)
			return apierr.JSONInternalError(c)
		}
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	accounts, err := h.packIPAccounts(rows)
	if err != nil {
		slog.Error("admin/ip/accounts: user lookup failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	// **落とした件数を返す。** これが無いと画面は `accounts` が空のときに
	// 「0 行ヒット」と「N 行ヒットして全部落とした」を区別できず、
	// **記録が残っているのに「接続は記録されていません」と断定する**ことになる。
	droppedCount := len(rows) - len(accounts)

	// **照会そのものを監査に残す** (#3106)。記録するのは「誰が・いつ・何を・どの
	// 期間で引いて・何件返したか」だけで、**結果は残さない** (残すとこの表が
	// 第 2 の「IP とアカウントの対応」になる)。
	if me := middleware.GetUser(c); me != nil && h.ipLookupAudit != nil {
		h.ipLookupAudit.Record(iplookuplog.Entry{
			UserID: me.ID, Kind: model.IPLookupKindIP,
			IP: ip, SinceDays: sinceDays, ResultCount: len(accounts),
		})
	}
	noStoreIPLookup(c)
	return c.JSON(http.StatusOK, ipAccountsResponse{
		IP:             ip,
		LoggingEnabled: loggingEnabled,
		HasAnyHistory:  hasAnyHistory,
		SinceDays:      sinceDays,
		RetentionDays:  ipSearchRetentionDays,
		Limit:          limit,
		Offset:         offset,
		HasMore:        hasMore,
		DroppedCount:   droppedCount,
		Accounts:       accounts,
	})
}

// ipAccountsResponse is the endpoint's body.
type ipAccountsResponse struct {
	// IP is the normalized form actually searched (入力そのままではない)。
	IP string `json:"ip"`
	// LoggingEnabled / HasAnyHistory は「一致なし」を「記録が無い」と取り違えない
	// ための材料。両方 true で Accounts が空なら、本当に一致が無い。
	LoggingEnabled bool `json:"loggingEnabled"`
	HasAnyHistory  bool `json:"hasAnyHistory"`
	SinceDays      int  `json:"sinceDays"`
	// RetentionDays は `user_ip` を保持する日数。**「一致なし」を「もう消した」と
	// 説明できるようにする材料** — これが無いと画面が保持期間を自分で決め打ちする
	// ことになり、サーバーが変えたときに黙って嘘になる。
	RetentionDays int `json:"retentionDays"`
	Limit         int `json:"limit"`
	Offset        int `json:"offset"`
	// HasMore は**利用者を解決する前**の行数で決まる。解決できない観測を落とすので
	// `accounts` は `limit` より短くなり、**1 ページが丸ごと落ちると空になる**
	// (`hasMore` は true のまま)。読む側は `accounts` が空でも `hasMore` が立って
	// いれば `offset += limit` で次を引くこと。
	HasMore bool `json:"hasMore"`
	// DroppedCount はこのページで引けたが**利用者の行を解決できずに落とした**
	// 観測の数。**「一致なし」と「候補が全員消えている」を言い分けるために要る** —
	// `accounts` が空でも `droppedCount > 0` なら、その IP からの接続は記録されて
	// いる (アカウントが完全削除されているだけ)。これが無いと画面は最後のページで
	// 「接続は記録されていません」と、記録が残っているのに断定することになる。
	DroppedCount int               `json:"droppedCount"`
	Accounts     []ipAccountEntity `json:"accounts"`
}

// ipAccountEntity is one candidate account.
type ipAccountEntity struct {
	User        entity.UserLite `json:"user"`
	IsSuspended bool            `json:"isSuspended"`
	IsDeleted   bool            `json:"isDeleted"`
	// LastActiveDate は**アカウント全体の最終活動**。対象 IP の最終観測とは別物で、
	// 取れなければ null (検索時刻やページを開いた時刻で代用しない、#3066 §2)。
	LastActiveDate *string `json:"lastActiveDate"`
	// FirstSeenAt / LastSeenAt / ObservationCount は**対象 IP についての**観測。
	FirstSeenAt string `json:"firstSeenAt"`
	LastSeenAt  string `json:"lastSeenAt"`
	// ObservationCount は保存している記録の件数で、接続回数そのものではない (#3066 §6)。
	ObservationCount int `json:"observationCount"`
}

// packIPAccounts resolves the user rows and keeps the repository's order.
func (h *Handler) packIPAccounts(rows []repository.UserIPAccountRow) ([]ipAccountEntity, error) {
	out := make([]ipAccountEntity, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.UserID)
	}
	users, err := h.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	for _, r := range rows {
		u := byID[r.UserID]
		if u == nil {
			// **利用者の行が引けなければ候補として出さない。** 名前もアバターも
			// 出せない候補は調査の材料にならないうえ、`user_ip.userId` には
			// **FK が無い** (upstream の `MiUserIp` にも relation が無く、
			// migration も制約を作らない) ので、アカウントを完全削除しても行が
			// 残る。実際に消えるのは 90 日の掃除が来たとき。
			continue
		}
		out = append(out, ipAccountEntity{
			User:             entity.PackUserLite(u),
			IsSuspended:      u.IsSuspended,
			IsDeleted:        u.IsDeleted,
			LastActiveDate:   formatIPTimePtr(u.LastActiveDate),
			FirstSeenAt:      entity.ISOMillis(r.FirstSeen),
			LastSeenAt:       entity.ISOMillis(r.LastSeen),
			ObservationCount: r.ObservationCount,
		})
	}
	return out, nil
}

// formatIPTimePtr is the `*string` flavour of entity.ISOMillis.
//
// `entity.ISOMillisPtr` は `any` を返すので `*string` の field には入らない。
// 書式そのものは `entity.ISOMillis` に寄せてある (`.UTC()` を落とすと、末尾 `Z`
// を付けたままローカル時刻を出す)。
func formatIPTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := entity.ISOMillis(*t)
	return &s
}

// SetIPLookupAudit wires the audit recorder used by admin/ip/* (#3106).
func (h *Handler) SetIPLookupAudit(s *iplookuplog.Service) { h.ipLookupAudit = s }

// HasIPLookupAudit reports whether the audit recorder was wired. 起動時検査に使う。
func (h *Handler) HasIPLookupAudit() bool { return h.ipLookupAudit.Wired() }

// noStoreIPLookup marks an IP lookup response as uncacheable.
//
// **共有キャッシュに残さない** (#3106 / #3066 §8)。POST + Authorization なので
// 既定でも共有キャッシュは保存しないが、**明示しないと検証できない** — 中間に
// キャッシュを置く構成や、将来 GET 版を足したときに黙って漏れる。
// `private` ではなく `no-store` にするのは、ブラウザのディスクキャッシュにも
// 残したくないため。
func noStoreIPLookup(c echo.Context) {
	c.Response().Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Response().Header().Set("Pragma", "no-cache")
}
