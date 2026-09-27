package signup

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/captcha"
	coreemail "github.com/shiroha-a/mk/internal/core/email"
	"github.com/shiroha-a/mk/internal/core/role"
	coresignup "github.com/shiroha-a/mk/internal/core/signup"
	"github.com/shiroha-a/mk/internal/core/signupform"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/l10n"
	"github.com/shiroha-a/mk/internal/misc/id"
	miscsmtp "github.com/shiroha-a/mk/internal/misc/smtp"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// TicketStore abstracts registration_ticket DB operations for testability.
type TicketStore interface {
	FindByCode(code string) (*model.RegistrationTicket, error)
	MarkUsed(ticketID, userID string) error
	// ClaimForSignup / ReleaseClaim reserve and release a ticket around
	// account creation (repository.RegistrationTicketRepository)。
	ClaimForSignup(ticketID string, emailRequired bool) (bool, error)
	ReleaseClaim(ticketID string) error
	// MarkPending records usedAt + pendingUserId for an email-confirmation
	// signup (確認メール再送防止窓、#2083)。
	MarkPending(ticketID, pendingID string) error
}

// SigninRecorder fires the side-effects of a successful login (signin history,
// login notification, main-stream publish) after account creation, matching
// upstream SignupApiService -> signinService.signin (#1804)。実装は
// internal/api/signin.Handler.RecordSuccessfulSignin。未配線なら副作用を発火しない
// だけ (response shape は不変)。
type SigninRecorder interface {
	RecordSuccessfulSignin(userID, ip string, headers http.Header)
}

// Handler handles POST /api/signup.
type Handler struct {
	signupService *coresignup.Service
	metaRepo      repository.MetaRepository
	idGen         id.Generator
	captchaSvc    *captcha.Service // optional
	ticketStore   TicketStore      // optional, invitation code検証用
	testMode      bool             // true のとき disableRegistration / captcha をバイパス (本家 TS と同じ)
	// emailSender は EmailRequiredForSignup フローで確認メールを送る callback。
	// router 配線時に SMTP infra を closure に閉じ込めて注入する。未設定の場合は
	// 確認メールが送られないだけで pending row 自体は作る (テスト用)。
	// text と html 両方を受けて multipart/alternative で送出する想定 (html 空なら
	// text/plain only)。
	emailSender func(to string, msg miscsmtp.Message)
	// serverURL は確認 link の base。emailSender とセットで設定。
	serverURL string
	// emailValidationClient は verifymail / truemail SaaS API への outbound
	// に使う SSRF-safe HTTP client (#638)。nil ならデフォルト client が使われる。
	emailValidationClient *http.Client
	// signinRecorder はアカウント作成成功時に signin 副作用 (履歴 / login 通知 /
	// main publish) を発火する。upstream SignupApiService が signinService.signin を
	// 呼ぶのに相当 (#1804)。未配線なら副作用なし。
	signinRecorder SigninRecorder
	// userPolicies は作成直後の利用者の**実効** policy を返す (#2673)。
	// 未配線なら従来どおり素の default にフォールバックする。
	userPolicies UserPolicyResolver
	// emailDB は email-address/available の重複判定用 (#2791 で router.go の
	// inline closure から移設)。
	emailDB *gorm.DB
	// userRepo / usedUsernameRepo は username/available 用 (#2791 で router.go の
	// inline closure から移設)。
	userRepo         repository.UserRepository
	usedUsernameRepo repository.UsedUsernameRepository
	// applications は承認制の登録 (#2569)。未配線なら該当 endpoint は 503。
	// ticketStore は承認済み申請でも使う。**発行するのはメール確認の経路だけ**
	// (#2813) で、確認リンクを踏んだ時点で消費する。即時作成は発行しないが、
	// メール必須を切る前に始まっていた確認待ちの残骸を破棄するのに触る。
	applications SignupApplications
	// formTokens は captcha の実 provider が 1 つも無いときに
	// signup-application/apply を守る署名付きフォームトークン (#2806)。
	// **captcha の代替ではない** — 位置づけは core/signupform の doc を見ること。
	formTokens *signupform.Issuer
}

// SetFormTokenIssuer wires the signed form tokens used when no real captcha
// provider is configured (#2806).
func (h *Handler) SetFormTokenIssuer(i *signupform.Issuer) {
	h.formTokens = i
}

// HasFormTokens reports whether the signup form token issuer is wired.
//
// 未配線だと、captcha が 1 つも設定されていないインスタンスで申請 endpoint の
// 唯一の簡易チェックが素通しになる。router のコメントも同じ理由で critical
// wiring に載せている。
func (h *Handler) HasFormTokens() bool { return h.formTokens != nil }

// SetSigninRecorder wires the recorder used to fire login side-effects after
// account creation (#1804)。
func (h *Handler) SetSigninRecorder(r SigninRecorder) {
	h.signinRecorder = r
}

// fireSigninSideEffects records the successful-login side-effects for a newly
// created account (#1804). headers は Echo が request を再利用する前に Clone する。
func (h *Handler) fireSigninSideEffects(c echo.Context, userID string) {
	if h.signinRecorder == nil || userID == "" {
		return
	}
	h.signinRecorder.RecordSuccessfulSignin(userID, c.RealIP(), c.Request().Header.Clone())
}

// SetTestMode enables test-mode bypass (本家 `process.env.NODE_ENV !== 'test'` 相当).
func (h *Handler) SetTestMode(v bool) {
	h.testMode = v
}

// NewHandler creates a new signup Handler.
func NewHandler(signupService *coresignup.Service, metaRepo repository.MetaRepository, idGen id.Generator) *Handler {
	return &Handler{signupService: signupService, metaRepo: metaRepo, idGen: idGen}
}

// SetCaptcha attaches a CaptchaService for signup verification.
func (h *Handler) SetCaptcha(svc *captcha.Service) {
	h.captchaSvc = svc
}

// SetTicketStore attaches a TicketStore for invitation code validation.
func (h *Handler) SetTicketStore(ts TicketStore) {
	h.ticketStore = ts
}

// SetEmailSender wires a callback used to send signup confirmation emails.
// reset-password handler と同じ pattern。serverURL は 確認 link 生成に使う。
// callback は text + html 両 body を受け、multipart/alternative で送る (#600 item 4)。
func (h *Handler) SetEmailSender(serverURL string, send func(to string, msg miscsmtp.Message)) {
	h.serverURL = serverURL
	h.emailSender = send
}

// SetEmailValidationClient wires the outbound HTTP client used by verifymail
// / truemail siteverify calls (#638). production では SSRF-safe + forward
// proxy 経由の client を渡すこと。
func (h *Handler) SetEmailValidationClient(c *http.Client) {
	h.emailValidationClient = c
}

type signupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Host は TestMode でのみ読む。upstream SignupApiService も
	// NODE_ENV === 'test' のときだけ body.host を受け付け、リモートユーザーを
	// 直接作れるようにしている (e2e が「リモートユーザーのノートが LTL に
	// 出ない」等を検証するのに使う)。本番では無視する。
	Host           string `json:"host"`
	EmailAddress   string `json:"emailAddress"`
	InvitationCode string `json:"invitationCode"`
	// CAPTCHA tokens (フィールド名はTS版スキーマに準拠)
	HcaptchaResponse    string `json:"hcaptcha-response"`
	RecaptchaResponse   string `json:"g-recaptcha-response"`
	TurnstileResponse   string `json:"turnstile-response"`
	McaptchaResponse    string `json:"m-captcha-response"`
	TestcaptchaResponse string `json:"testcaptcha-response"`
}

// registrationClosedError is returned by every signup route while the server
// accepts no registrations (mk-go, #3186).
//
// **招待コードの誤り (INVITATION_CODE_INVALID) と分ける。** 同じにすると、利用者は
// コードを打ち間違えたと読んでやり直し続ける。
func registrationClosedError(c echo.Context) error {
	return c.JSON(http.StatusForbidden,
		apierr.Error("REGISTRATION_CLOSED", "This server is not accepting new registrations.", "2776429f-df02-4b56-a708-30d5ddda7b95"))
}

// duplicatedUsernameError は username 重複時の error response を返す。
//
// upstream Misskey TS は \`/api/signup\` の username 重複を Fastify-style
// reply error 形式 \`{statusCode:400, error:"Bad Request",
// message:"Error: DUPLICATED_USERNAME"}\` で返す (third_party の
// SignupApiService.ts:174 で \`throw new FastifyReplyError(400,
// 'DUPLICATED_USERNAME')\`)。mk-go も同 status / shape に揃える
// (#798 で status / code、#802 で body shape)。
//
// signup-pending 経路 / 通常 signup / signup-pending promote の 3 箇所から
// 参照されるので helper 化している。
func duplicatedUsernameError(c echo.Context) error {
	return apierr.FastifyReply(c, http.StatusBadRequest, "DUPLICATED_USERNAME")
}

// Signup handles POST /api/signup.
func (h *Handler) Signup(c echo.Context) error {
	var req signupRequest
	if err := c.Bind(&req); err != nil || req.Username == "" || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	meta, err := h.metaRepo.Fetch()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// 「受け付けない」は招待コードを見る前に返す (#3186)。testMode でも迂回しない —
	// upstream の e2e はこの列を立てないので、迂回させる理由が無い。
	if meta.RegistrationClosed {
		return registrationClosedError(c)
	}

	// 承認制のときは通常の登録経路を閉じる (#2557)。
	//
	// **承認制それ自体がゲート。** disableRegistration と組み合わせる運用にすると、
	// 訪問者には「招待制」と表示されてしまい実態と食い違う。承認フローは
	// signupService を直接呼ぶのでこの分岐を通らない。
	if !h.testMode && meta.ApprovalRequiredForSignup {
		return c.JSON(http.StatusForbidden,
			apierr.Error("APPROVAL_REQUIRED", "This server requires an approved application to sign up.", "5b6c7d8e-9f0a-4b1c-8d2e-3f4a5b6c7d8e"))
	}

	// 登録無効時はinvitation code必須 (テストモードではバイパス — 本家 TS 互換)
	var ticket *model.RegistrationTicket
	if !h.testMode && meta.DisableRegistration {
		t, vErr := h.validateInvitationCode(req.InvitationCode, meta.EmailRequiredForSignup)
		if vErr != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVITATION_CODE_INVALID", "Invalid invitation code.", "11e71a03-43c4-4a99-92cf-bb7e2c581998"))
		}
		ticket = t
	}

	// CAPTCHA検証 (テストモードではスキップ)
	if !h.testMode && h.captchaSvc != nil {
		tokens := captcha.CaptchaTokens{
			Hcaptcha:    req.HcaptchaResponse,
			Recaptcha:   req.RecaptchaResponse,
			Turnstile:   req.TurnstileResponse,
			Mcaptcha:    req.McaptchaResponse,
			Testcaptcha: req.TestcaptchaResponse,
		}
		if err := h.captchaSvc.Verify(c.Request().Context(), tokens); err != nil {
			// upstream は captcha verify 失敗時に Fastify-style reply error
			// (`throw new FastifyReplyError(400, err)`) を返す (SignupApiService.ts:81)。
			// shape を揃えて drop-in 互換を担保する (#802)。
			return apierr.FastifyReply(c, http.StatusBadRequest, "CAPTCHA_FAILED")
		}
	}

	// emailRequiredForSignup=true: 即時に user は作らず user_pending に積み、
	// 確認メールを送って /api/signup-pending で本登録させる Misskey TS 互換フロー。
	// testMode は完全バイパスしてフロント test での通常 signup を妨げない。
	if !h.testMode && meta.EmailRequiredForSignup {
		if req.EmailAddress == "" {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "emailAddress is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		if verr := validateEmailWithMeta(c.Request().Context(), meta, req.EmailAddress, h.emailValidationClient); verr != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Email is not available.", "a25440a9-451e-41de-b291-00a8f29fbca6"))
		}
		// **確認済みの重複を弾く。** 判定は `email-address/available` にしか
		// 実装が無く、実際に書くここは通っていなかった。upstream は
		// `validateEmailForAccount` を全経路が通る。DB にも UNIQUE が無いので、
		// 1 つのメールボックスから無制限にアカウントを作れた
		// (`emailRequiredForSignup` が sockpuppet 対策として期待する性質が失われる)。
		if h.userRepo != nil {
			inUse, ierr := h.userRepo.EmailVerifiedInUse(req.EmailAddress)
			if ierr != nil {
				// **判定できないときは通さない。** 重複検査を素通りさせない。
				slog.Error("signup: cannot check whether the email is in use", "err", ierr)
				return apierr.JSONInternalError(c)
			}
			if inUse {
				return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Email is not available.", "a25440a9-451e-41de-b291-00a8f29fbca6"))
			}
		}
		// 招待制併用時は ticket.ID を pending row に保存しておき、
		// PromotePending 完了時に MarkUsed で消費する (#600 item 5)。
		var ticketID *string
		if ticket != nil {
			ticketID = &ticket.ID
			if resp := h.claimInvitation(c, ticket, true); resp != nil {
				return resp()
			}
		}
		pending, perr := h.signupService.CreatePending(req.Username, req.EmailAddress, req.Password, ticketID)
		if perr != nil {
			h.releaseInvitation(ticket)
			// upstream は username 系 error を Fastify-style reply error
			// で投げる (SignupApiService.ts:174-184)。mk-go も同 shape に揃える
			// (#802)。INVALID_PARAM は upstream 上流に対応 code が無いが
			// (= signupService 内部の generic catch で error.toString() を
			// message に詰める形) drop-in でも shape を揃える方が consumer 側
			// で 1 経路に統一できるので Fastify-style にする。
			if errors.Is(perr, coresignup.ErrUsernameAlreadyExists) {
				return duplicatedUsernameError(c)
			}
			if errors.Is(perr, coresignup.ErrInvalidUsername) {
				// upstream SignupService は INVALID_USERNAME を投げる (#2081)。
				return apierr.FastifyReply(c, http.StatusBadRequest, "INVALID_USERNAME")
			}
			if errors.Is(perr, coresignup.ErrUsernameUsed) {
				return apierr.FastifyReply(c, http.StatusBadRequest, "USED_USERNAME")
			}
			if errors.Is(perr, coresignup.ErrUsernameReserved) {
				// upstream SignupApiService の email-required path は preserved を
				// DENIED_USERNAME で返す (非 email path の USED_USERNAME とは異なる、#2080)。
				return apierr.FastifyReply(c, http.StatusBadRequest, "DENIED_USERNAME")
			}
			if errors.Is(perr, coresignup.ErrUsernameTooShort) {
				// **`USED_USERNAME` / `INVALID_USERNAME` に混ぜない (#3015)。**
				// 前者は「他人が使っている」、後者は「文字種か長さの上限が不正」で、
				// どちらも利用者の直し方が違う。最小文字数は運営者の設定なので、
				// 何文字必要かが分からないと直しようがない。additive な error code
				// なので既存クライアントの互換は壊れない (未知の code は汎用の
				// エラー表示に落ちる)。
				return apierr.FastifyReply(c, http.StatusBadRequest, "USERNAME_TOO_SHORT")
			}
			if errors.Is(perr, coresignup.ErrPasswordTooLong) {
				return apierr.FastifyReply(c, http.StatusBadRequest, "PASSWORD_TOO_LONG")
			}
			return apierr.FastifyReply(c, http.StatusInternalServerError, "INTERNAL_ERROR")
		}
		// 招待制併用時は ticket に usedAt + pendingId をセットし、確認メール送信から
		// 30 分間の再送を validateInvitationCode で弾く (upstream SignupApiService の
		// update({usedAt, pendingUserId})、#2083)。確認完了時に PromotePending が
		// MarkUsed で usedById を立てる。
		if ticket != nil && h.ticketStore != nil {
			if merr := h.ticketStore.MarkPending(ticket.ID, pending.ID); merr != nil {
				// best-effort (pending は作成済) だが、失敗すると再送防止窓が効かないので
				// observability のため warn を残す (#2083)。
				slog.Warn("signup: failed to mark invitation ticket pending", "ticketId", ticket.ID, "err", merr)
			}
		}
		h.sendSignupConfirmation(meta, req.EmailAddress, pending.Code, c.Request().Header.Get("Accept-Language"))
		// TS 互換: 本体は何も返さない (frontend は確認メールを待つ)。
		return c.NoContent(http.StatusNoContent)
	}

	// 本家 SignupService は経路を問わず rootUserId 未設定なら、作成した
	// アカウントを root にする (SignupService.ts:160)。mk-go は本番でこれを
	// 認めない。本家は /api/signup 側で setupPassword を検証しないため、
	// disableRegistration を開けた瞬間に誰でも root を取れてしまうからで、
	// mk-go では setupPassword で保護された admin/accounts/create のみを
	// root 生成経路とする (docs/divergence.md)。
	//
	// ただし TestMode では本家に揃える。本家の e2e は
	// `root = await signup({ username: 'root' })` で管理者を作る前提で書かれて
	// おり、揃えないと admin 系がまとめて 403 になる。TestMode は本番で必ず
	// false かつ起動時に警告が出る。
	isInitialSetup := h.testMode && meta.RootUserID == nil
	// host は TestMode 限定。本番で受け付けると、ローカルユーザーを任意の
	// リモートホスト所属に偽装できてしまう。
	var remoteHost *string
	if h.testMode && strings.TrimSpace(req.Host) != "" {
		hv := strings.ToLower(strings.TrimSpace(req.Host))
		remoteHost = &hv
	}
	if ticket != nil {
		if resp := h.claimInvitation(c, ticket, false); resp != nil {
			return resp()
		}
	}
	result, err := h.signupService.SignupWithHost(req.Username, req.Password, isInitialSetup, remoteHost, coresignup.UsernamePolicyPublic)
	if err != nil {
		h.releaseInvitation(ticket)
		// upstream の `/api/signup` は username 系 error を Fastify-style
		// reply error で投げる (SignupApiService.ts)。shape を揃える (#802)。
		if errors.Is(err, coresignup.ErrUsernameAlreadyExists) {
			return duplicatedUsernameError(c)
		}
		if errors.Is(err, coresignup.ErrInvalidUsername) {
			// upstream SignupService は INVALID_USERNAME を投げる (#2081)。
			return apierr.FastifyReply(c, http.StatusBadRequest, "INVALID_USERNAME")
		}
		if errors.Is(err, coresignup.ErrUsernameUsed) {
			return apierr.FastifyReply(c, http.StatusBadRequest, "USED_USERNAME")
		}
		if errors.Is(err, coresignup.ErrUsernameReserved) {
			// 非 email path は upstream SignupService と同じく preserved も USED_USERNAME (#2080)。
			return apierr.FastifyReply(c, http.StatusBadRequest, "USED_USERNAME")
		}
		if errors.Is(err, coresignup.ErrUsernameTooShort) {
			// 最小文字数 (#3015)。理由は email path 側のコメントを参照。
			return apierr.FastifyReply(c, http.StatusBadRequest, "USERNAME_TOO_SHORT")
		}
		if errors.Is(err, coresignup.ErrPasswordTooLong) {
			return apierr.FastifyReply(c, http.StatusBadRequest, "PASSWORD_TOO_LONG")
		}
		return apierr.FastifyReply(c, http.StatusInternalServerError, "INTERNAL_ERROR")
	}

	// invitation code使用済みにする。確保 (usedAt) は済んでいるので、ここで失敗しても
	// 同じコードは再利用できない。アカウントは作成済みなので戻さずに記録だけ残す。
	if ticket != nil && h.ticketStore != nil {
		if err := h.ticketStore.MarkUsed(ticket.ID, result.User.ID); err != nil {
			slog.Warn("signup: failed to bind invitation ticket to the account", "ticketId", ticket.ID, "err", err)
		}
	}

	// upstream SignupApiService が signinService.signin を呼ぶのは signup-pending
	// (メール確認) 経路だけで、通常の signup は pack した MeDetailed に token を
	// 足して返すだけ。ここで signin を通すと login 通知が 1 件生まれ、作りたての
	// アカウントの hasUnreadNotification / unreadNotificationsCount が upstream と
	// 食い違う (#1804 の適用範囲を signup-pending に限定)。
	return c.JSON(http.StatusOK, packSignupResponse(result.User, result.Profile, result.Token, h.idGen, h.signupPolicies(result.User.ID)))
}

// SignupPending handles POST /api/signup-pending. Misskey TS 互換: code を
// 受け取り、対応する user_pending を本登録に昇格して { id, i: token } を返す。
//
// upstream `SignupApiService.signupPending` は handler 全体を try-catch で
// 囲み catch 節で `throw new FastifyReplyError(400, ...)` を投げるため、
// 既知 error も unknown error も status 400 + Fastify shape で返る。
// mk-go も #809 で status/shape を upstream に揃える (旧 mk-go は
// NO_SUCH_CODE=404 / EXPIRED=410 / INVITATION_ALREADY_USED=409 /
// INVITATION_REVOKED=410 と独自 status を返していて drop-in 互換が崩れて
// いた)。INTERNAL_ERROR (default catch) のみ server-side error として 500
// を維持する (upstream の 400 を再現すると true internal failure が 400 に
// 丸まり sanity check 上望ましくないため意図的 drift)。
func (h *Handler) SignupPending(c echo.Context) error {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.Bind(&req); err != nil || req.Code == "" {
		return apierr.FastifyReply(c, http.StatusBadRequest, "INVALID_PARAM")
	}
	result, err := h.signupService.PromotePending(req.Code)
	if err != nil {
		switch err {
		case coresignup.ErrRegistrationClosed:
			return registrationClosedError(c)
		case coresignup.ErrPendingNotFound:
			return apierr.FastifyReply(c, http.StatusBadRequest, "NO_SUCH_CODE")
		case coresignup.ErrPendingExpired:
			return apierr.FastifyReply(c, http.StatusBadRequest, "EXPIRED")
		case coresignup.ErrUsernameAlreadyExists:
			return duplicatedUsernameError(c)
		case coresignup.ErrInvitationAlreadyUsed:
			return apierr.FastifyReply(c, http.StatusBadRequest, "INVITATION_ALREADY_USED")
		case coresignup.ErrApplicationNotApproved:
			// 承認が既に使われている / 期限切れ / 不在 (#2576)、または承認制が
			// 有効なのに申請に紐付いていない (#2804)。**どちらもアカウントは
			// 作られていない** — 前者は同じ tx で巻き戻り、後者は tx に入る前に
			// 返る。
			return apierr.FastifyReply(c, http.StatusBadRequest, "NOT_APPROVED")
		case coresignup.ErrInvitationRevoked:
			// admin が ticket を削除した状態 (#610 item 2)。AlreadyUsed と区別。
			return apierr.FastifyReply(c, http.StatusBadRequest, "INVITATION_REVOKED")
		default:
			return apierr.FastifyReply(c, http.StatusInternalServerError, "INTERNAL_ERROR")
		}
	}
	// 招待コード消費 — Service の tx 経路では tx 内で MarkUsed 済 (#604)。
	// 非 tx 経路 (mock テスト等) のみ handler 側で best-effort consume。
	// Service が consume したかは SignupResult.InvitationTicketConsumed で判別。
	if result.InvitationTicketID != nil && !result.InvitationTicketConsumed && h.ticketStore != nil {
		_ = h.ticketStore.MarkUsed(*result.InvitationTicketID, result.User.ID)
	}
	// 承認制 + メール必須の登録はここで初めてアカウントになる (#2571)。
	// **申請を completed にしないと approved のまま残り、同じクレームコードで
	// 何度でも登録を始められる。**
	// tx 経路では Service が同じ tx 内で確定済み (#2576)。ここで重ねて呼ぶと
	// ErrNotApproved が warn として出るだけなので、済んでいるなら触らない。
	if result.SignupApplicationID != nil && !result.SignupApplicationCompleted && h.applications != nil {
		ticketID := ""
		if result.InvitationTicketID != nil {
			ticketID = *result.InvitationTicketID
		}
		if cerr := h.applications.MarkCompleted(*result.SignupApplicationID, result.User.ID, ticketID); cerr != nil {
			// アカウントは成立しているので 500 にはしない。申請の完了記録は監査用。
			slog.Warn("signup: mark application completed failed",
				"applicationId", *result.SignupApplicationID, "userId", result.User.ID, "err", cerr)
		}
	}
	// signup-pending 完了も signin 経路を通す (履歴 / login 通知 / main publish、#1804)。
	h.fireSigninSideEffects(c, result.User.ID)
	return c.JSON(http.StatusOK, map[string]any{
		"id": result.User.ID,
		"i":  result.Token,
	})
}

// sendSignupConfirmation mails the confirmation link for a pending signup.
//
// 承認制の登録 (#2571) も同じメールに合流させるため helper 化している。
// **2 箇所に同じ文面を書かない** — 片方だけ直すと、どちらの経路で登録したかで
// 届く内容が変わる。
func (h *Handler) sendSignupConfirmation(meta *model.Meta, to, code, acceptLanguage string) {
	if h.emailSender == nil {
		return
	}
	siteName := "Misskey"
	if meta != nil && meta.Name != nil && *meta.Name != "" {
		siteName = *meta.Name
	}
	lang := l10n.ResolveFromHeader(acceptLanguage, l10n.LangsFromMeta(meta))
	subject, lead, linkLabel := l10n.SignupConfirm(lang, siteName)
	confirmURL := h.signupConfirmURL(code)
	text, bodyHTML := coreemail.LinkText(lead, linkLabel, confirmURL)
	html := coreemail.WrapHTML(coreemail.HTMLWrapInput{
		SiteName: siteName,
		SiteURL:  h.serverURL,
		Subject:  subject,
		BodyHTML: bodyHTML,
	})
	go h.emailSender(to, miscsmtp.Message{
		Subject: subject,
		Text:    text,
		HTML:    html,
	})
}

// signupConfirmURL builds the email confirmation link Misskey TS uses
// (`/signup-complete/<code>`)。frontend がこの path を hand-off する想定。
func (h *Handler) signupConfirmURL(code string) string {
	base := h.serverURL
	if base == "" {
		base = "https://localhost"
	}
	return base + "/signup-complete/" + code
}

// validateEmailWithMeta runs the same validators i/UpdateEmail uses, so
// signup と email 変更で挙動が乖離しないようにする (banned domains / format /
// active check 等)。client は verifymail / truemail への outbound に使う
// SSRF-safe HTTP client (#638)。nil なら http.DefaultClient フォールバック。
func validateEmailWithMeta(ctx context.Context, meta *model.Meta, addr string, client *http.Client) error {
	svc := coreemail.NewServiceWithClient(meta, client)
	return svc.Validate(ctx, addr)
}

// claimInvitation reserves the invitation ticket right before the account
// (or the pending signup) is created. It returns a response to send when
// the ticket could not be reserved, or nil to continue.
//
// **validateInvitationCode は読むだけなので、それだけでは 1 回きりにならない。**
// 同じコードで並行に来たリクエストは全部検証を通るので、作成の前にここで
// 条件付き UPDATE により 1 件だけを通す (upstream 2026.9.1 SignupApiService の
// claimRegistrationTicket)。取れなかった側には検証失敗と同じエラーを返す。
func (h *Handler) claimInvitation(c echo.Context, ticket *model.RegistrationTicket, emailRequired bool) func() error {
	if ticket == nil || h.ticketStore == nil {
		return nil
	}
	claimed, err := h.ticketStore.ClaimForSignup(ticket.ID, emailRequired)
	if err != nil {
		slog.Error("signup: cannot reserve the invitation ticket", "ticketId", ticket.ID, "err", err)
		return func() error { return apierr.JSONInternalError(c) }
	}
	if !claimed {
		return func() error {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVITATION_CODE_INVALID", "Invalid invitation code.", "11e71a03-43c4-4a99-92cf-bb7e2c581998"))
		}
	}
	return nil
}

// releaseInvitation undoes claimInvitation when the signup failed, so the
// code is not wasted. A ticket already bound to an account is left alone.
func (h *Handler) releaseInvitation(ticket *model.RegistrationTicket) {
	if ticket == nil || h.ticketStore == nil {
		return
	}
	if err := h.ticketStore.ReleaseClaim(ticket.ID); err != nil {
		slog.Warn("signup: failed to release the invitation ticket", "ticketId", ticket.ID, "err", err)
	}
}

// validateInvitationCode checks the ticket store for a valid invitation code.
// emailRequired は emailRequiredForSignup。upstream SignupApiService の usedAt
// gate (#2083) を再現する: email 確認制では確認メール送信から 30 分以内の ticket を
// 弾き (再送防止)、非 email 制では usedAt が立っている ticket を弾く。
func (h *Handler) validateInvitationCode(code string, emailRequired bool) (*model.RegistrationTicket, error) {
	if code == "" || h.ticketStore == nil {
		return nil, errInvalidCode
	}
	ticket, err := h.ticketStore.FindByCode(code)
	if err != nil {
		return nil, errInvalidCode
	}
	if ticket.UsedByID != nil {
		return nil, errInvalidCode
	}
	if ticket.ExpiresAt != nil && ticket.ExpiresAt.Before(time.Now()) {
		return nil, errInvalidCode
	}
	if emailRequired {
		// 確認メール送信 (= MarkPending で usedAt セット) から 30 分以内は再送不可。
		if ticket.UsedAt != nil && ticket.UsedAt.Add(30*time.Minute).After(time.Now()) {
			return nil, errInvalidCode
		}
	} else if ticket.UsedAt != nil {
		// 非 email 制では usedAt が立っていれば使用済 (upstream の else if 分岐)。
		return nil, errInvalidCode
	}
	return ticket, nil
}

// UserPolicyResolver resolves a user's effective role policies. Implemented by
// role.Service.
type UserPolicyResolver interface {
	GetUserPolicies(userID string) map[string]any
}

// SetUserPolicyResolver wires the effective-policy source for the signup
// response.
func (h *Handler) SetUserPolicyResolver(r UserPolicyResolver) { h.userPolicies = r }

// signupPolicies returns the policies advertised in the signup response.
//
// **実効値を返すこと。** upstream の SignupApiService は MeDetailed を pack し、
// UserEntityService が `policies: roleService.getUserPolicies(user.id)` を埋める。
// ここで素の default を返すと、meta の base policy override も server cap も
// 反映されず、作成直後の利用者に対して signup と /api/i が違う値を答える (#2673)。
func (h *Handler) signupPolicies(userID string) map[string]any {
	if h.userPolicies == nil {
		return role.DefaultPolicies()
	}
	return h.userPolicies.GetUserPolicies(userID)
}

// packSignupResponse builds a MeDetailed + token response for a newly created user.
//
// upstream SignupApiService は `pack(account, account, {schema: 'MeDetailed',
// includeSecrets: true})` の結果に token を足して返す。つまり /api/i と同じ形。
// mk-go も同じ packer を通し、MeDetailed struct に無い /api/i 固有 field
// (pinnedPage / clientData / room / securityKeysList 等) だけを新規ユーザーの
// 既定値で補う。個別に map を組み立てていた頃は 24 個の field が欠けていた。
func packSignupResponse(u *model.User, profile *model.UserProfile, token string, idGen id.Generator, policies map[string]any) map[string]any {
	me := entity.PackMeDetailed(u, profile, idGen)
	me.Policies = policies
	b, _ := json.Marshal(me)
	resp := map[string]any{}
	_ = json.Unmarshal(b, &resp)

	// includeSecrets 相当。signup は native session なので secret を出す。
	resp["email"] = nil
	resp["emailVerified"] = false
	if profile != nil {
		resp["email"] = profile.Email
		resp["emailVerified"] = profile.EmailVerified
	}
	resp["securityKeysList"] = []any{}
	// MeDetailed struct に無い /api/i 固有 field。新規ユーザーは常に未設定。
	resp["pinnedPageId"] = nil
	resp["pinnedPage"] = nil

	resp["token"] = token
	return resp
}

// defaultPolicies returns the Misskey default policies for a new user.
var errInvalidCode = errors.New("invalid invitation code")

// HasUserPolicyResolver reports whether the effective-policy resolver was wired.
//
// 未配線だと素の default policy を返すので、meta の base policy override も
// server cap も反映されず、signup と /api/i が違う値を答える (#2673)。
// 起動時検査に使う (#2683)。
func (h *Handler) HasUserPolicyResolver() bool { return h.userPolicies != nil }
