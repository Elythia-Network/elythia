package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// Handler handles MiAuth endpoints.
type Handler struct {
	// background は応答の後の処理 (token 作成の通知) を起動する箱 (#3661)。世代の停止で
	// 待つ。nil なら素の goroutine で起動する (テスト)。
	background *bgwork.Group
	// detailExtras は UserDetailed のピン留め・移行先を users/show と同じ規則で埋める (#3330)。
	detailExtras userpack.DetailExtras
	repo         repository.AuthSessionRepository
	cfg          *config.Config
	idGen        id.Generator
	// userRepo は miauth/:session/check で承認ユーザーを UserDetailedNotMe で
	// pack するために使う (#1224)。未配線時は user フィールドを省く。
	userRepo repository.UserRepository
	// tokenNotifier は gen-token で access token を生成した時に本人へ
	// 'createToken' 通知を送る (#1559)。未配線なら通知しない。
	tokenNotifier TokenNotifier
	// entityPacker は利用者や投稿を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3792)。組み立てで渡す。
	entityPacker *entity.Packer
}

// SetEntityPacker wires the Packer this handler packs entities with (#3792).
func (h *Handler) SetEntityPacker(p *entity.Packer) { h.entityPacker = p }

// HasEntityPacker reports whether SetEntityPacker received a Packer that
// rewrites media URLs. 無いと avatarUrl などを生のまま返し、閲覧者の IP が
// リモートへ漏れる。
func (h *Handler) HasEntityPacker() bool { return h.entityPacker.HasMediaURL() }

// TokenNotifier records a 'createToken' notification when a token is generated
// (#1559)。循環依存を避けるため interface で受け取る (実装は
// core/notification.Hook)。
type TokenNotifier interface {
	OnCreateToken(userID string)
}

// SetBackground sets the group the work after a response runs in, so stopping
// the server generation waits for it (#3661).
func (h *Handler) SetBackground(g *bgwork.Group) { h.background = g }

// HasBackground reports whether SetBackground received a group.
func (h *Handler) HasBackground() bool { return h.background != nil }

// NewHandler creates a new auth handler.
func NewHandler(repo repository.AuthSessionRepository, cfg *config.Config, idGen id.Generator) *Handler {
	return &Handler{repo: repo, cfg: cfg, idGen: idGen}
}

// SetUserRepo wires the user repository used by MiAuthCheck to pack the
// approving user.
func (h *Handler) SetUserRepo(r repository.UserRepository) { h.userRepo = r }

// SetTokenNotifier attaches a TokenNotifier so gen-token creates a
// 'createToken' notification (upstream miauth/gen-token)。
func (h *Handler) SetTokenNotifier(n TokenNotifier) { h.tokenNotifier = n }

// SessionGenerate handles POST /api/auth/session/generate.
func (h *Handler) SessionGenerate(c echo.Context) error {
	var req struct {
		AppSecret *string `json:"appSecret"`
	}
	// 空文字は本家と同じく、見つからないアプリとして扱う(#3764)。
	if err := c.Bind(&req); err != nil || req.AppSecret == nil {
		return apierr.JSONInvalidParam(c)
	}

	app, err := h.repo.FindAppBySecret(*req.AppSecret)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_APP", "No such app.", "92f93e63-428e-4f2f-a5a4-39e1407fe998"))
	}

	token := uuid.New().String()
	session := &model.AuthSession{
		ID:    h.idGen.Generate(time.Now()),
		Token: token,
		AppID: app.ID,
	}
	if err := h.repo.CreateSession(session); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"token": token,
		"url":   fmt.Sprintf("%s/auth/%s", h.cfg.URL, token),
	})
}

// SessionShow handles POST /api/auth/session/show.
func (h *Handler) SessionShow(c echo.Context) error {
	var req struct {
		Token *string `json:"token"`
	}
	// 空文字は本家と同じく、見つからないセッションとして扱う(#3764)。
	if err := c.Bind(&req); err != nil || req.Token == nil {
		return apierr.JSONInvalidParam(c)
	}

	session, err := h.repo.FindSessionByToken(*req.Token)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_SESSION", "No such session.", "bd72c97d-eba7-4adb-a467-f171b8847250"))
	}

	// app.isAuthorized は認証済 caller (me) のときのみ含める (upstream
	// AppEntityService、#1557)。me が app の access_token を持つかで判定する。
	var isAuthorized *bool
	if me := middleware.GetUser(c); me != nil && session.App != nil {
		_, atErr := h.repo.FindAccessTokenByAppAndUser(session.App.ID, me.ID)
		v := atErr == nil
		isAuthorized = &v
	}

	return c.JSON(http.StatusOK, packSession(session, isAuthorized))
}

// Accept handles POST /api/auth/accept.
func (h *Handler) Accept(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Token *string `json:"token"`
	}
	// 空文字は本家と同じく、見つからないセッションとして扱う(#3764)。
	if err := c.Bind(&req); err != nil || req.Token == nil {
		return apierr.JSONInvalidParam(c)
	}
	// **プラグインが管理するアカウントには app token を発行しない (#3468)。**
	// 外からはそのアカウントの native token が通らないので、ここに来るのは
	// プラグインの AsUser だけ。通すと、外で使える資格情報をプラグインが
	// 作れてしまう。
	if user.IsPluginManaged() {
		return c.JSON(http.StatusBadRequest, apierr.PluginManagedAccount())
	}

	session, err := h.repo.FindSessionByToken(*req.Token)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_SESSION", "No such session.", "9c72d8de-391a-43c1-9d06-08d29efde8df"))
	}

	// アクセストークンが既に存在するか確認
	_, tokenErr := h.repo.FindAccessTokenByAppAndUser(session.AppID, user.ID)
	if tokenErr != nil {
		// アクセストークンを新規生成
		tokenStr := misc.SecureRandomHex(32)
		hash := sha256Hex(tokenStr + session.App.Secret)
		now := time.Now()
		accessToken := &model.AccessToken{
			ID:         h.idGen.Generate(now),
			Token:      tokenStr,
			Hash:       hash,
			UserID:     user.ID,
			AppID:      &session.AppID,
			Permission: session.App.Permission,
			LastUsedAt: &now,
		}
		if err := h.repo.CreateAccessToken(accessToken); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
	}

	// セッションにユーザーIDを設定
	if err := h.repo.UpdateSessionUserID(session.ID, user.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	return c.NoContent(http.StatusNoContent)
}

// SessionUserkey handles POST /api/auth/session/userkey.
func (h *Handler) SessionUserkey(c echo.Context) error {
	var req struct {
		AppSecret *string `json:"appSecret"`
		Token     *string `json:"token"`
	}
	// 空文字は本家と同じく、見つからないアプリかセッションとして扱う(#3764)。
	if err := c.Bind(&req); err != nil || req.AppSecret == nil || req.Token == nil {
		return apierr.JSONInvalidParam(c)
	}

	app, err := h.repo.FindAppBySecret(*req.AppSecret)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_APP", "No such app.", "fcab192a-2c5a-43b7-8ad8-9b7054d8d40d"))
	}

	session, err := h.repo.FindSessionByTokenAndAppID(*req.Token, app.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_SESSION", "No such session.", "5b5a1503-8bc8-4bd0-8054-dc189e8cdcb3"))
	}

	if session.UserID == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("PENDING_SESSION", "This session is not yet approved.", "8c8a4145-02cc-4cca-8e66-29ba60445a8e"))
	}
	// プラグインが管理するアカウント (#3468) の token は払い出さない。accept が
	// 承認させないので本来ここには来ないが、それより前のデータなどで承認済みの
	// session があっても資格情報を外へ出さない。居ない session と同じ応答にする。
	if session.User != nil && session.User.IsPluginManaged() {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_SESSION", "No such session.", "5b5a1503-8bc8-4bd0-8054-dc189e8cdcb3"))
	}

	accessToken, err := h.repo.FindAccessTokenByAppAndUser(app.ID, *session.UserID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// セッション削除（使い捨て）
	_ = h.repo.DeleteSession(session.ID)

	resp := map[string]any{
		"accessToken": accessToken.Token,
		"user":        h.packUserDetailed(session.User, *session.UserID),
	}
	return c.JSON(http.StatusOK, resp)
}

// packUserDetailed packs the approving user with the UserDetailedNotMe schema,
// matching upstream auth/session/userkey (res.user is ref 'UserDetailedNotMe',
// userEntityService.pack(session.userId, null, {schema:'UserDetailedNotMe'})).
// userRepo は router で必ず wire されるが、未配線 (unit test 等) のときは
// プロフィール無しで pack して最低限 UserDetailed shape を維持する。
func (h *Handler) packUserDetailed(u *model.User, userID string) any {
	if u == nil {
		return map[string]any{}
	}
	var profile *model.UserProfile
	if h.userRepo != nil {
		profile, _ = h.userRepo.FindProfileByUserID(userID)
	}
	return h.packDetailedNotMe(u, profile)
}

// packDetailedNotMe packs u like upstream pack(user, null, {schema:
// 'UserDetailedNotMe'}): the pinned notes / page and move targets are filled
// as seen by an anonymous viewer.
func (h *Handler) packDetailedNotMe(u *model.User, profile *model.UserProfile) entity.UserDetailed {
	d := h.entityPacker.PackUserDetailed(u, profile, h.idGen)
	if h.detailExtras != nil {
		h.detailExtras.FillDetailedExtras(context.Background(), nil, u, profile, &d)
	}
	return d
}

// GenToken handles POST /api/miauth/gen-token.
// MiAuth用のアクセストークンを直接生成する（App不要）。
func (h *Handler) GenToken(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Session     *string  `json:"session"`
		Name        *string  `json:"name"`
		Description *string  `json:"description"`
		IconURL     *string  `json:"iconUrl"`
		Permission  []string `json:"permission"`
	}
	if err := c.Bind(&req); err != nil || req.Permission == nil {
		return apierr.JSONInvalidParam(c)
	}
	// auth/accept と同じ理由で、管理するアカウントには発行しない (#3468)。
	if user.IsPluginManaged() {
		return c.JSON(http.StatusBadRequest, apierr.PluginManagedAccount())
	}

	tokenStr := misc.SecureRandomHex(32)
	now := time.Now()
	accessToken := &model.AccessToken{
		ID:          h.idGen.Generate(now),
		LastUsedAt:  &now,
		Token:       tokenStr,
		Hash:        sha256Hex(tokenStr),
		UserID:      user.ID,
		Session:     req.Session,
		Name:        req.Name,
		Description: req.Description,
		IconURL:     req.IconURL,
		Permission:  req.Permission,
	}
	if err := h.repo.CreateAccessToken(accessToken); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// createToken 通知を本人へ送る (upstream miauth/gen-token、#1559)。
	if h.tokenNotifier != nil {
		uid := user.ID
		h.background.Go(func(context.Context) { h.tokenNotifier.OnCreateToken(uid) })
	}

	return c.JSON(http.StatusOK, map[string]any{"token": tokenStr})
}

// MiAuthCheck handles POST /api/miauth/:session/check.
//
// 3rd-party クライアントは MiAuth 承認 (= GenToken で session 紐付き access
// token 作成) 後、本 endpoint を polling して token を取得する。upstream
// ApiServerService の /miauth/:session/check 相当: session に対応する未取得の
// token があれば fetched=true にして {ok, token, user} を返し、無ければ
// {ok:false} を返す。認証不要 (token をまだ持たない client が叩くため)。
func (h *Handler) MiAuthCheck(c echo.Context) error {
	session := c.Param("session")
	if session == "" {
		return c.JSON(http.StatusOK, map[string]any{"ok": false})
	}
	tok, err := h.repo.FindAccessTokenBySession(session)
	if err != nil || tok == nil || tok.Session == nil || tok.Fetched {
		// session 不在 / 既に取得済 (one-time) は ok:false。
		return c.JSON(http.StatusOK, map[string]any{"ok": false})
	}
	// **持ち主は、取得済みにする前に確かめる。** プラグインが管理するアカウント
	// (#3468) の token は、gen-token / accept が作らないので本来ここには無い。
	// それでも行があれば (それより前のデータや手で入れた行) 払い出さない。外では
	// 使えない token だが、資格情報を外へ出す口を残さない。持ち主を読めない障害の
	// ときも払い出さず、取得済みにもしない (クライアントは polling で取り直せる)。
	var owner *model.User
	if h.userRepo != nil {
		u, uerr := h.userRepo.FindByID(tok.UserID)
		switch {
		case uerr != nil && !repository.IsNotFound(uerr):
			return c.JSON(http.StatusOK, map[string]any{"ok": false})
		case uerr == nil && u != nil && u.IsPluginManaged():
			return c.JSON(http.StatusOK, map[string]any{"ok": false})
		case uerr == nil:
			owner = u
		}
	}
	// one-time 取得: fetched=false→true の遷移をアトミックに行い、勝った
	// リクエストだけが token を払い出す。並行 polling で同じ token が二重に
	// 渡ることを防ぐ (上の Fetched チェックは fast path で、ここが真の関門)。
	won, err := h.repo.MarkAccessTokenFetched(tok.ID)
	if err != nil || !won {
		return c.JSON(http.StatusOK, map[string]any{"ok": false})
	}

	resp := map[string]any{"ok": true, "token": tok.Token}
	if owner != nil {
		profile, _ := h.userRepo.FindProfileByUserID(owner.ID)
		// 本家 ApiServerService の miauth check も pack(token.userId, null,
		// {schema: 'UserDetailedNotMe'}) (#3330)。
		resp["user"] = h.packDetailedNotMe(owner, profile)
	}
	return c.JSON(http.StatusOK, resp)
}

func packSession(s *model.AuthSession, isAuthorized *bool) map[string]any {
	result := map[string]any{
		"id":    s.ID,
		"token": s.Token,
	}
	if s.App != nil {
		app := map[string]any{
			"id":          s.App.ID,
			"name":        s.App.Name,
			"callbackUrl": s.App.CallbackURL,
			"permission":  s.App.Permission,
		}
		// isAuthorized は me!=null のときだけ含める (upstream、#1557)。
		if isAuthorized != nil {
			app["isAuthorized"] = *isAuthorized
		}
		result["app"] = app
	}
	return result
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
