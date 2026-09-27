package admin

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/moderationlog"
	"github.com/shiroha-a/mk/internal/misc/idnhost"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
)

// toPunyHost normalizes a host the same way upstream UtilityService.toPuny does
// (domainToASCII(host.toLowerCase()), node:url / UTS#46 mapping)。Go の HTTP client
// が dial する名前 (`idna.Lookup`) と同じ形で、全角英字や U+3002 も畳む。変換でき
// ない不正 host は小文字化のみで返し、後段の FindByHost に「見つからない」と判定させる。
//
// 取り込み側 (`resolver.hostFromURI`) も #2706 で同じ正規化を掛けるので、保存形と
// 引き当ては同じ値になる。**backfill 前に非正規化で保存された行は引けない** —
// この lookup は完全一致で、#2996 以降は acct 経路も同じになった。
// `cmd/backfill-remote-host` を流すこと。
//
// **既定ポートの扱いだけは違う。** `hostFromURI` は `https://h:443` を `h` として
// 保存するようになったが (連合ゲートの綴り回避を塞ぐため)、`Puny` はポートを
// 剥がさない。ポートを含む host を引き当てるときは、保存側と同じ形にしてから
// 渡すこと。
//
// **実装は `idnhost.Puny` へ委譲する。** 手写しを 3 つ目に増やすと、保存形が
// `idnhost.Puny` に固定された今、drift したときに保存形と引き当てがずれる
// (#2714 review LOW-10)。
func toPunyHost(host string) string { return idnhost.Puny(host) }

// FederationDeleteAllFiles handles POST /api/admin/federation/delete-all-files.
//
// 指定ホストの DriveFile (= リモート user の上げた添付) を一括削除する。
// DriveBulkDeleter physically deletes all drive files of a remote host
// (storage objects + drive usage decrement + row delete). Implemented by
// *core/drive.Service (DeleteAllByHost). 循環依存回避のため interface で受ける。
type DriveBulkDeleter interface {
	DeleteAllByHost(host string) (int64, error)
}

// SetDriveBulkDeleter wires the physical drive purge used by
// federation/delete-all-files (#1772).
func (h *Handler) SetDriveBulkDeleter(d DriveBulkDeleter) { h.driveBulkDeleter = d }

// FederationDeleteAllFiles handles POST /api/admin/federation/delete-all-files.
//
// upstream delete-all-files.ts は host の各 file に driveService.deleteFile を
// 呼び、物理ストレージ (S3 / internal) 削除 + drive 使用量減算 + row 削除を行う。
// mk-go も driveBulkDeleter (= core/drive.Service.DeleteAllByHost) 経由で同等の
// 物理削除を行う (#1772。以前は row のみ削除で S3 オブジェクトが orphan 化 +
// 使用量カウンタが未減算だった)。driveBulkDeleter 未配線時は row のみ削除に
// degrade。host 空 / repo 未配線は no-op 204。
func (h *Handler) FederationDeleteAllFiles(c echo.Context) error {
	var req struct {
		Host string `json:"host"`
	}
	_ = c.Bind(&req)
	if req.Host == "" {
		return c.NoContent(http.StatusNoContent)
	}
	if h.driveBulkDeleter != nil {
		if _, err := h.driveBulkDeleter.DeleteAllByHost(req.Host); err != nil {
			return apierr.JSONInternalError(c)
		}
		return c.NoContent(http.StatusNoContent)
	}
	if h.driveFileRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	if _, err := h.driveFileRepo.DeleteByHost(req.Host); err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// FederationRefreshRemoteInstanceMetadata handles POST /api/admin/federation/refresh-remote-instance-metadata.
// InstanceMetadataFetcher (= coreinstance.FetchMetadataService) 経由で指定ホストの
// nodeinfo + iconUrl / faviconUrl を再取得する。fetcher 未設定または host 未指定
// の場合は no-op で 204 を返す (本家 TS も失敗時エラーコードは返さない挙動)。
func (h *Handler) FederationRefreshRemoteInstanceMetadata(c echo.Context) error {
	var req struct {
		Host string `json:"host"`
	}
	_ = c.Bind(&req)
	if h.instanceMetadataFetcher == nil || req.Host == "" {
		return c.NoContent(http.StatusNoContent)
	}
	host := toPunyHost(req.Host)
	// upstream は findOneBy({host: toPuny(host)}) == null で
	// `throw new Error('instance not found')` (= 500 INTERNAL_ERROR) する。
	// instanceRepo 配線時は事前に存在確認し、未登録 host へのエラーを伝播する。
	if h.instanceRepo != nil {
		if _, err := h.instanceRepo.FindByHost(host); err != nil {
			return apierr.JSONInternalError(c)
		}
	}
	// fetch 失敗はユーザーへ明示的にエラー返す必要はない (upstream も
	// fetchInstanceMetadata を await せず fire-and-forget する)。ログに残して
	// リトライ可能な状態にしておく。
	if err := h.instanceMetadataFetcher.Fetch(host); err != nil {
		slog.Warn("federation refresh metadata failed", "host", host, "err", err)
	}
	return c.NoContent(http.StatusNoContent)
}

// FederationRemoveAllFollowing handles POST /api/admin/federation/remove-all-following.
//
// 指定ホストの Follower (= 当インスタンスのローカル user を follow している
// リモート user) を全員 unfollow させる。followerHost = req.Host の Following
// 行を列挙し、各 (followerID, followeeID) ペアごとに per-pair Unfollow ジョブを
// キューに enqueue する。実際の row 削除と Reject(Follow) 配送は worker
// (processors.UnfollowProcessor) が core/following.Service.Unfollow を呼んで
// 行うので、HTTP request はブロックされない。
//
// 旧実装は no-op だったが #587 で本実装化。Misskey TS 本家
// (packages/backend/src/server/api/endpoints/admin/federation/remove-all-following.ts
// が queueService.createUnfollowJob 経由で per-pair queueing する) と挙動を
// 揃える。host 未指定 / 依存未配線の場合は no-op で 204 を返す。
//
// Worker 側で処理されるため、enumeration 中に row 削除は発生しない。よって
// offset ベースの pagination で安全に列挙できる (sync 版で必要だった seen-set
// は不要)。
// cleanupSuspendedUserRelations performs the local relationship side-effects of
// suspending a user, mirroring upstream UserSuspendService.postSuspend +
// unFollowAll (#1759): delete the user's pending follow requests (both
// directions) and unfollow everyone the user follows (outgoing follows). All
// best-effort — failures are logged but never block the suspend.
func (h *Handler) cleanupSuspendedUserRelations(userID string) {
	if h.followReqRepo != nil {
		if err := h.followReqRepo.DeleteAllByUser(userID); err != nil {
			slog.Warn("admin suspend: delete follow requests failed", "userId", userID, "err", err)
		}
	}
	if h.followingRepo == nil || h.unfollowEnqueuer == nil {
		return
	}
	const pageSize = 100
	const maxBatches = 1000 // safety cap
	for batch := 0; batch < maxBatches; batch++ {
		// upstream unFollowAll は followerId = user の outgoing follow のみを
		// silent に unfollow する (incoming follower は触らない)。EnqueueUnfollow が
		// 行削除 + Undo(Follow) 連合を worker 側で行う。
		rows, err := h.followingRepo.ListFollowing(userID, pageSize, batch*pageSize)
		if err != nil {
			slog.Warn("admin suspend: list following failed", "userId", userID, "err", err)
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			if err := h.unfollowEnqueuer.EnqueueUnfollow(queue.UnfollowPayload{
				FollowerID: f.FollowerID,
				FolloweeID: f.FolloweeID,
			}); err != nil {
				slog.Warn("admin suspend: enqueue unfollow failed",
					"follower", f.FollowerID, "followee", f.FolloweeID, "err", err)
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
}

func (h *Handler) FederationRemoveAllFollowing(c echo.Context) error {
	var req struct {
		Host string `json:"host"`
	}
	_ = c.Bind(&req)
	if req.Host == "" || h.followingRepo == nil || h.unfollowEnqueuer == nil {
		return c.NoContent(http.StatusNoContent)
	}
	const pageSize = 100
	const maxBatches = 1000 // safety cap: 100k pairs / request
	for batch := 0; batch < maxBatches; batch++ {
		// 本家 admin/federation/remove-all-following.ts は followingsRepository.
		// findBy({ followerHost: host }) で「follower が指定ホストの行」を引く。
		// followerHost で絞るのは ListFollowingByHost なのでこちらを使う
		// (#1544 で federation/{followers,following} の host 列を本家準拠に
		// 訂正した結果、ListFollowersByHost は followeeHost 絞りになった)。
		rows, err := h.followingRepo.ListFollowingByHost(req.Host, pageSize, batch*pageSize)
		if err != nil {
			return apierr.JSONInternalError(c)
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			if err := h.unfollowEnqueuer.EnqueueUnfollow(queue.UnfollowPayload{
				FollowerID: f.FollowerID,
				FolloweeID: f.FolloweeID,
			}); err != nil {
				// enqueue 失敗は個別ペアで握りつぶす - admin 操作の best-effort
				// として残りの enqueue を妨げない。Worker retry が効かないため
				// ログには残しておく。
				slog.Warn("admin: federation/remove-all-following: enqueue failed",
					"host", req.Host,
					"follower", f.FollowerID,
					"followee", f.FolloweeID,
					"err", err)
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// federationUpdateInstanceRequest is the JSON shape for
// /api/admin/federation/update-instance. ModerationNote は **pointer** にして
// 「未送信 (nil)」と「明示的に空文字列で clear」を区別する (#675)。
// json.Unmarshal は欠落 field を nil pointer のまま残すので、両者を JSON
// decode 境界で正しく分離できる。string 型だと "" がデフォルト値と区別でき
// ず空文字列で note を消す操作が無視される (元バグ)。
//
// IsBlocked / IsSilenced は upstream Misskey TS の wire 互換のため受信する
// が、対応 DB 列が無く mk-go schema にも upstream にも存在しないため
// updates() で silently drop される (#715 / #724)。frontend がスイッチを
// 操作しても効果なし、将来 schema 拡張で対応 column が増えたら updates()
// に変換 case を足す。
type federationUpdateInstanceRequest struct {
	Host           string  `json:"host"`
	IsSuspended    *bool   `json:"isSuspended"`
	IsBlocked      *bool   `json:"isBlocked"`
	IsSilenced     *bool   `json:"isSilenced"`
	ModerationNote *string `json:"moderationNote"`
}

// updates derives the GORM Updates(map) payload from the request. Only
// fields explicitly sent in the request are included so the caller can
// "clear" string fields by sending "" (and not "leave unchanged" via
// omitting the field).
//
// upstream Misskey TS の admin/federation/update-instance は wire 上では
// `isSuspended bool` を受けるが、mk-go の `instance` table は
// `suspensionState varchar` enum (`none` / `manuallySuspended` / 等) で
// 管理しており `isSuspended` 列は存在しない (#715 / #724)。boolean を
// enum に変換して GORM Updates 用 map に詰める。
//
// `isBlocked` / `isSilenced` は upstream にも対応 column が無く、mk-go
// schema にも存在しない。silently drop。将来 schema 拡張で対応 column が
// 増えたら本関数で変換を足す。
func (req federationUpdateInstanceRequest) updates() map[string]any {
	out := map[string]any{}
	if req.IsSuspended != nil {
		// admin が「suspend する」を選んだら manuallySuspended、解除は none。
		// auto-suspend (goneSuspended / autoSuspendedForNotResponding) はこの
		// 経路では生成されず、別 path で発生する。
		if *req.IsSuspended {
			out["suspensionState"] = string(model.SuspensionStateManuallySuspended)
		} else {
			out["suspensionState"] = string(model.SuspensionStateNone)
		}
	}
	if req.ModerationNote != nil {
		// pointer != nil なら明示的に送信されたので空文字列でも反映する。
		out["moderationNote"] = *req.ModerationNote
	}
	return out
}

// FederationUpdateInstance handles POST /api/admin/federation/update-instance.
//
// Misskey TS 互換 (admin/federation/update-instance.ts) で `isSuspended` 変更時に
// `suspendRemoteInstance` / `unsuspendRemoteInstance`、`moderationNote` 変更時に
// `updateRemoteInstanceNote` の moderation_log を **個別に** 出力する。1 リクエ
// ストで両方変更されれば 2 ログ。`isBlocked` / `isSilenced` は TS 仕様に該当
// type が無いので skip。
//
// instance 行の lookup / 更新は `InstanceRepository` (#676 で DI 化) を経由する。
// 未配線時は no-op で 204 を返す (元の挙動と一貫)。
func (h *Handler) FederationUpdateInstance(c echo.Context) error {
	if h.instanceRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req federationUpdateInstanceRequest
	if err := c.Bind(&req); err != nil || req.Host == "" {
		return c.NoContent(http.StatusNoContent)
	}
	// upstream は lookup 前に toPuny(host) で punycode / 小文字化する。IDN / 大文字
	// host でも instance を引けるよう正規化してから FindByHost / UpdateFields に渡す。
	host := toPunyHost(req.Host)
	// before snapshot for moderation log diff (`isSuspended` の変化判定 +
	// `moderationNote` の before/after)。
	beforePtr, err := h.instanceRepo.FindByHost(host)
	if err != nil {
		// upstream は instance==null で `throw new Error('instance not found')`
		// (= 500 INTERNAL_ERROR) する。旧実装は silent 204 だったが本家に合わせて
		// エラーを伝播する。
		return apierr.JSONInternalError(c)
	}
	// 値コピーで snapshot を凍結する: UpdateFields の実装によっては同じ struct
	// を mutate することがあり (例: in-memory mock)、後段の moderation log diff
	// で before/after が両方 after 値になる退行を防ぐ。
	//
	// 注意: 浅 copy なので *string 等の pointer field は参照先を共有する。
	// 現状 log で参照する `SuspensionState string` / `ModerationNote string`
	// は scalar なので問題なし。pointer field を log に含める拡張が入った
	// 時は deep copy への昇格を検討。
	before := *beforePtr
	if updates := req.updates(); len(updates) > 0 {
		if err := h.instanceRepo.UpdateFields(host, updates); err != nil {
			// silently 握り潰すと #724 のように DB 列ミスマッチで NO-OP に
			// なって moderation log だけ書き込まれる症状が再発する。warn
			// で残すことで運用者が気付ける。
			slog.Warn("admin: instance UpdateFields failed", "host", host, "err", err)
		} else if req.IsSuspended != nil {
			// suspensionState を更新したら deliver hot path の suspend 判定
			// cache を即時失効し、TTL を待たず配送可否へ反映する (#1407 review)。
			h.invalidateInstanceSuspendCache(host)
		}
	}
	// suspend / unsuspend と moderationNote 変更で個別 log を出す。
	// SuspensionState != "none" を「suspended」と扱う。
	beforeSuspended := before.SuspensionState != model.SuspensionStateNone
	if req.IsSuspended != nil && *req.IsSuspended != beforeSuspended {
		t := moderationlog.LogSuspendRemoteInstance
		if !*req.IsSuspended {
			t = moderationlog.LogUnsuspendRemoteInstance
		}
		h.logModeration(c, t, map[string]any{
			"id":   before.ID,
			"host": before.Host,
		})
	}
	if req.ModerationNote != nil && *req.ModerationNote != before.ModerationNote {
		h.logModeration(c, moderationlog.LogUpdateRemoteInstanceNote, map[string]any{
			"id":     before.ID,
			"host":   before.Host,
			"before": before.ModerationNote,
			"after":  *req.ModerationNote,
		})
	}
	return c.NoContent(http.StatusNoContent)
}
