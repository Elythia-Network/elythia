package admin

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/driveusage"
	"github.com/shiroha-a/mk/internal/repository"
)

// DriveUsageProvider serves instance-wide drive usage snapshots.
// 実装は core/driveusage.Service。interface で受け取るのは handler のテストから
// 集計を差し替えられるようにするため (依存自体は `driveusage` を import している)。
type DriveUsageProvider interface {
	Breakdown(forceRecalc bool) (*driveusage.Result, error)
}

// SetDriveUsageProvider wires the drive usage aggregator (#3053)。
func (h *Handler) SetDriveUsageProvider(p DriveUsageProvider) {
	h.driveUsage = p
}

// driveUsageSourceDatabase marks the numbers as "what the DB knows".
//
// **実ストレージの使用量ではない。** object storage に実際に置かれている量は、
// 削除の失敗や孤児があれば必ずこれとずれる。後から S3 の API を叩く経路を足す
// ときに区別が付くよう、応答に出所を書いておく (#3053)。
const driveUsageSourceDatabase = "database"

// DriveUsage handles POST /api/admin/drive/usage.
//
// **mk-go 独自 endpoint** (#3053)。upstream はインスタンス全体のストレージ
// 使用量を出す口を持たない (per-user の `driveCapacityMb` だけ)。
//
// 集計は都度走らせて短い TTL のスナップショットを使い回す。**未配線なら 500 を
// 返す** — 0 バイトを返すと「使っていない」という誤った事実を管理画面に出す
// ことになるため。
func (h *Handler) DriveUsage(c echo.Context) error {
	if h.driveUsage == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		// ForceRecalc はキャッシュを無視して集計し直す。管理画面の「更新」用。
		ForceRecalc bool `json:"forceRecalc"`
	}
	// body 無しでも既定値で応答する (管理画面が引数なしで叩く)。
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}

	res, err := h.driveUsage.Breakdown(req.ForceRecalc)
	if err != nil {
		// 集計が落ちた理由は応答に出さない (汎用の 500)。運用側が追えるよう
		// ログには残す。
		slog.Error("admin/drive/usage: aggregation failed", "error", err)
		return apierr.JSONInternalError(c)
	}
	if res == nil || res.Breakdown == nil {
		// provider の契約違反。空の内訳を描くと「容量ゼロ」に見えるので 500。
		slog.Error("admin/drive/usage: provider returned no breakdown")
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, packDriveUsage(res))
}

// driveUsageBucket is one aggregate cell on the wire.
type driveUsageBucket struct {
	Count int64 `json:"count"`
	// Size は DB の `size` 列の合計 (バイト)。
	Size int64 `json:"size"`
	// LinkCount は Count のうち実体を持たない行 (`isLink = true`) の数。
	LinkCount int64 `json:"linkCount"`
}

type driveUsageKindRow struct {
	Kind   string `json:"kind"`
	Origin string `json:"origin"`
	driveUsageBucket
}

type driveUsageHostRow struct {
	Host string `json:"host"`
	driveUsageBucket
}

type driveUsageUserRow struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	driveUsageBucket
}

// driveUsageResponse is the endpoint's body.
type driveUsageResponse struct {
	// CalculatedAt は集計を走らせた時刻 (返した時刻ではない)。
	CalculatedAt string `json:"calculatedAt"`
	ElapsedMs    int64  `json:"elapsedMs"`
	// Cached が true なら過去のスナップショットの使い回し。
	Cached          bool             `json:"cached"`
	CacheTTLSeconds int              `json:"cacheTtlSeconds"`
	TopLimit        int              `json:"topLimit"`
	Source          string           `json:"source"`
	Total           driveUsageBucket `json:"total"`
	Local           driveUsageBucket `json:"local"`
	Remote          driveUsageBucket `json:"remote"`
	// ByKind は 種類 x origin を 0 埋めで必ず全件返す。
	ByKind []driveUsageKindRow `json:"byKind"`
	ByHost []driveUsageHostRow `json:"byHost"`
	ByUser []driveUsageUserRow `json:"byUser"`
}

// packDriveUsage converts the snapshot into the wire shape.
func packDriveUsage(res *driveusage.Result) driveUsageResponse {
	b := res.Breakdown
	out := driveUsageResponse{
		CalculatedAt:    res.CalculatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		ElapsedMs:       res.Elapsed.Milliseconds(),
		Cached:          res.Cached,
		CacheTTLSeconds: int(res.TTL.Seconds()),
		TopLimit:        res.TopN,
		Source:          driveUsageSourceDatabase,
		Total:           packDriveUsageBucket(b.Total()),
		Local:           packDriveUsageBucket(b.LocalTotal()),
		Remote:          packDriveUsageBucket(b.RemoteTotal()),
		ByKind:          make([]driveUsageKindRow, 0, len(b.ByKind)),
		ByHost:          make([]driveUsageHostRow, 0, len(b.ByHost)),
		ByUser:          make([]driveUsageUserRow, 0, len(b.ByUser)),
	}
	for _, row := range b.ByKind {
		out.ByKind = append(out.ByKind, driveUsageKindRow{
			Kind: row.Kind, Origin: row.Origin,
			driveUsageBucket: packDriveUsageBucket(row.DriveUsageBucket),
		})
	}
	for _, row := range b.ByHost {
		out.ByHost = append(out.ByHost, driveUsageHostRow{
			Host:             row.Host,
			driveUsageBucket: packDriveUsageBucket(row.DriveUsageBucket),
		})
	}
	for _, row := range b.ByUser {
		out.ByUser = append(out.ByUser, driveUsageUserRow{
			UserID: row.UserID, Username: row.Username,
			driveUsageBucket: packDriveUsageBucket(row.DriveUsageBucket),
		})
	}
	return out
}

func packDriveUsageBucket(b repository.DriveUsageBucket) driveUsageBucket {
	return driveUsageBucket{Count: b.Count, Size: b.Size, LinkCount: b.LinkCount}
}
