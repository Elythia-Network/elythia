package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	corereversi "github.com/shiroha-a/mk/internal/core/reversi"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"gorm.io/datatypes"
)

// FederationDeliverer delivers AP activities to remote users.
// 循環依存を避けるため interface で定義。実装は core/federation.DeliverService。
type FederationDeliverer interface {
	DeliverToUser(signerUserID string, recipient *model.User, body []byte) error
}

// ReversiStreamPublisher publishes `invited` events to a local user's
// reversi stream topic (#417 P2)。実装は internal/stream.ReversiGamePublisher。
type ReversiStreamPublisher interface {
	PublishInvited(targetUserID string, inviter *model.User)
}

// Handler handles reversi/* endpoints.
type Handler struct {
	repo         repository.ReversiRepository
	svc          *corereversi.Service
	idGen        id.Generator
	baseURL      string
	deliverer    FederationDeliverer
	fedCache     *corereversi.FederationIDCache
	userRepo     repository.UserRepository
	streamPub    ReversiStreamPublisher
	fedChecker   FederationAvailabilityChecker
	remoteLookup RemoteUserLookup
	// lites は reversi/invitations の招待者 (UserLite) の instance と絵文字を
	// まとめて埋める (本家 packMany(invitations, me)、#3330)。連合対戦では
	// リモートの招待者が混ざる。
	lites userpack.LiteFiller
}

// SetLiteFiller wires the batch resolver of instance / emojis for the
// inviters of reversi/invitations (#3330).
func (h *Handler) SetLiteFiller(f userpack.LiteFiller) {
	h.lites = f
}

// HasLiteFiller reports whether the lite filler was wired.
//
// 未配線だと reversi/invitations のリモートの招待者に instance と絵文字が載らない。
// 起動時検査に使う。
func (h *Handler) HasLiteFiller() bool { return h.lites != nil }

// FederationAvailabilityChecker determines whether a remote host advertises
// a compatible reversiVersion via its nodeinfo.
// 実装は core/reversi.FederationChecker。
type FederationAvailabilityChecker interface {
	Available(ctx context.Context, host string) bool
}

// RemoteUserLookup resolves an uncached remote user identified by acct
// (`@user@host`) via WebFinger + ActivityPub Person fetch.
// 実装は core/federation.RemoteUserResolver。
type RemoteUserLookup interface {
	ResolveByUsernameHost(username, host string) (*model.User, error)
}

// NewHandler creates a new reversi handler.
func NewHandler(repo repository.ReversiRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// SetService attaches the core reversi service. 必須ではないが設定されていれば
// Surrender 等の player action が service 経由で動く (IsStarted バリデーション +
// WebSocket 通知が効くようになる)。nil の場合は従来の repo 直接操作にフォール
// バックするので既存テスト互換。
func (h *Handler) SetService(svc *corereversi.Service) {
	h.svc = svc
}

// SetFederationChecker attaches a FederationAvailabilityChecker used to
// gate outbound Invite delivery when the remote host does not advertise a
// compatible reversiVersion (#417 P3)。未設定なら常に送信を許可する
// (従来互換)。
func (h *Handler) SetFederationChecker(c FederationAvailabilityChecker) {
	h.fedChecker = c
}

// SetRemoteUserLookup attaches a webfinger-capable remote user resolver so
// that /match can accept `@user@host` acct for users not yet cached locally
// (#417 P3 Enhancement)。
func (h *Handler) SetRemoteUserLookup(r RemoteUserLookup) {
	h.remoteLookup = r
}

// SetStreamPublisher attaches a per-user reversi stream publisher used for
// local-side `invited` events (#417 P2)。
//
// NOTE: local invite 経路でこのパブリッシャを使うには userRepo が必要で、
// userRepo は SetFederation で設定される。プロダクションは router.go で
// 両方同時に配線するので問題無いが、片方だけ呼んだ場合 publish は skip
// される (Match 側で h.userRepo != nil guard がある)。
func (h *Handler) SetStreamPublisher(p ReversiStreamPublisher) {
	h.streamPub = p
}

// SetFederation attaches federation support.
func (h *Handler) SetFederation(baseURL string, deliverer FederationDeliverer, fedCache *corereversi.FederationIDCache, userRepo repository.UserRepository) {
	h.baseURL = baseURL
	h.deliverer = deliverer
	h.fedCache = fedCache
	h.userRepo = userRepo
}

func packGame(g *model.ReversiGame, idGen id.Generator) map[string]any {
	result := map[string]any{
		"id":         g.ID,
		"user1Id":    g.User1ID,
		"user2Id":    g.User2ID,
		"user1Ready": g.User1Ready,
		"user2Ready": g.User2Ready,
		"black":      g.Black,
		"isStarted":  g.IsStarted,
		"isEnded":    g.IsEnded,
		// form1 / form2 は upstream packDetail が `optional:false, nullable:true`
		// で常に出力する (ReversiGameEntityService.packDetail / json-schema
		// reversi-game.ts form1/form2)。frontend のゲーム設定 UI が form を
		// 参照するため欠落させない。空 (nil) は JSON で null になる (#1553)。
		"form1":                jsonOrNull(g.Form1),
		"form2":                jsonOrNull(g.Form2),
		"winnerId":             g.WinnerID,
		"surrenderedUserId":    g.SurrenderedUserID,
		"timeoutUserId":        g.TimeoutUserID,
		"timeLimitForEachTurn": g.TimeLimitForEachTurn,
		"noIrregularRules":     g.NoIrregularRules,
		"isLlotheo":            g.IsLlotheo,
		"canPutEverywhere":     g.CanPutEverywhere,
		"loopedBoard":          g.LoopedBoard,
		"map":                  g.Map,
		"bw":                   g.BW,
		// startedAt / endedAt は upstream ReversiGameEntityService が toISOString()
		// で常にミリ秒 3 桁固定の ISO8601 を返す。*time.Time を生で map に入れると
		// encoding/json が RFC3339Nano (小数 0 秒は .000 無し、pg microsecond は 6 桁)
		// で出力し createdAt 等の .000Z 正規化と不一致になるため明示フォーマットする
		// (nil は null、#1774)。
		"startedAt": isoOrNull(g.StartedAt),
		"endedAt":   isoOrNull(g.EndedAt),
		"logs":      g.Logs,
		"crc32":     g.CRC32,
	}
	if t, err := idGen.ParseTime(g.ID); err == nil {
		result["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if g.User1 != nil {
		result["user1"] = entity.PackUserLite(g.User1)
	}
	if g.User2 != nil {
		result["user2"] = entity.PackUserLite(g.User2)
	}
	// winner は upstream Misskey TS の ReversiGameEntityService.packDetail と
	// 同じく winnerId から UserLite を派生させる (#649)。frontend は
	// `v-if="game.winner"` で勝敗表示を切り替えるため、winnerId だけでは
	// draw に倒れる。json-schema は optional:false, nullable:true なので
	// winnerId が無い (対局中 / draw) ときもキー自体は null で常に出す (#1553)。
	result["winner"] = nil
	if g.WinnerID != nil {
		if g.User1 != nil && g.User1.ID == *g.WinnerID {
			result["winner"] = entity.PackUserLite(g.User1)
		} else if g.User2 != nil && g.User2.ID == *g.WinnerID {
			result["winner"] = entity.PackUserLite(g.User2)
		}
	}
	return result
}

// jsonOrNull normalizes a datatypes.JSON column into a value safe for
// json.Marshal: a nil or empty byte slice becomes a nil json.RawMessage so it
// serializes to `null`, otherwise the raw JSON bytes are emitted verbatim.
// 非 nil だが長さ 0 の datatypes.JSON をそのまま map に入れると
// json.Marshal がレスポンス全体を失敗させる (encoding/json は空 JSON を
// 不正とみなす) ため、ここで null に正規化しておく。
func jsonOrNull(j datatypes.JSON) any {
	if len(j) == 0 {
		return nil
	}
	return json.RawMessage(j)
}

// isoOrNull renders an optional timestamp as the millisecond-fixed ISO8601
// string used across mk-go packers (toISOString-compatible), or null when the
// pointer is nil. Used for ReversiGame startedAt / endedAt (#1774)。
func isoOrNull(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// 標準8x8盤面
var defaultMap = model.StringArray{
	"--------",
	"--------",
	"--------",
	"---wb---",
	"---bw---",
	"--------",
	"--------",
	"--------",
}

// resolveAcct converts an acct form (`@user` or `@user@host`) into a local
// user id。最初に UserRepository を引き、remote でキャッシュ未取込ならば
// RemoteUserLookup (WebFinger + AP Person fetch) にフォールバックする
// (#417 P3 Enhancement: リモートユーザーを招待できるようにする)。
func (h *Handler) resolveAcct(acct string) (string, error) {
	trimmed := strings.TrimPrefix(acct, "@")
	if trimmed == "" {
		return "", errors.New("empty acct")
	}
	username := trimmed
	var host string
	if at := strings.IndexByte(trimmed, '@'); at >= 0 {
		username = trimmed[:at]
		host = strings.ToLower(trimmed[at+1:])
	}
	// DB 側は username_lower 比較、WebFinger は RFC 7033 case-insensitive、
	// AP resolver も canonical username を再取得するので username を一度だけ
	// 小文字化して両経路で同じ値を使う (#417 P3 Devin review)。
	username = strings.ToLower(username)
	var hostPtr *string
	if host != "" {
		hostPtr = &host
	}
	if u, err := h.userRepo.FindByUsernameLower(username, hostPtr); err == nil {
		return u.ID, nil
	}
	// ローカル DB に無い remote user → WebFinger で取り込む。
	if host != "" && h.remoteLookup != nil {
		if u, err := h.remoteLookup.ResolveByUsernameHost(username, host); err == nil && u != nil {
			return u.ID, nil
		}
	}
	return "", errors.New("user not found")
}

// Games handles POST /api/reversi/games — list games.
// CherryPick 本家互換で sinceId / untilId / my の keyset pagination を受ける。
// my=true (要認証) は自分が User1 or User2 のゲーム、それ以外は isStarted=true
// のゲームを返す。pagination を無視すると frontend の無限スクロールが同じ
// ページを繰り返しロードするので必須 (#417 P1 UDS 検証で発覚)。
func (h *Handler) Games(c echo.Context) error {
	var req struct {
		Limit     int    `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		My        bool   `json:"my"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Limit > 100 {
		req.Limit = 100
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	viewer := middleware.GetUser(c)
	var games []*model.ReversiGame
	if req.My && viewer != nil {
		games, _ = h.repo.ListByUserCursor(viewer.ID, sinceID, untilID, req.Limit)
	} else {
		games, _ = h.repo.ListStartedCursor(sinceID, untilID, req.Limit)
	}
	// #2106 L15 (cherrypick-lineage divergence): vanilla の reversi/games は ReversiGameLite
	// (form1/form2/logs/map を含まない) を返すが、mk-go は cherrypick 系統 + 連合対戦拡張
	// (crc32 等) を持ち全 endpoint で Detailed packGame を共有する。追加 field は加算的で
	// misskey-js は未知 field を無視するため drop-in client は壊れない。vanilla golden では
	// なく cherrypick + 連合拡張を権威とする (MEMORY: reversi_cherrypick_lineage)。
	out := make([]map[string]any, len(games))
	for i, g := range games {
		out[i] = packGame(g, h.idGen)
	}
	return c.JSON(http.StatusOK, out)
}

// Invitations handles POST /api/reversi/invitations — list pending invitations.
// レスポンス shape は CherryPick 本家と同じ UserLite[] (招待者一覧)。Game[] を
// 返すと Misskey フロントエンドの matching UI が期待値と食い違って無限 loading
// になる (#417 P1 deploy で発覚)。viewer が User2 (招待される側) となっている
// 未開始 / 未終了 game の User1 を UserLite として返す。
func (h *Handler) Invitations(c echo.Context) error {
	user := middleware.GetUser(c)
	games, _ := h.repo.ListByUser(user.ID, 20)
	inviters := make([]entity.UserLite, 0)
	users := make([]*model.User, 0)
	seen := make(map[string]struct{})
	for _, g := range games {
		if g.IsStarted || g.IsEnded {
			continue
		}
		// 招待を受けている側 (User2) のみ対象。自分が招待側 (User1) のゲームは
		// invitations UI の文脈的に表示しない。
		if g.User2ID != user.ID {
			continue
		}
		if g.User1 == nil {
			continue
		}
		if _, dup := seen[g.User1.ID]; dup {
			continue
		}
		seen[g.User1.ID] = struct{}{}
		inviters = append(inviters, entity.PackUserLite(g.User1))
		users = append(users, g.User1)
	}
	if h.lites != nil {
		ptrs := make([]*entity.UserLite, len(users))
		for i := range users {
			ptrs[i] = &inviters[i]
		}
		h.lites.FillLites(users, ptrs)
	}
	return c.JSON(http.StatusOK, inviters)
}

// ShowGame handles POST /api/reversi/show-game.
func (h *Handler) ShowGame(c echo.Context) error {
	var req struct {
		GameID string `json:"gameId"`
	}
	if err := c.Bind(&req); err != nil || req.GameID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "gameId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	game, err := h.repo.FindByID(req.GameID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		// show-game の noSuchGame は upstream で endpoint 固有 UUID
		// (show-game.ts:19)。surrender / verify とは別値なので使い回さない。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_GAME", "No such game.", "f13a03db-fae1-46c9-87f3-43c8165419e1"))
	}
	// ユーザーが invitations UI からではなく「自分の対局」一覧などから
	// pending game に直接入った場合、/match が呼ばれないままなので相手側が
	// ゲームを作成できない (Join が飛ばない)。viewer が User2 (招待された
	// 側) で game が pre-start、かつ User1 がリモートなら Join を自動配信
	// する (#417 P1 UDS 検証で発覚)。deliver は idempotent。
	viewer := middleware.GetUser(c)
	if viewer != nil && !game.IsStarted && !game.IsEnded && game.User2ID == viewer.ID {
		h.sendJoinForAcceptedInvite(c, viewer, game)
	}
	return c.JSON(http.StatusOK, packGame(game, h.idGen))
}

// Match handles POST /api/reversi/match — create or join a game.
// userId は本家互換で local user id を受け付けるが、CherryPick 拡張として
// `@user` / `@user@host` 形式 (acct) も受け入れる。これは vanilla Misskey
// フロントエンドが「対戦相手選択画面」を持たず、リモートユーザーを選択で
// きない制約を緩和するための backend-side workaround。
func (h *Handler) Match(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		UserID string `json:"userId"`
		// upstream match.ts paramDef の noIrregularRules / multiple (既定 false)。
		// noIrregularRules は specific-match では upstream も matched() で false 固定
		// (ReversiService.ts:125) で、any-match (mk-go は 204 stub) でしか効かないため
		// ここでは accept するが live path では inert。multiple は下の outbound dedup を
		// スキップするのに使う (#1774)。
		NoIrregularRules bool `json:"noIrregularRules"`
		Multiple         bool `json:"multiple"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	_ = req.NoIrregularRules // accepted for paramDef parity; inert on specific-match

	// acct 形式 (@user / @user@host) を local user id に解決する
	if strings.HasPrefix(req.UserID, "@") && h.userRepo != nil {
		resolved, err := h.resolveAcct(req.UserID)
		if err != nil {
			// match の noSuchUser は upstream match.ts:22 の endpoint 固有 UUID。
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER", "No such user.", "0b4f0559-b484-4e31-9581-3f73cee89b28"))
		}
		req.UserID = resolved
	}

	// 自分自身を相手指定したら upstream match.ts:57 と同じ TARGET_IS_YOURSELF。
	// acct (`@自分`) が self に解決されるケースも弾くため resolve の後で判定する。
	if req.UserID != "" && req.UserID == user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("TARGET_IS_YOURSELF", "Target user is yourself.", "96fd7bd6-d2bc-426c-a865-d055dcd2828e"))
	}

	// ランダムマッチ (userId 無し) は相手が確定するまで成立しない。upstream は
	// meta.res が optional:true で、matchAnyUser がマッチ未成立時に null を返す
	// と endpoint も空ボディ (204) になる (match.ts:33-37,68)。従来の
	// 「user1=user2 の仮置き game 行を作って 200 で返す」実装は frontend を
	// 即ゲーム画面 (自分 vs 自分) に遷移させてしまう上、仮置き行が invitations
	// に自分自身として出るため、行を作らず本家と同じ空レスポンスを返す (#1553)。
	// any-match の実ペアリングは待機列 (Redis ZSET) で行う (#2407)。
	if req.UserID == "" {
		return h.matchAny(c, user, req.NoIrregularRules, req.Multiple)
	}

	// 既に相手から招待を受けている pending game があれば、それを accept 扱いで
	// 再利用する (#417 P1)。CherryPick 本家の matchSpecificUser と同じ動作:
	// inbound Invite 経由で作成された reversi_game 行 (User1=相手, User2=自分)
	// を流用し、相手に Join を返すだけにする。これをやらないと /match が毎回
	// 新しいゲーム + 新しい session_id を作って二重招待になり、state 伝播が
	// 噛み合わなくなる。
	if existing := h.findPendingInvitationFrom(user.ID, req.UserID); existing != nil {
		h.sendJoinForAcceptedInvite(c, user, existing)
		return c.JSON(http.StatusOK, packGame(existing, h.idGen))
	}

	// upstream matchSpecificUser: multiple=false なら直近 3 分以内の未開始 pending
	// game を再利用して二重招待を防ぐ (ReversiService.ts:99-112)。mk-go では招待が
	// 実 game 行なので、自分発 (User1=me, User2=target) の未開始 game が 3 分以内に
	// あればそれを返す (再 Invite はしない = upstream も返すだけ)。相手発の再利用は
	// 上の inbound-accept (mk-go 独自の Join 送信) が既に担うのでここでは扱わない。
	// multiple=true は upstream 同様このスキップで毎回新規作成する (#1774)。
	if !req.Multiple {
		if existing := h.findPendingInvitationFrom(req.UserID, user.ID); existing != nil && h.withinMatchDedupWindow(existing) {
			// まだ相手が accept していない自分発の招待。upstream は招待を
			// Redis にしか置かず、対局成立まで null (= 204) を返す
			// (match.ts:68 `if (game == null) return;`)。200 で game を返すと
			// フロントが即ゲーム画面に遷移してしまう (#1553 と同じ罠)。
			return c.NoContent(http.StatusNoContent)
		}
	}

	// target user を一度だけ引いて pre-check と deliver 両方で使い回す
	// (#417 P3 Devin review: 元実装では pre-check と delivery で 2 回
	// FindByID していた)。
	var targetUser *model.User
	if h.userRepo != nil {
		if u, err := h.userRepo.FindByID(req.UserID); err == nil {
			targetUser = u
		}
	}

	// 新規招待で remote target の場合は reversiVersion 互換性を先に確認する
	// (#417 P3)。非対応ホストへの招待は silent に握りつぶされるだけで UI が
	// 「待機中」のまま動かないので、ゲーム行作成前に 400 エラーで弾く。
	if targetUser != nil && h.fedChecker != nil && targetUser.Host != nil && *targetUser.Host != "" {
		if !h.fedChecker.Available(c.Request().Context(), *targetUser.Host) {
			return c.JSON(http.StatusBadRequest, apierr.Error(
				"NO_REVERSI_FEDERATION",
				"The target user's server does not support reversi federation.",
				"3c9d76c8-8d40-4f6e-9bea-e4e57ae02fed"))
		}
	}

	now := time.Now()
	game := &model.ReversiGame{
		ID:                   h.idGen.Generate(now),
		User1ID:              user.ID,
		User2ID:              req.UserID,
		Map:                  defaultMap,
		BW:                   "random",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}

	if err := h.repo.Create(game); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// ターゲット種別によって挙動分岐:
	//   - remote: federation session を Redis に保存 + Invite 送信
	//            (Host != nil かつ URI != nil の両方を要求するのは remote
	//             branch で AP Invite の targetURI として URI が必要なため)
	//   - local : 招待された側の reversi stream に `invited` イベントを push
	//            (CherryPick 互換。フロントが polling 無しで招待を検知できる)
	//   - host はあるが URI が nil の degenerate state は理論上起こらない
	//     (resolver が両方セットする) のでここで local 扱いに落ちても
	//     Redis publish は subscriber 無しで no-op となり実害なし。
	if targetUser != nil {
		if targetUser.Host != nil && targetUser.URI != nil {
			// remote: AP Invite を配信 (reversiVersion 互換性は
			// ゲーム行作成前の pre-check 済み)。
			if h.deliverer != nil && h.fedCache != nil {
				sessionID := h.idGen.Generate(now) + "-fed"
				h.fedCache.Set(c.Request().Context(), sessionID, game.ID)
				invite := corereversi.RenderInvite(h.baseURL, sessionID,
					h.baseURL+"/users/"+user.ID, *targetUser.URI, now.UTC().Format(time.RFC3339))
				if body, jerr := json.Marshal(invite); jerr == nil {
					_ = h.deliverer.DeliverToUser(user.ID, targetUser, body)
				}
			}
		} else if h.streamPub != nil {
			// local: stream に invited イベントを push (#417 P2)
			h.streamPub.PublishInvited(targetUser.ID, user)
		}
	}

	// 新規招待は対局未成立。upstream matchSpecificUser が null を返す経路なので
	// 204 + 空ボディで揃える。mk-go は招待を reversi_game 行として持つ設計だが、
	// それは連合 (AP Invite の target) の都合であって API 契約ではない。
	return c.NoContent(http.StatusNoContent)
}

// findPendingInvitationFrom scans viewer's recent reversi_game rows and
// returns the pending game where User1=inviter, User2=viewer, not started / ended.
// inbound Invite によって作られた受信待ち招待を「ゲーム成立」として消費する
// ために使う。corereversi.FindPendingInvitation に共有実装。
func (h *Handler) findPendingInvitationFrom(viewerID, inviterID string) *model.ReversiGame {
	return corereversi.FindPendingInvitation(h.repo, viewerID, inviterID)
}

// withinMatchDedupWindow reports whether a pending game was created recently
// enough to be reused by the multiple=false dedup. Upstream gates the dedup on
// a 3-minute window (ReversiService.ts:103) so stale invites fall through to a
// fresh game/invite (#1774)。idGen.ParseTime 不能なら安全側で false。
func (h *Handler) withinMatchDedupWindow(g *model.ReversiGame) bool {
	t, err := h.idGen.ParseTime(g.ID)
	if err != nil {
		return false
	}
	return time.Since(t) < 3*time.Minute
}

// sendJoinForAcceptedInvite delivers a Join activity to the remote inviter so
// both sides agree the match has started. local-only の招待の場合 (User1 が
// ローカル) は federation 不要なので何もしない。
func (h *Handler) sendJoinForAcceptedInvite(c echo.Context, accepter *model.User, game *model.ReversiGame) {
	if h.userRepo == nil || h.deliverer == nil || h.fedCache == nil {
		return
	}
	inviter, err := h.userRepo.FindByID(game.User1ID)
	if err != nil || inviter == nil || inviter.Host == nil || inviter.URI == nil {
		return
	}
	ctx := c.Request().Context()
	sessionID, ok := h.fedCache.GetSessionByGame(ctx, game.ID)
	if !ok {
		return
	}
	// show-game 経由で User2 が pending game を表示するたびに呼ばれる
	// 経路があるため、session ごとに 1 回だけ送るよう guard する
	// (#417 Devin review)。相手側 (CherryPick) でも zset idempotent なので
	// 重複しても実害は無いが、無駄な deliver job を抑える。
	if h.fedCache.IsJoinSent(ctx, sessionID) {
		return
	}
	join := corereversi.RenderJoin(h.baseURL, sessionID,
		h.baseURL+"/users/"+accepter.ID, *inviter.URI, time.Now().UTC().Format(time.RFC3339))
	body, err := json.Marshal(join)
	if err != nil {
		return
	}
	if err := h.deliverer.DeliverToUser(accepter.ID, inviter, body); err != nil {
		return
	}
	h.fedCache.MarkJoinSent(ctx, sessionID)
}

// CancelMatch handles POST /api/reversi/cancel-match.
// Service.CancelGame 経由で: Leave をリモート相手に配信 + canceled
// イベントを WebSocket に publish + fedCache の session mapping も
// 片付く。svc 未配線な旧テスト互換のため repo.Delete フォールバックも残す。
func (h *Handler) CancelMatch(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	ctx := c.Request().Context()
	// userId 未指定 = ランダムマッチの取り消し。待機列から自分を外す (#2407)。
	// upstream matchAnyUserCancel に対応する。ここで外さないと、取り消した
	// つもりの利用者が他人にマッチされ続ける。
	if req.UserID == "" && h.svc != nil {
		if err := h.svc.CancelMatchAny(ctx, user.ID); err != nil {
			slog.Warn("reversi cancel-match: dequeue from match-any failed",
				"userId", user.ID, "err", err)
		}
	}
	games, _ := h.repo.ListByUser(user.ID, 10)
	for _, g := range games {
		if g.IsStarted || g.IsEnded {
			continue
		}
		// userId 指定時は upstream matchSpecificUserCancel 互換 (cancel-match.ts:
		// 33-38): 自分がその相手に送った招待 (User1=自分, User2=相手) だけを
		// 取り消す。未指定時は自分の pending を全部片付ける (待機列からの離脱は
		// 上で済ませてある)。
		if req.UserID != "" && (g.User1ID != user.ID || g.User2ID != req.UserID) {
			continue
		}
		if h.svc != nil {
			// ListByUser snapshot 後に相手が ready / start させた場合、
			// ErrAlreadyStarted で失敗する。その場合は進行中のゲームを
			// 潰さないよう fedCache の掃除もせず skip する
			// (#417 Devin review: TOCTOU)。ErrAlreadyStarted 以外の
			// 想定外エラー (DB障害等) は observability のため WARN する。
			if err := h.svc.CancelGame(ctx, g.ID, user.ID); err != nil {
				if !errors.Is(err, corereversi.ErrAlreadyStarted) {
					slog.Warn("reversi cancel-match: cancel failed",
						"gameId", g.ID, "userId", user.ID, "err", err)
				}
				continue
			}
		} else {
			_ = h.repo.Delete(g.ID)
		}
		// fedCache 片付けは Service.CancelGame が既に実行済み (#417 Devin
		// review で全終了経路に統一)。
	}
	return c.NoContent(http.StatusNoContent)
}

// Surrender handles POST /api/reversi/surrender.
// 対局中 (IsStarted かつ not IsEnded) のみ許可する。service.Surrender を経由
// することで WebSocket チャネルに `ended` イベントを publish するので、両
// プレイヤーのクライアントが即座に終局状態を検出できる。
func (h *Handler) Surrender(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		GameID string `json:"gameId"`
	}
	if err := c.Bind(&req); err != nil || req.GameID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "gameId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	// federation Leave を送るために winner (= 相手) を先に引いておく。
	// service.Surrender は repo 越しに game を読むが、federation session の
	// lookup は handler のタイミングで必要なので preload する。
	game, err := h.repo.FindByID(req.GameID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_GAME", "No such game.", "ace0b11f-e0a6-4076-a30d-e8284c81b2df"))
	}
	// #2106 L16: upstream surrender.ts は isEnded(ALREADY_ENDED) を player 判定(ACCESS_DENIED)
	// より先に評価する。終了済 game への非プレイヤー surrender は ALREADY_ENDED(400) を返す。
	if game.IsEnded {
		return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_ENDED", "Game has already ended.", "6c2ad4a6-cbf1-4a5b-b187-b772826cfc6d"))
	}
	if game.User1ID != user.ID && game.User2ID != user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "6e04164b-a992-4c93-8489-2123069973e1"))
	}

	// service が注入されていればそちらを経由する (本来のパス)。
	// フォールバックは従来の repo 直接操作 (service 未注入の古いテスト互換)。
	if h.svc != nil {
		if err := h.svc.Surrender(c.Request().Context(), req.GameID, user.ID); err != nil {
			return surrenderErrorResponse(c, err)
		}
	} else {
		// svc 未配線 path は legacy test 互換。winnerID もここで計算する。
		var winnerID string
		if game.User1ID == user.ID {
			winnerID = game.User2ID
		} else {
			winnerID = game.User1ID
		}
		now := time.Now()
		game.IsEnded = true
		game.EndedAt = &now
		game.WinnerID = &winnerID
		game.SurrenderedUserID = &user.ID
		_ = h.repo.Update(game)
	}

	// Leave 配信 + fedCache cleanup は Service.Surrender が実行済み
	// (#417 Devin review で全終了経路を Service 側に統一)。
	return c.NoContent(http.StatusNoContent)
}

// surrenderErrorResponse maps core/reversi service errors to Misskey-
// compatible HTTP error bodies. Unknown errors fall back to 500.
//
// 本 helper は Surrender ハンドラ専用なので NO_SUCH_GAME / ACCESS_DENIED /
// ALREADY_ENDED はすべて upstream surrender.ts:20,26,32 の endpoint 固有
// UUID を使う (#1553)。NOT_STARTED は upstream surrender に存在しない
// cherrypick 由来 code のため独自 UUID を維持する。
func surrenderErrorResponse(c echo.Context, err error) error {
	switch {
	case errors.Is(err, corereversi.ErrGameNotFound):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_GAME", "No such game.", "ace0b11f-e0a6-4076-a30d-e8284c81b2df"))
	case errors.Is(err, corereversi.ErrNotPlayer):
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "6e04164b-a992-4c93-8489-2123069973e1"))
	case errors.Is(err, corereversi.ErrAlreadyEnded):
		return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_ENDED", "Game has already ended.", "6c2ad4a6-cbf1-4a5b-b187-b772826cfc6d"))
	case errors.Is(err, corereversi.ErrNotStarted):
		return c.JSON(http.StatusBadRequest, apierr.Error("NOT_STARTED", "Game has not started yet.", "ac4bb45f-ea81-44d3-a5b3-fe5f30be2c8d"))
	}
	return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
}

// Verify handles POST /api/reversi/verify — verify game integrity.
// クライアントが送ってくる `crc32` を保存済みの game.crc32 と比較し、
// 不一致なら `{desynced: true, game}` を返してフロント側で restoreGame を
// 走らせる。一致なら `{desynced: false}` のみ。
func (h *Handler) Verify(c echo.Context) error {
	var req struct {
		GameID string `json:"gameId"`
		CRC32  string `json:"crc32"`
	}
	// crc32 は upstream verify.ts:38-41 で required (gameId と同列)。欠落を
	// 黙って desynced=false にすると client 側の desync が一生検出されない。
	if err := c.Bind(&req); err != nil || req.GameID == "" || req.CRC32 == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "gameId and crc32 are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	game, err := h.repo.FindByID(req.GameID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		// verify の noSuchGame は upstream verify.ts:17 の endpoint 固有 UUID。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_GAME", "No such game.", "8fb05624-b525-43dd-90f7-511852bdfeee"))
	}

	// upstream checkCrc は DB 保存済みの game.crc32 と直接比較する
	// (ReversiService.ts:618)。ログ再生で都度再計算すると保存値と別ソースに
	// なり desync 判定が本家と乖離するため、StartGame / PutStone が更新する
	// 保存値をそのまま使う (#1553)。保存値が無い (開始前) 場合は本家同様
	// 不一致 = desynced 扱いになり、client は game から状態を復元する。
	desynced := game.CRC32 == nil || *game.CRC32 != req.CRC32
	resp := map[string]any{"desynced": desynced}
	if desynced {
		resp["game"] = packGame(game, h.idGen)
	}
	return c.JSON(http.StatusOK, resp)
}

// matchAny implements the random-opponent path of /api/reversi/match (#2407).
//
// upstream `ReversiService.matchAnyUser` と同じ順序で 3 段階に分かれる。
//
//  1. 既に成立している未開始の対局があればそれを返す (multiple=false のとき)
//  2. 自分宛ての招待があれば、それを受ける
//  3. 待機列で相手を探す。居なければ自分が待機列に入って 204
//
// 順序が意味を持つ。2 を 3 より先に見ないと、自分を名指しで誘っている相手が
// 居るのに無関係な相手とマッチしてしまう。
//
// # 連合はしない
//
// 待機列 (3) に載るのは **このインスタンスで認証を通した local user だけ**なので、
// ランダムマッチの相手がリモートになることはない。upstream Misskey も
// yojo-art/cherrypick も連合ランダムマッチは持っていないので、意図的に揃えている。
//
// ただし (2) の招待は inbound AP Invite 由来 = リモートからのものがありうる。
// そちらは specific-match と同じ accept 経路 (`sendJoinForAcceptedInvite`) を
// 通るので、連合対局として正しく成立する。
func (h *Handler) matchAny(c echo.Context, user *model.User, noIrregularRules, multiple bool) error {
	ctx := c.Request().Context()

	// 1. 直近の未開始対局の再利用。upstream は multiple=true でこれを飛ばして
	//    毎回新しく探す (ReversiService.ts:146)。
	if !multiple {
		if g := h.findPendingGameFor(user.ID); g != nil {
			return c.JSON(http.StatusOK, packGame(g, h.idGen))
		}
	}

	// 2. 自分宛ての招待を消費する。mk-go では招待が reversi_game 行なので、
	//    「自分が User2 の未開始 game」がそれにあたる。specific-match の
	//    accept 経路と同じ扱いにして、相手に Join を返す。
	if g := h.findPendingInvitationTo(user.ID); g != nil {
		h.sendJoinForAcceptedInvite(c, user, g)
		return c.JSON(http.StatusOK, packGame(g, h.idGen))
	}

	// 3. 待機列。
	if h.svc == nil {
		return c.NoContent(http.StatusNoContent)
	}
	res, err := h.svc.EnqueueMatchAny(ctx, user.ID, noIrregularRules)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	if !res.Matched() {
		// 待機列に入った。frontend の matchHeatbeat が再呼び出しする。
		return c.NoContent(http.StatusNoContent)
	}

	// 相手を確保できたので対局を作る。**待機列から取れた時点で相手は確定して
	// いる** (ZRem の戻り値で排他済み) ので、ここで作った game が二重になる
	// ことはない。
	now := time.Now()
	game := &model.ReversiGame{
		ID:                   h.idGen.Generate(now),
		User1ID:              res.OpponentID,
		User2ID:              user.ID,
		Map:                  defaultMap,
		BW:                   "random",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
	if err := h.repo.Create(game); err != nil {
		// 対局を作れなかったら相手を待機列に戻す。ここで戻さないと、確保だけ
		// して対局が無い「消えた待機者」になる。
		if h.svc != nil {
			_, _ = h.svc.EnqueueMatchAny(ctx, res.OpponentID, res.NoIrregularRules)
		}
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// 相手側は heartbeat を待たずに対局へ入れるよう stream で知らせる。
	// specific-match の invited と同じ経路。
	if h.streamPub != nil {
		h.streamPub.PublishInvited(res.OpponentID, user)
	}
	return c.JSON(http.StatusOK, packGame(game, h.idGen))
}

// findPendingGameFor returns the caller's most recent unstarted game within the
// dedup window, regardless of who invited whom. upstream matchAnyUser の
// 「既にマッチしている対局が無いか探す(3分以内)」に対応する。
func (h *Handler) findPendingGameFor(userID string) *model.ReversiGame {
	games, err := h.repo.ListByUser(userID, 20)
	if err != nil {
		return nil
	}
	for _, g := range games {
		if g.IsStarted || g.IsEnded {
			continue
		}
		if !h.withinMatchDedupWindow(g) {
			continue
		}
		return g
	}
	return nil
}

// findPendingInvitationTo returns any pending invitation addressed to userID
// (= unstarted game where the caller is User2), regardless of the inviter.
//
// specific-match は相手が分かっているので FindPendingInvitation を使うが、
// any-match は「誰でもいいから自分宛ての招待」を探す。
func (h *Handler) findPendingInvitationTo(userID string) *model.ReversiGame {
	games, err := h.repo.ListByUser(userID, 20)
	if err != nil {
		return nil
	}
	for _, g := range games {
		if g.IsStarted || g.IsEnded {
			continue
		}
		if g.User2ID == userID && g.User1ID != userID {
			return g
		}
	}
	return nil
}
