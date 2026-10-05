package admin

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

const (
	ipLookupLogDefaultLimit = 30
	ipLookupLogMaxLimit     = 100
)

// SetIPLookupLogRepo wires the audit trail read path (#3106).
func (h *Handler) SetIPLookupLogRepo(r repository.IPLookupLogRepository) { h.ipLookupLogRepo = r }

// IPLookupLog handles POST /api/admin/ip/lookup-log.
//
// **mk-go 独自 endpoint** (#3106、親 #3066)。誰がいつどの IP / 利用者を照会したかを
// 返す。upstream に対応物は無い (照会の機能そのものが無い)。
//
// **この応答自体が機密。** 照会に使った IP がそのまま入るので、照会と同じ 3 段の
// 権限で守り、`Cache-Control: no-store` を付ける。
func (h *Handler) IPLookupLog(c echo.Context) error {
	if h.ipLookupLogRepo == nil || h.userRepo == nil {
		// **空の一覧を返さない。** 「誰も照会していない」という誤った事実になる (#2792)。
		slog.Error("admin/ip/lookup-log: dependencies are not wired")
		return apierr.JSONInternalError(c)
	}
	var req struct {
		Limit  *int `json:"limit"`
		Offset *int `json:"offset"`
	}
	// Bind のエラーを捨てない (理由は `IPAccounts` と同じ)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, ipLookupLogDefaultLimit, ipLookupLogMaxLimit)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	offset := 0
	if req.Offset != nil {
		if *req.Offset < 0 || *req.Offset > ipSearchMaxOffset {
			return apierr.JSONInvalidParam(c)
		}
		offset = *req.Offset
	}

	// +1 件引いて「次がある」を判断する (総件数は数えない)。
	rows, err := h.ipLookupLogRepo.List(limit+1, offset)
	if err != nil {
		slog.Error("admin/ip/lookup-log: list failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	entries, err := h.packIPLookupLog(rows)
	if err != nil {
		slog.Error("admin/ip/lookup-log: user lookup failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	noStoreIPLookup(c)
	return c.JSON(http.StatusOK, ipLookupLogResponse{
		RetentionDays: ipLookupLogRetentionDays,
		Limit:         limit,
		Offset:        offset,
		HasMore:       hasMore,
		Entries:       entries,
	})
}

// ipLookupLogRetentionDays is how long an audit record survives, in days.
// 画面が決め打ちすると、サーバーが変えたときに黙って嘘になる。
var ipLookupLogRetentionDays = max(1, int(iplookuplog.Retention/(24*time.Hour)))

type ipLookupLogResponse struct {
	// RetentionDays は監査記録を残す日数。**これより前の照会は残っていない** ことを
	// 読む側が知るために要る (「照会されていない」と取り違えない)。
	RetentionDays int                 `json:"retentionDays"`
	Limit         int                 `json:"limit"`
	Offset        int                 `json:"offset"`
	HasMore       bool                `json:"hasMore"`
	Entries       []ipLookupLogEntity `json:"entries"`
}

type ipLookupLogEntity struct {
	ID string `json:"id"`
	// User は照会した人。**引けなければ null** — `ip_lookup_log.userId` に FK は
	// 無いので、退会しても記録は残る (監査の目的からして残すのが正しい)。
	User *entity.UserLite `json:"user"`
	// UserID は User が引けないときでも誰かを追えるように常に返す。
	UserID string `json:"userId"`
	Kind   string `json:"kind"`
	// IP は照会に使ったアドレス (IP 起点のときだけ。それ以外は空文字)。
	IP string `json:"ip"`
	// TargetUser は利用者起点の照会で対象にした利用者。引けなければ null。
	TargetUser   *entity.UserLite `json:"targetUser"`
	TargetUserID string           `json:"targetUserId"`
	SinceDays    int              `json:"sinceDays"`
	// ResultCount は返した候補の件数。**結果そのものは記録していない。**
	ResultCount int    `json:"resultCount"`
	CreatedAt   string `json:"createdAt"`
}

// packIPLookupLog resolves both user sides in one lookup and keeps the order.
func (h *Handler) packIPLookupLog(rows []*model.IPLookupLog) ([]ipLookupLogEntity, error) {
	out := make([]ipLookupLogEntity, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows)*2)
	for _, r := range rows {
		ids = append(ids, r.UserID)
		if r.TargetUserID != "" {
			ids = append(ids, r.TargetUserID)
		}
	}
	users, err := h.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	pack := func(id string) *entity.UserLite {
		if id == "" {
			return nil
		}
		u := byID[id]
		if u == nil {
			return nil
		}
		lite := entity.PackUserLite(u)
		return &lite
	}
	for _, r := range rows {
		out = append(out, ipLookupLogEntity{
			ID:           r.ID,
			User:         pack(r.UserID),
			UserID:       r.UserID,
			Kind:         r.Kind,
			IP:           r.IP,
			TargetUser:   pack(r.TargetUserID),
			TargetUserID: r.TargetUserID,
			SinceDays:    r.SinceDays,
			ResultCount:  r.ResultCount,
			CreatedAt:    entity.ISOMillis(r.CreatedAt),
		})
	}
	return out, nil
}
