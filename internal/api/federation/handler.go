// Package federation provides /api/federation/* endpoints.
package federation

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	coreinstance "github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// ActorResolver is the narrow subset of core/federation.Resolver that
// federation/update-remote-user needs. Kept as an interface so unit tests can
// swap in a fake without pulling in HTTP transports.
type ActorResolver interface {
	ForceResolveActor(uri string) (*model.User, error)
}

// ModeratorChecker reports whether a user has moderator privileges. Used to
// gate moderator-only fields (moderationNote) on the otherwise-public
// federation/instances and federation/show-instance responses. nil disables
// the gate (moderationNote is never exposed).
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// SignatureCapabilityLookup is the read-only subset of
// repository.InstanceSignatureCapabilityRepository needed to decorate instance
// responses with the observed signature scheme (#2393). 未配線なら
// signatureCapability は常に null になる。
type SignatureCapabilityLookup interface {
	FindByHost(host string) (*model.InstanceSignatureCapability, error)
	FindManyByHosts(hosts []string) ([]*model.InstanceSignatureCapability, error)
}

// Handler handles federation-related API endpoints.
type Handler struct {
	svc           *coreinstance.Service
	followingRepo repository.FollowingRepository
	userRepo      repository.UserRepository
	resolver      ActorResolver
	moderator     ModeratorChecker
	// idGen は federation/followers / federation/following の Following packer
	// で createdAt を aidx ID から導出するために使う。nil の場合 createdAt は
	// 空文字列になる (= 互換性は維持しつつ degrade)。
	idGen id.Generator
	// packer は embed する user/followee を本家 packMany(users, me) と同じ形
	// (関係・カウントのゲート・モデレーター向けの項目・ピン留め・移行先・instance・
	// 絵文字) に組む (#1957-a、#3330)。未配線なら素の UserDetailed を返す。
	packer userpack.ListPacker
	// capabilities は instance ごとの署名方式観測 (#2393)。未配線なら
	// signatureCapability は null。
	capabilities SignatureCapabilityLookup
}

// NewHandler creates a new federation Handler.
func NewHandler(svc *coreinstance.Service) *Handler {
	return &Handler{svc: svc}
}

// SetFollowingRepo attaches a FollowingRepository for followers/following lookup.
func (h *Handler) SetFollowingRepo(r repository.FollowingRepository) {
	h.followingRepo = r
}

// SetUserRepo attaches a UserRepository for the users-per-host listing.
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetIDGen wires an id.Generator so federation/followers / federation/following
// can derive Following.createdAt from aidx-encoded row IDs.
func (h *Handler) SetIDGen(g id.Generator) {
	h.idGen = g
}

// SetResolver attaches an ActorResolver for update-remote-user.
func (h *Handler) SetResolver(r ActorResolver) {
	h.resolver = r
}

// SetListPacker wires the packer of the users embedded in federation/users,
// federation/followers and federation/following (#1957-a, #3330).
func (h *Handler) SetListPacker(p userpack.ListPacker) {
	h.packer = p
}

// HasListPacker reports whether the list packer was wired.
//
// 未配線だと federation/users・followers・following の利用者に関係・ピン留め・
// 移行先・instance・絵文字が載らない。起動時検査に使う。
func (h *Handler) HasListPacker() bool { return h.packer != nil }

// SetSignatureCapabilityLookup wires the store that reports which signature
// scheme each remote host uses (#2393).
func (h *Handler) SetSignatureCapabilityLookup(l SignatureCapabilityLookup) {
	h.capabilities = l
}

// SetModeratorChecker attaches a ModeratorChecker so moderationNote is only
// surfaced to moderators on the public instance-listing endpoints.
func (h *Handler) SetModeratorChecker(m ModeratorChecker) {
	h.moderator = m
}

// requesterIsModerator reports whether the (optionally authenticated) caller
// is a moderator. The global Authenticate middleware populates the user when a
// valid token is presented; unauthenticated callers return false.
func (h *Handler) requesterIsModerator(c echo.Context) bool {
	if h.moderator == nil {
		return false
	}
	user := middleware.GetUser(c)
	if user == nil {
		return false
	}
	return h.moderator.IsModerator(user.ID)
}

// InstancesRequest is the request body for federation/instances.
//
// query タグは upstream instances.ts の allowGet:true 経路用 (#2106 N9)。echo の
// DefaultBinder は GET で BindQueryParams を呼ぶが、explicit query タグの無い field は
// 束縛しない。frontend welcome.entrance.classic.vue は misskeyApiGet で sort/limit/blocked
// 等を渡すため、これらが無いと表示順・件数・block 除外が全て効かなくなる。
type InstancesRequest struct {
	Host          string `json:"host" query:"host"`
	Suspended     *bool  `json:"suspended" query:"suspended"`
	NotResponding *bool  `json:"notResponding" query:"notResponding"`
	Blocked       *bool  `json:"blocked" query:"blocked"`
	Silenced      *bool  `json:"silenced" query:"silenced"`
	Federating    *bool  `json:"federating" query:"federating"`
	Subscribing   *bool  `json:"subscribing" query:"subscribing"`
	Publishing    *bool  `json:"publishing" query:"publishing"`
	Sort          string `json:"sort" query:"sort"`
	Limit         *int   `json:"limit" query:"limit"`
	Offset        int    `json:"offset" query:"offset"`
	SinceID       string `json:"sinceId" query:"sinceId"`
	UntilID       string `json:"untilId" query:"untilId"`
	SinceDate     *int64 `json:"sinceDate" query:"sinceDate"`
	UntilDate     *int64 `json:"untilDate" query:"untilDate"`
}

// Instances handles POST /api/federation/instances.
//
// frontend Paginator (cursor mode) は untilId / sinceId を forward する (#493)。
func (h *Handler) Instances(c echo.Context) error {
	var req InstancesRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// blocked / silenced は instance 列ではなく meta.blockedHosts /
	// silencedHosts との突合で判定する。meta は 1 回だけ取得し、フィルタ突合と
	// レスポンスの isBlocked / isSilenced / isMediaSilenced 算出の双方で使う。
	hosts, err := h.svc.FederationHostLists()
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 30, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	filter := model.InstanceListFilter{
		Host:          req.Host,
		Suspended:     req.Suspended,
		NotResponding: req.NotResponding,
		Blocked:       req.Blocked,
		Silenced:      req.Silenced,
		BlockedHosts:  hosts.Blocked,
		SilencedHosts: hosts.Silenced,
		Federating:    req.Federating,
		Subscribing:   req.Subscribing,
		Publishing:    req.Publishing,
		SortBy:        req.Sort,
		Limit:         limit,
		Offset:        req.Offset,
		SinceID:       sinceID,
		UntilID:       untilID,
	}
	rows, err := h.svc.List(filter)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	showModNote := h.requesterIsModerator(c)
	// 1 ページぶんの host をまとめて 1 クエリで引く。行ごとに引くと 1 ページで
	// limit 回のクエリが飛ぶ (N+1)。
	caps := h.lookupCapabilities(rows)
	out := make([]map[string]any, 0, len(rows))
	for _, inst := range rows {
		out = append(out, instanceToMap(inst, hosts, showModNote, caps[inst.Host]))
	}
	return c.JSON(http.StatusOK, out)
}

// ShowInstanceRequest is the request body for federation/show-instance.
type ShowInstanceRequest struct {
	Host string `json:"host"`
}

// ShowInstance handles POST /api/federation/show-instance.
func (h *Handler) ShowInstance(c echo.Context) error {
	var req ShowInstanceRequest
	if err := c.Bind(&req); err != nil || req.Host == "" {
		return apierr.JSONInvalidParam(c)
	}
	inst, err := h.svc.FindByHost(req.Host)
	if err != nil {
		// upstream Misskey TS は該当 instance 行がないとき 204 No Content を
		// 返す (= null 相当)。frontend admin の lookup UI も 204 を「未知 host」
		// として扱うため drop-in 互換でもこの shape に合わせる (#915)。
		// DB 障害等の真の error は 500 として観測性を保つ。
		if errors.Is(err, coreinstance.ErrInstanceNotFound) {
			return c.NoContent(http.StatusNoContent)
		}
		slog.Error("federation/show-instance: FindByHost failed", "host", req.Host, "err", err)
		return apierr.JSONInternalError(c)
	}
	hosts, err := h.svc.FederationHostLists()
	if err != nil {
		slog.Error("federation/show-instance: FederationHostLists failed", "host", req.Host, "err", err)
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, instanceToMap(inst, hosts, h.requesterIsModerator(c), h.lookupCapability(inst.Host)))
}

// instanceToMap shapes an Instance row into the JSON response object expected
// by misskey-js. 不明値や nil ポインタはそのまま JSON null になるよう map に
// 載せる。response field は upstream InstanceEntityService.pack に揃える
// (federating/subscribing/publishing/notRespondingSince は response field では
// なく federation/instances の query param なので含めない、#1991)。
//
// isBlocked / isSilenced / isMediaSilenced は instance 列ではなく
// meta.blockedHosts / silencedHosts / mediaSilencedHosts との suffix-match で
// 判定する (本家 InstanceEntityService と同じ)。突合対象の host 一覧は呼び出し
// 元が meta から 1 度だけ取得して渡す。
func instanceToMap(inst *model.Instance, hosts coreinstance.FederationHostSets, showModerationNote bool, sigCap *model.InstanceSignatureCapability) map[string]any {
	// moderationNote はモデレーター専用フィールド。公開エンドポイントなので
	// 非モデレーターには null を返す (upstream InstanceEntityService の
	// `moderationNote: iAmModerator ? note : null` 互換)。
	var moderationNote any
	if showModerationNote {
		moderationNote = inst.ModerationNote
	}
	// softwareSuspended: meta.deliverSuspendedSoftware に該当する software なら
	// 配送停止扱い。upstream InstanceEntityService は
	// isSuspended = suspensionState!=='none' || softwareSuspended、
	// suspensionState は none かつ softwareSuspended なら 'softwareSuspended' (#1732)。
	softwareSuspended := corefederation.MatchSuspendedSoftware(inst.SoftwareName, inst.SoftwareVersion, hosts.SuspendedSoftware)
	isSuspended := inst.SuspensionState != model.SuspensionStateNone || softwareSuspended
	var suspensionState any = inst.SuspensionState
	if inst.SuspensionState == model.SuspensionStateNone && softwareSuspended {
		suspensionState = "softwareSuspended"
	}
	return map[string]any{
		"id":               inst.ID,
		"firstRetrievedAt": entity.ISOMillis(inst.FirstRetrievedAt),
		"host":             inst.Host,
		"usersCount":       inst.UsersCount,
		"notesCount":       inst.NotesCount,
		"followingCount":   inst.FollowingCount,
		"followersCount":   inst.FollowersCount,
		// federating/subscribing/publishing は upstream では federation/instances の
		// query 用 paramDef であって InstanceEntity の response field ではない。
		// notRespondingSince も upstream InstanceEntityService.pack に無い (response は
		// isNotResponding boolean のみ)。strict parity のため 4 余剰 field を削除した
		// (#1991。frontend は federating 等を request filter としてのみ使い response を
		// 読まない)。
		"latestRequestReceivedAt": entity.ISOMillisPtr(inst.LatestRequestReceivedAt),
		"isNotResponding":         inst.IsNotResponding,
		"isSuspended":             isSuspended,
		"suspensionState":         suspensionState,
		"isBlocked":               coreinstance.HostMatchesAny(hosts.Blocked, inst.Host),
		"isSilenced":              coreinstance.HostMatchesAny(hosts.Silenced, inst.Host),
		"isMediaSilenced":         coreinstance.HostMatchesAny(hosts.MediaSilenced, inst.Host),
		"softwareName":            inst.SoftwareName,
		"softwareVersion":         inst.SoftwareVersion,
		"openRegistrations":       inst.OpenRegistrations,
		"name":                    inst.Name,
		"description":             inst.Description,
		"maintainerName":          inst.MaintainerName,
		"maintainerEmail":         inst.MaintainerEmail,
		"iconUrl":                 inst.IconURL,
		"faviconUrl":              inst.FaviconURL,
		"themeColor":              inst.ThemeColor,
		"infoUpdatedAt":           entity.ISOMillisPtr(inst.InfoUpdatedAt),
		"moderationNote":          moderationNote,
		// mk-go 独自の additive field (#2393)。upstream の InstanceEntityService には
		// 無いので misskey-js の型にも載らない。fork frontend はローカル型で受ける。
		"signatureCapability": signatureCapabilityToMap(sigCap),
	}
}

// lookupCapabilities batch-resolves the signature capability of every host in
// rows. 未配線 / DB error では空 map を返し、レスポンスは signatureCapability:
// null になる (表示用のメタデータなので、取得できないことで一覧全体を失敗させない)。
func (h *Handler) lookupCapabilities(rows []*model.Instance) map[string]*model.InstanceSignatureCapability {
	if h.capabilities == nil || len(rows) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(rows))
	for _, inst := range rows {
		hosts = append(hosts, inst.Host)
	}
	found, err := h.capabilities.FindManyByHosts(hosts)
	if err != nil {
		slog.Warn("federation/instances: signature capability lookup failed", "err", err)
		return nil
	}
	out := make(map[string]*model.InstanceSignatureCapability, len(found))
	for _, row := range found {
		out[row.Host] = row
	}
	return out
}

// lookupCapability resolves a single host's signature capability. 行が無い場合
// (= 未観測) と未配線 / error はどちらも nil を返す。
func (h *Handler) lookupCapability(host string) *model.InstanceSignatureCapability {
	if h.capabilities == nil || host == "" {
		return nil
	}
	row, err := h.capabilities.FindByHost(host)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			slog.Warn("federation/show-instance: signature capability lookup failed", "host", host, "err", err)
		}
		return nil
	}
	return row
}

// signatureCapabilityToMap shapes the observed signature capability into the
// additive `signatureCapability` response block (#2393).
//
// ed25519 / ldSignature は導出値。生の観測時刻も併せて返すのは、「いつの情報か」が
// 連合トラブルの切り分けで効くため (宣言だけ古い / 配送成功が最近ある、等が
// 区別できる)。観測が 1 つも無い host は null を返し、frontend 側はラベルを出さない。
func signatureCapabilityToMap(sigCap *model.InstanceSignatureCapability) any {
	if sigCap == nil {
		return nil
	}
	var inboundAlg any
	if sigCap.InboundAlg != nil {
		inboundAlg = *sigCap.InboundAlg
	}
	return map[string]any{
		"ed25519":           sigCap.SupportsEd25519(),
		"ldSignature":       sigCap.SupportsLDSignature(),
		"inboundAlgorithm":  inboundAlg,
		"ed25519DeclaredAt": entity.ISOMillisPtr(sigCap.Ed25519DeclaredAt),
		"ed25519AcceptedAt": entity.ISOMillisPtr(sigCap.Ed25519AcceptedAt),
		"inboundObservedAt": entity.ISOMillisPtr(sigCap.InboundObservedAt),
		"ldSignatureSeenAt": entity.ISOMillisPtr(sigCap.LDSignatureSeenAt),
	}
}
