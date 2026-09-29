package admin

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/core/iplookuplog"
	"github.com/shiroha-a/mk/internal/core/iprelation"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

const (
	// ipRelatedMaxTargetIPs は起点にする対象ユーザーの IP の上限 (#3105)。
	//
	// **最終観測の新しい順に切る。** 時間減衰は古いほど軽いので、打ち切ったときに
	// 残るのが重みの大きい IP になる。
	ipRelatedMaxTargetIPs = 50
	// ipRelatedMaxPerIP は 1 つの IP から取る候補の上限。
	//
	// **CGNAT や公衆 Wi-Fi の IP は 1 つで数千アカウントが載りうる。** 上限が
	// 無いと、利用者 1 人を指定するだけで無制限のクエリを外から回せる。
	// 打ち切ったことは `truncated` で伝える — 黙って切ると「これで全部」と読まれる。
	ipRelatedMaxPerIP = 200
	// ipRelatedDefaultLimit / ipRelatedMaxLimit bound one page of candidates.
	ipRelatedDefaultLimit = 30
	ipRelatedMaxLimit     = 100
)

// IPRelatedAccounts handles POST /api/admin/ip/related-accounts.
//
// **mk-go 独自 endpoint** (#3105、親 #3066)。対象ユーザーが期間内に使った IP を
// 起点に、同じ IP を使った**他の**ローカルアカウントを関連候補として返す。
// upstream にはこの向きの機能が無い。
//
// **同じ IP を使ったことは同一人物であることを意味しない。** 家庭・会社・学校・
// 共有 Wi-Fi・携帯回線の CGNAT・VPN で IP は共有される。返すのは調査の候補と
// **その根拠**で、自動判定や自動処分には使わない (#3066)。
func (h *Handler) IPRelatedAccounts(c echo.Context) error {
	if h.ipSearchRepo == nil || h.metaRepo == nil || h.userRepo == nil {
		// **空の結果を返さない。** 「関連する候補は無い」という誤った事実になる (#2792)。
		slog.Error("admin/ip/related-accounts: dependencies are not wired")
		return apierr.JSONInternalError(c)
	}
	var req struct {
		UserID    string `json:"userId"`
		SinceDays *int   `json:"sinceDays"`
		Limit     *int   `json:"limit"`
		Offset    *int   `json:"offset"`
	}
	// Bind のエラーを捨てない (理由は `IPAccounts` と同じ)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, ipRelatedDefaultLimit, ipRelatedMaxLimit)
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

	// **対象ユーザーを先に解決する。** 画面に出す基本情報が要るうえ、ここで
	// 弾いておけば NUL を含む id が一覧系のクエリへ届かない (#3025)。
	target, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		if repository.IsNotFound(err) {
			return apierr.JSONNoSuchUser(c)
		}
		slog.Error("admin/ip/related-accounts: target lookup failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	if target == nil {
		return apierr.JSONNoSuchUser(c)
	}
	// **リモート利用者は対象にしない。** `user_ip` は自インスタンスで認証を通した
	// 利用者しか書かないので、リモートを指定すると必ず「候補なし」になる。
	// それを「関連が無い」と読ませないために、引く前に断る。
	if target.Host != nil && *target.Host != "" {
		return apierr.JSONInvalidParam(c)
	}
	m, err := h.metaRepo.Fetch()
	if err != nil || m == nil {
		slog.Error("admin/ip/related-accounts: meta fetch failed", "error", err)
		return apierr.JSONInternalError(c)
	}

	// **対象が管理者 / root / system アカウントなら断る。**
	//
	// `admin/show-user` は同じ相手に `ACCESS_DENIED` を返すのに、こちらは
	// 候補一覧と共有 IP (生のアドレス) を返していた。候補ゼロでも
	// `targetIpCount` / `hasAnyHistory` から「その管理者の IP 記録が何本あるか」
	// が分かる。何より候補一覧そのものが「その管理者の別アカウントはどれか」で、
	// `admin/show-user` が拒否している情報より踏み込んでいる。
	//
	// 判定できないときは 500 に倒す (#2792 / #3037 と同じ形)。
	switch denied, undetermined := h.credentialTakeoverDenied(c, target); {
	case undetermined:
		return apierr.JSONInternalError(c)
	case denied:
		return c.JSON(http.StatusBadRequest,
			apierr.Error("ACCESS_DENIED", "Cannot show info of admin.", "0d4e3a3e-2c1f-4d8b-9d2a-7a0c1c1b2f3a"))
	}

	// **now は 1 回の検索で固定する** (#3105)。呼ぶたびに取り直すと、同じ検索の
	// 中で先に計算した候補ほど減衰が浅くなり、順位が計算順に依存する。
	now := h.now()
	since := now.AddDate(0, 0, -sinceDays)

	// 起点は対象ユーザーの IP。+1 件引いて打ち切ったかを判断する。
	targetIPs, err := h.ipSearchRepo.ListIPsByUser(target.ID, since, ipRelatedMaxTargetIPs+1)
	if err != nil {
		slog.Error("admin/ip/related-accounts: target ips failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	// **打ち切りは原因ごとに分ける。** 起点の IP を切ったのか候補を切ったのかで
	// 運用者が取れる手が違う (前者は期間を絞る、後者はどうにもならない)。
	// 1 つのフラグに潰すと、画面は起点を切ったときに「候補が多い」と誤って説明する。
	targetIPsTruncated := len(targetIPs) > ipRelatedMaxTargetIPs
	if targetIPsTruncated {
		targetIPs = targetIPs[:ipRelatedMaxTargetIPs]
	}

	// **行が引けていれば記録はある。** 無いときだけ probe する (走査になるため)。
	hasAnyHistory := len(targetIPs) > 0
	if !hasAnyHistory {
		hasAnyHistory, err = h.ipSearchRepo.HasAnyHistory()
		if err != nil {
			// **DB 障害を「記録が無い」にしない** (#3066 §7 / #2792)。
			slog.Error("admin/ip/related-accounts: history probe failed", "error", err)
			return apierr.JSONInternalError(c)
		}
	}

	observations, sharedTruncated, err := h.collectSharedObservations(targetIPs, since, target.ID)
	if err != nil {
		slog.Error("admin/ip/related-accounts: shared lookup failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	truncated := targetIPsTruncated || sharedTruncated

	ranked := iprelation.Rank(now, iprelation.DefaultHalfLife, observations)
	page := ranked
	if offset >= len(page) {
		page = nil
	} else {
		page = page[offset:]
	}
	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}
	candidates, err := h.packRelatedCandidates(page)
	if err != nil {
		slog.Error("admin/ip/related-accounts: candidate lookup failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	// 落とした件数の意味は `IPAccounts` と同じ (利用者の行を解決できなかった観測)。
	droppedCount := len(page) - len(candidates)

	// 照会そのものを監査に残す (#3106)。結果は残さない (理由は `IPAccounts` と同じ)。
	if me := middleware.GetUser(c); me != nil && h.ipLookupAudit != nil {
		h.ipLookupAudit.Record(iplookuplog.Entry{
			UserID: me.ID, Kind: model.IPLookupKindRelated,
			TargetUserID: target.ID, SinceDays: sinceDays, ResultCount: len(candidates),
		})
	}
	noStoreIPLookup(c)
	return c.JSON(http.StatusOK, ipRelatedResponse{
		User:                entity.PackUserLite(target),
		LoggingEnabled:      m.EnableIPLogging,
		HasAnyHistory:       hasAnyHistory,
		SinceDays:           sinceDays,
		RetentionDays:       ipSearchRetentionDays,
		HalfLifeDays:        int(iprelation.DefaultHalfLife / (24 * time.Hour)),
		TargetIPCount:       len(targetIPs),
		Truncated:           truncated,
		TargetIPsTruncated:  targetIPsTruncated,
		CandidatesTruncated: sharedTruncated,
		Limit:               limit,
		Offset:              offset,
		HasMore:             hasMore,
		DroppedCount:        droppedCount,
		Candidates:          candidates,
	})
}

// collectSharedObservations joins the target's IPs with the accounts on them.
//
// **`perIP+1` 件まで引いて、その IP のアカウント数を「見えた範囲の下限」として
// 数える。** 正確な数を出すには全行読むしかなく、それは CGNAT 規模の IP で
// 走査が有界でなくなる (repository 側のコメント参照)。共有回線かの判断には
// 「200 件以上」で足りる。
func (h *Handler) collectSharedObservations(targetIPs []repository.UserIPWindowRow, since time.Time, targetID string) ([]iprelation.Observation, bool, error) {
	if len(targetIPs) == 0 {
		return nil, false, nil
	}
	ips := make([]string, 0, len(targetIPs))
	targetLast := make(map[string]time.Time, len(targetIPs))
	for _, r := range targetIPs {
		ips = append(ips, r.IP)
		targetLast[r.IP] = r.LastSeen
	}
	fetch := ipRelatedMaxPerIP + 1
	rows, err := h.ipSearchRepo.ListSharedIPAccounts(ips, since, fetch)
	if err != nil {
		return nil, false, err
	}

	// その IP で見えた行数。対象本人の行も含む (アカウント数なので)。
	seen := make(map[string]int, len(ips))
	for _, r := range rows {
		seen[r.IP]++
	}
	truncated := false
	for _, n := range seen {
		if n >= fetch {
			truncated = true
			break
		}
	}

	taken := make(map[string]int, len(ips))
	out := make([]iprelation.Observation, 0, len(rows))
	for _, r := range rows {
		// **対象本人は候補にしない。** SQL では落とせない (落とすと数えられない)。
		if r.UserID == targetID {
			continue
		}
		// **`targetLast` に無い IP は捨てる。** 起点に無い IP が返ることは現在の
		// SQL では起きないが、入ると対象側の最終観測がゼロ値になり、
		// `0001-01-01` を「観測日時」として画面に出すことになる。
		//
		// 枠 (`taken`) より前に置いてあるが、**順序を入れ替えても結果は変わらない**
		// (枠は IP ごとなので、起点に無い IP は自分の枠しか消費しない)。
		// 読む順として自然なほうに寄せただけで、テストでは区別できない。
		tl, ok := targetLast[r.IP]
		if !ok {
			continue
		}
		if taken[r.IP] >= ipRelatedMaxPerIP {
			continue
		}
		taken[r.IP]++
		out = append(out, iprelation.Observation{
			UserID: r.UserID, IP: r.IP,
			TargetLastSeen: tl, CandidateLastSeen: r.LastSeen,
			IPAccountCount: seen[r.IP],
			// 上限に達していれば、その数は下限でしかない。
			IPAccountCountIsLowerBound: seen[r.IP] >= fetch,
		})
	}
	return out, truncated, nil
}

// ipRelatedResponse is the endpoint's body.
type ipRelatedResponse struct {
	// User は検索の対象。画面が別途引き直さずに済むように返す。
	User entity.UserLite `json:"user"`
	// LoggingEnabled / HasAnyHistory は「候補なし」を「記録が無い」と取り違えない
	// ための材料 (#3066 §7)。
	LoggingEnabled bool `json:"loggingEnabled"`
	HasAnyHistory  bool `json:"hasAnyHistory"`
	SinceDays      int  `json:"sinceDays"`
	RetentionDays  int  `json:"retentionDays"`
	// HalfLifeDays は時間減衰の半減期。**順位の根拠を読む側に見せるため**に返す
	// (画面で決め打ちすると、サーバーが変えたときに黙って嘘になる)。
	HalfLifeDays int `json:"halfLifeDays"`
	// TargetIPCount は起点にした対象ユーザーの IP の数 (打ち切り後)。
	TargetIPCount int `json:"targetIpCount"`
	// Truncated は**どちらかの上限で打ち切ったか**。立っているときは調べたのが
	// 全体の一部なので、**「一致が無い」とは言えない**。黙って切ると「これで全部」
	// と読まれる。
	Truncated bool `json:"truncated"`
	// TargetIPsTruncated / CandidatesTruncated は**どちらを切ったか**。
	//
	// **原因で取れる手が違う。** 起点を切ったなら期間を絞れば絞り込めるが、
	// 候補側の打ち切りはどうにもならない。1 つに潰すと、画面は起点を切ったときに
	// 「候補が多い」と誤って説明する。
	//
	// **両方立つことがある。** 片方だけを出す形にすると、そのとき残りの説明が
	// 丸ごと落ちる (IPv6 の起点 50 本超と CGNAT の IP は同時に起きやすい)。
	TargetIPsTruncated  bool `json:"targetIpsTruncated"`
	CandidatesTruncated bool `json:"candidatesTruncated"`
	Limit               int  `json:"limit"`
	Offset              int  `json:"offset"`
	HasMore             bool `json:"hasMore"`
	// DroppedCount はこのページで**利用者の行を解決できずに落とした**候補の数
	// (`user_ip.userId` に FK が無いので、完全削除しても行は残る)。
	DroppedCount int                  `json:"droppedCount"`
	Candidates   []ipRelatedCandidate `json:"candidates"`
}

// ipRelatedCandidate is one account that shared at least one IP with the target.
type ipRelatedCandidate struct {
	User        entity.UserLite `json:"user"`
	IsSuspended bool            `json:"isSuspended"`
	IsDeleted   bool            `json:"isDeleted"`
	// LastActiveDate は**アカウント全体の最終活動**。共有 IP の観測とは別物で、
	// 取れなければ null (検索時刻で代用しない、#3066 §2)。
	LastActiveDate *string `json:"lastActiveDate"`
	// SharedIPCount は共有した IP の数 (重複なし)。
	SharedIPCount int `json:"sharedIpCount"`
	// Score は**順位を決めるためだけの値で、確率ではない。**
	//
	// 重みの単純和なので上限は 1 ではなく、共有 IP の数だけ足し上がる
	// (今日使った IP を 3 つ共有していれば 3.0)。**パーセントとして出さないこと。**
	Score float64 `json:"score"`
	// SharedIPs は順位の根拠。重みの大きい順。
	SharedIPs []ipRelatedSharedIP `json:"sharedIps"`
}

// ipRelatedSharedIP is one shared IP and both sides' last observation of it.
type ipRelatedSharedIP struct {
	IP string `json:"ip"`
	// TargetLastSeenAt / CandidateLastSeenAt は同じ IP に対する双方の最終観測。
	//
	// **これを「同時に使っていた」と書かないこと** (#3105)。一致していなければ
	// ただ両方の時刻が並ぶだけで、重なりを意味しない。
	TargetLastSeenAt    string `json:"targetLastSeenAt"`
	CandidateLastSeenAt string `json:"candidateLastSeenAt"`
	// IPAccountCount は窓の中でその IP を使ったアカウント数 (対象本人を含む)。
	// **共有回線かを読む側が判断するための材料** — 数が大きいほど一致は弱い。
	//
	// **正確な数とは限らない。** 数えるにはその IP の全行を読む必要があり、それは
	// CGNAT 規模の IP で走査が有界でなくなる。上限まで見えたときは
	// `ipAccountCountIsLowerBound` が立ち、その数は「これ以上」を意味する。
	IPAccountCount             int  `json:"ipAccountCount"`
	IPAccountCountIsLowerBound bool `json:"ipAccountCountIsLowerBound"`
	// ElapsedDays は減衰に使った経過日数 = `max(0, now - min(双方の最終観測))`。
	//
	// **重みそのものは返さない。** あれは [0,1] に収まるので、`score` と違って
	// パーセントと見分けが付かない。経過日数は事実で、`halfLifeDays` と
	// 合わせれば読む側が重みを再現できる。
	ElapsedDays float64 `json:"elapsedDays"`
}

// packRelatedCandidates resolves the user rows and keeps the ranking order.
func (h *Handler) packRelatedCandidates(ranked []iprelation.Candidate) ([]ipRelatedCandidate, error) {
	out := make([]ipRelatedCandidate, 0, len(ranked))
	if len(ranked) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(ranked))
	for _, c := range ranked {
		ids = append(ids, c.UserID)
	}
	users, err := h.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	for _, c := range ranked {
		u := byID[c.UserID]
		if u == nil {
			// **利用者の行が引けなければ候補として出さない。** `user_ip.userId` には
			// FK が無いので、アカウントを完全削除しても行は 90 日の掃除まで残る。
			continue
		}
		shared := make([]ipRelatedSharedIP, 0, len(c.SharedIPs))
		for _, p := range c.SharedIPs {
			shared = append(shared, ipRelatedSharedIP{
				IP:                         p.IP,
				TargetLastSeenAt:           entity.ISOMillis(p.TargetLastSeen),
				CandidateLastSeenAt:        entity.ISOMillis(p.CandidateLastSeen),
				IPAccountCount:             p.IPAccountCount,
				IPAccountCountIsLowerBound: p.IPAccountCountIsLowerBound,
				ElapsedDays:                p.ElapsedDays,
			})
		}
		out = append(out, ipRelatedCandidate{
			User:           entity.PackUserLite(u),
			IsSuspended:    u.IsSuspended,
			IsDeleted:      u.IsDeleted,
			LastActiveDate: formatIPTimePtr(u.LastActiveDate),
			SharedIPCount:  len(c.SharedIPs),
			Score:          c.Score,
			SharedIPs:      shared,
		})
	}
	return out, nil
}
