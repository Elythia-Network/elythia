package signup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/keyword"
	passwordpkg "github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// localUsernamePattern は upstream Misskey TS の `localUsernameSchema`
// (= 本家の packages/backend/src/models/User.ts:319) に整合する username 検証 regex。
// `^\w{1,20}$` 相当で `\w` = [a-zA-Z0-9_]、length 1-20。
//
// Go の regexp は RE2 default で `\w` が ASCII の [0-9A-Za-z_] に限定されるため
// upstream JS の `\w` と挙動が一致する。意図を明示するため明示的に文字クラスを
// 書き下している (Unicode property を含めない契約をコード上に固定する目的)。
//
// 旧 mk-go は length 128 文字まで許容していて drop-in で TS instance に
// 引き継いだ後 frontend (= TS の username schema) で reject される
// regression があった (#800)。
var localUsernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{1,20}$`)

// UsernamePolicy selects which meta-driven username restrictions apply (#3015).
//
// **引数で渡すのが要点。** `admin/accounts/create` は `Signup` を呼ぶので、
// `preservedUsernames` を見ている位置からは通常の公開登録と区別が付かない
// (どちらも `isInitialSetup == false`)。位置だけで除外しようとすると admin まで
// 巻き込むので、呼び出し側に選ばせる。**シグネチャを変えることで、
// コンパイラに全呼び出し元の書き換えを強制する** (#3025 で `NormalizeCursor` を
// 3 値にしたのと同じ考え方)。
//
// **`bool` にしない。** 隣に `isInitialSetup bool` があるので、位置を取り違えても
// コンパイルが通ってしまう。
type UsernamePolicy int

const (
	// UsernamePolicyPublic applies every meta-driven restriction. 公開登録と
	// 申請経由の登録はこちら。
	UsernamePolicyPublic UsernamePolicy = iota
	// UsernamePolicyOperator skips the minimum length only.
	//
	// **`preservedUsernames` は引き続き適用される。** 運営が公式アカウントに
	// 短い ID を配れるようにするのが目的で、予約済みの名前まで通したいわけでは
	// ないため。結果として admin 経路は「予約は効くが最小長は効かない」という
	// 非対称になる。
	UsernamePolicyOperator
)

// minimumUsernameLength returns the effective floor, clamped to the range the
// column is validated against.
//
// **0 や負値は 1 に倒す。** 列は 1-20 で検証するが、TS 側が書いた行や手で
// UPDATE した行が範囲外を持ちうる。**上限も切る** — 20 を超えると
// `localUsernamePattern` が先に弾くので、どの username も通らない
// インスタンスになる (設定ミスで登録が全滅する形は避ける)。
func minimumUsernameLength(meta *model.Meta) int {
	if meta == nil || meta.MinimumUsernameLength < 1 {
		return 1
	}
	if meta.MinimumUsernameLength > MaxUsernameLength {
		return MaxUsernameLength
	}
	return meta.MinimumUsernameLength
}

const (
	// MinUsernameLength / MaxUsernameLength は `localUsernamePattern` に
	// 焼かれている境界。`admin/update-meta` の範囲検証が参照する (#3015)。
	//
	// **パターンと二重管理になる。** 変えるときは両方直すこと —
	// TestUsernameLengthBoundsMatchPattern が食い違いを落とす。
	MinUsernameLength = 1
	MaxUsernameLength = 20
)

// violatesMinimumUsernameLength reports whether username is shorter than the
// configured floor. policy が Operator なら常に false。
//
// **長さは byte で数える。** 呼び出し元はいずれも先に
// `normalizeAndValidateUsername` か `ValidUsernameFormat` を通しており、
// `localUsernamePattern` が `[a-zA-Z0-9_]` に限定するので byte 長と文字数は
// 一致する。format 検証を通っていない値を渡すと、multibyte が 1 文字で
// 3 byte に数えられて**制限を素通りする**ので、その順序を崩さないこと。
func violatesMinimumUsernameLength(username string, meta *model.Meta, policy UsernamePolicy) bool {
	if policy == UsernamePolicyOperator {
		return false
	}
	return len(username) < minimumUsernameLength(meta)
}

// normalizeAndValidateUsername trims surrounding whitespace and validates
// the username against upstream Misskey TS の `localUsernameSchema`. 返り値
// の username は trim 済みなので、caller はそのまま小文字化や DB 永続化に
// 利用してよい。validation 失敗時は ErrInvalidUsername を返す。
func normalizeAndValidateUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if !localUsernamePattern.MatchString(username) {
		return "", ErrInvalidUsername
	}
	return username, nil
}

// ValidUsernameFormat reports whether username matches the local username
// schema (`^[a-zA-Z0-9_]{1,20}$`). Exposed so the username/available endpoint
// shares the exact same format validation as signup (upstream localUsernameSchema、
// #1551)。trim はしない (caller が必要なら行う)。
func ValidUsernameFormat(username string) bool {
	return localUsernamePattern.MatchString(username)
}

var (
	// ErrUsernameAlreadyExists is returned when the username is taken.
	ErrUsernameAlreadyExists = errors.New("username already exists")
	// ErrInvalidUsername is returned when the username is invalid.
	ErrInvalidUsername = errors.New("invalid username")
	// ErrUsernameReserved is returned when the username matches an entry in
	// meta.preservedUsernames (case-insensitive). 初回セットアップ時は root
	// ユーザー作成を妨げないため、このチェックはスキップする。
	ErrUsernameReserved = errors.New("username is reserved")
	// ErrUsernameTooShort is returned when the username is shorter than
	// meta.minimumUsernameLength (#3015).
	//
	// **`ErrInvalidUsername` と分ける。** あちらは format (文字種・20 文字の
	// 上限) の違反で、どのインスタンスでも同じ判定になる。こちらは運営者が
	// 設定した値に依存するので、利用者に返す文面も「使えない文字が入って
	// います」ではなく「n 文字以上にしてください」でなければ直しようがない。
	ErrUsernameTooShort = errors.New("username is shorter than the minimum length")
	// ErrUsernameUsed is returned when the username matches a deleted account's
	// username recorded in used_usernames (再利用防止)。upstream SignupService /
	// SignupApiService の usedUsernamesRepository.exists 相当 (#2080)。
	ErrUsernameUsed = errors.New("username was used by a deleted account")
	// ErrPendingNotFound is returned when no user_pending row matches the code.
	ErrPendingNotFound = errors.New("pending signup not found")
	// ErrPendingExpired is returned when the pending signup is past its TTL.
	// TTL は ID (ULID) 由来 timestamp から算出する (createdAt カラム不在のため)。
	ErrPendingExpired = errors.New("pending signup expired")
	// ErrApplicationNotApproved is returned when a pending signup cannot show
	// an approval that is still usable. 生産者は 2 系統ある。
	//
	//  1. settleApplicationTx (#2576) — 紐付く申請が使用済み / 期限切れ / 不在。
	//     **アカウント作成と同じトランザクションで判定する。** 別々にすると、
	//     確認と再送が重なったときに 1 つの承認から 2 アカウント作れる。
	//  2. checkApprovalGate (#2804) — 承認制が有効なのに申請に紐付いていない。
	//     こちらは tx に入る前に返る (tx で巻き戻すものが無い)。
	//
	// **どちらもアカウントは作られていない**が、その理由は同じではない。
	// 「この error なら tx で巻き戻っている」という形で依存しないこと。
	ErrApplicationNotApproved = errors.New("signup application is not approved")
	// ErrRegistrationClosed is returned while the server accepts no
	// registrations at all (meta.registrationClosed, #3186).
	ErrRegistrationClosed = errors.New("registration is closed")
	// ErrInvitationAlreadyUsed is returned when the invitation ticket linked to
	// a pending signup has already been consumed by another user. transaction
	// 経路で SELECT FOR UPDATE 後に usedById を確認することで concurrent burst
	// の race を完全に閉じる (#604)。
	ErrInvitationAlreadyUsed = errors.New("invitation already used")
	// ErrInvitationRevoked is returned when the invitation ticket linked to a
	// pending signup has been deleted (typically by an admin) before the user
	// completed the email verification. AlreadyUsed と区別することで frontend
	// が「使用済」「失効」をローカライズで分けられる (#610 item 2)。
	ErrInvitationRevoked = errors.New("invitation revoked")
	// ErrPasswordTooLong is returned when the supplied password exceeds the
	// 72-byte limit of bcrypt. Go の bcrypt は 73 byte 以上で
	// ErrPasswordTooLong を返すが Node の bcrypt は silent truncation する。
	// mk-go 側で 400 に変換しておかないと、空 hash が DB に書かれて user が
	// 永久 login 不能になる (#1075)。
	ErrPasswordTooLong = errors.New("password too long")
)

// PendingSignupTTL is the lifetime of a pending signup row.
//
// **upstream と同じ 30 分 (#3037)。** `SignupApiService.ts:254` が
// `idService.parse(pendingUser.id).date + 30 分` を過ぎた pending を弾く。
// 以前は 24h で、しかも doc コメントが「Misskey TS 実装では明示的な TTL は
// 無い」と**事実と逆のことを書いていた**ため、48 倍に広げている自覚が
// どこにも残っていなかった。
//
// 窓が長いほど、確認メールを盗まれたときに使われる余地と、承認制へ切り替える
// 直前に発行された確認メールが「承認を経ないアカウント」になる余地
// (#2804 のゲートが守っている範囲) が広がる。
//
// ID (ULID) の timestamp と比較して PromotePending 時に判定する。
const PendingSignupTTL = 30 * time.Minute

// WebhookHook is invoked after a new local user has been created so that
// system webhooks subscribed to `userCreated` can fire. 循環依存を避けるため
// interface で受け取る (実装は core/webhook)。
type WebhookHook interface {
	OnUserCreated(user *model.User)
}

// Service handles user registration.
type Service struct {
	userRepo         repository.UserRepository
	metaRepo         repository.MetaRepository
	keypairRepo      repository.UserKeypairRepository
	keypairExtraRepo repository.UserKeypairExtraRepository
	pendingRepo      repository.UserPendingRepository
	ticketRepo       repository.RegistrationTicketRepository
	appRepo          repository.SignupApplicationRepository
	// usedUsernameRepo は削除済 account の username 再利用を弾く (#2080)。
	// optional (nil なら used_usernames チェックを skip、後方互換)。
	usedUsernameRepo repository.UsedUsernameRepository
	// db は PromotePending を transaction 化して partial failure rollback と
	// invitation ticket の SELECT FOR UPDATE ロックを実現する用途。未設定 (nil)
	// なら従来の repo-based 非 tx パスにフォールバックする (mock テスト用)。
	db          *gorm.DB
	webhookHook WebhookHook
	idGen       id.Generator
}

// NewService creates a new SignupService.
func NewService(userRepo repository.UserRepository, metaRepo repository.MetaRepository, idGen id.Generator) *Service {
	return &Service{userRepo: userRepo, metaRepo: metaRepo, idGen: idGen}
}

// SetDB wires the GORM database handle so PromotePending can run inside a
// transaction. production の router 配線時に呼ぶ。設定しない場合は repo-based
// 非 tx パスで動作する (mock テスト用)。
func (s *Service) SetDB(db *gorm.DB) {
	s.db = db
}

// SetUsedUsernameRepo wires the UsedUsernameRepository so Signup / CreatePending
// reject usernames of deleted accounts (#2080)。未設定なら used_usernames チェックを
// skip する (後方互換)。
func (s *Service) SetUsedUsernameRepo(r repository.UsedUsernameRepository) {
	s.usedUsernameRepo = r
}

// recordUsedUsername best-effort records lower in used_username so the username
// cannot be reclaimed after account deletion (#2106 N23). Failures are logged but
// never fail the signup; the non-transactional callers have already created the
// user/profile, so aborting here would leave an inconsistent state.
func (s *Service) recordUsedUsername(lower string) {
	if s.usedUsernameRepo == nil {
		return
	}
	if err := s.usedUsernameRepo.Create(lower); err != nil {
		slog.Warn("failed to record used username", "username", lower, "error", err)
	}
}

// isUsedUsername reports whether lower (already lowercased) is recorded in
// used_usernames (削除済 account)。repo 未配線 / 照合失敗時は false (fail-open、
// オンライン性優先で既存 preserved/duplicate ガードに委ねる)。
func (s *Service) isUsedUsername(lower string) bool {
	if s.usedUsernameRepo == nil {
		return false
	}
	used, err := s.usedUsernameRepo.Exists(lower)
	return err == nil && used
}

// SetTicketRepo wires the RegistrationTicketRepository so PromotePending tx
// path で invitation ticket を SELECT FOR UPDATE ロックして MarkUsed まで
// 同一 transaction で完結できる。emailRequiredForSignup + DisableRegistration
// 併用時の race fix (#604) に必須。
func (s *Service) SetTicketRepo(r repository.RegistrationTicketRepository) {
	s.ticketRepo = r
}

// SetSignupApplicationRepo wires the approval application repository so
// PromotePending can settle the application inside the same transaction
// (#2576)。未配線なら handler 側の best-effort な MarkCompleted に委ねる。
func (s *Service) SetSignupApplicationRepo(r repository.SignupApplicationRepository) {
	s.appRepo = r
}

// SetUserPendingRepo wires the user_pending repository so CreatePending /
// PromotePending become available. emailRequiredForSignup フローを使う場合は
// 必須。未設定でも通常の Signup は動く。
func (s *Service) SetUserPendingRepo(r repository.UserPendingRepository) {
	s.pendingRepo = r
}

// SetKeypairRepo wires the user keypair repository. When set, Signup will
// generate a fresh RSA keypair for each newly created local user, which is
// required for ActivityPub federation (actor endpoints return publicKey).
func (s *Service) SetKeypairRepo(r repository.UserKeypairRepository) {
	s.keypairRepo = r
}

// SetKeypairExtraRepo wires the user keypair extra repository. When set,
// Signup will also generate an Ed25519 keypair alongside the RSA one and
// store it in user_keypair_extra so the actor JSON can publish it via
// assertionMethod[] (FEP-521a Multikey). Optional: 未配線でも RSA only で動く
// (= upstream Misskey TS と完全に同じ挙動)。
func (s *Service) SetKeypairExtraRepo(r repository.UserKeypairExtraRepository) {
	s.keypairExtraRepo = r
}

// SetWebhookHook attaches a WebhookHook invoked after user creation so that
// system webhooks subscribed to `userCreated` can fire.
func (s *Service) SetWebhookHook(h WebhookHook) {
	s.webhookHook = h
}

// txInserter abstracts `tx.Create(value).Error` so the tx-create error wrap
// path of maybeCreateEd25519Keypair can be unit-tested without spinning up a
// real PostgreSQL transaction. `*gorm.DB` satisfies this interface natively.
//
// PromotePending tx 経路の tx error path は idGen が毎回新規 ULID を返す関係で
// integration test での FK / PK violation 再現が困難なため、最小の interface
// 抽出だけで unit test 可能な形に切り出している (#1075 follow-up)。
type txInserter interface {
	Create(value any) *gorm.DB
}

// maybeCreateEd25519Keypair generates and stores an Ed25519 keypair for the
// given user when keypairExtraRepo is wired. tx が non-nil なら同 tx 内で
// INSERT し、nil なら repository の Upsert を使う。keypairExtraRepo 未配線
// (= mock テスト or 未対応 deployment) の場合は何もせず nil を返す。
//
// systemaccount.completeFromOrphan と error wrap 形式を揃え、log で発生個所が
// 追えるようにする。
//
// IMPORTANT: tx には untyped nil (= `nil` literal) または有効な `*gorm.DB` を
// 渡すこと。typed nil の `*gorm.DB` を渡すと interface comparison で not-nil
// 扱いになり (`var tx *gorm.DB = nil; var i txInserter = tx; i != nil` が true)、
// `tx.Create(...)` で nil deref panic になる。現 callsite (Signup /
// promotePendingNoTx は literal nil、promotePendingTx は Begin 直後の valid tx)
// はこれを満たすので問題ないが、新規 callsite を追加する際の罠。
func (s *Service) maybeCreateEd25519Keypair(tx txInserter, userID string) error {
	if s.keypairExtraRepo == nil {
		return nil
	}
	privPEM, pubPEM, err := activitypub.GenerateEd25519Keypair()
	if err != nil {
		return fmt.Errorf("signup: generate ed25519 keypair: %w", err)
	}
	row := &model.UserKeypairExtra{
		UserID:            userID,
		Ed25519PublicKey:  pubPEM,
		Ed25519PrivateKey: privPEM,
	}
	if tx != nil {
		if err := tx.Create(row).Error; err != nil {
			return fmt.Errorf("signup: create ed25519 keypair (tx): %w", err)
		}
		return nil
	}
	if err := s.keypairExtraRepo.Upsert(row); err != nil {
		return fmt.Errorf("signup: create ed25519 keypair: %w", err)
	}
	return nil
}

// SignupResult holds the created user and their native token.
type SignupResult struct {
	User  *model.User
	Token string
	// InvitationTicketID は PromotePending 経路で pending row から復元した
	// 招待コード ticket ID。通常 Signup や非招待 PromotePending では nil。
	InvitationTicketID *string
	// InvitationTicketConsumed は Service が tx 内で MarkUsed 済かどうか。
	// 非 tx 経路 (mock) で false なら handler 側で best-effort consume する。
	InvitationTicketConsumed bool
	// Profile は作成した user_profile。/api/signup のレスポンスは upstream と
	// 同じく MeDetailed そのものなので、packer に渡す profile が要る。
	Profile *model.UserProfile
	// SignupApplicationID は承認制 (#2571) の確認メール経路で pending row から
	// 復元した申請 ID。**返さないと申請が approved のまま残り、1 つの承認から
	// 複数アカウントを作れる。**
	SignupApplicationID *string
	// SignupApplicationCompleted は Service が tx 内で申請を completed にしたか
	// (#2576)。false なら handler 側で best-effort に MarkCompleted する
	// (非 tx 経路 / repo 未配線)。InvitationTicketConsumed と同じ形。
	SignupApplicationCompleted bool
}

// Signup creates a new local user with the given username and password.
// isInitialSetup=true の場合、作成したユーザーを rootUser に設定する。
// policy は meta 由来の username 制限のどれを適用するかを選ぶ (#3015)。
func (s *Service) Signup(username, password string, isInitialSetup bool, policy UsernamePolicy) (*SignupResult, error) {
	return s.SignupWithHost(username, password, isInitialSetup, nil, policy)
}

// SignupWithHost creates a user attributed to the given remote host.
//
// upstream SignupApiService は NODE_ENV === 'test' のときだけ body.host を
// 受け付け、SignupService に渡してリモートユーザーを直接作れるようにしている
// (e2e で「リモートユーザーのノートが LTL に出ない」等を検証するため)。
// host=nil なら従来どおりローカルユーザー。呼び出し側で TestMode を確認する
// こと。
func (s *Service) SignupWithHost(username, password string, isInitialSetup bool, host *string, policy UsernamePolicy) (*SignupResult, error) {
	username, err := normalizeAndValidateUsername(username)
	if err != nil {
		return nil, err
	}

	// ユーザー名の重複チェック
	lower := strings.ToLower(username)
	if _, err := s.userRepo.FindByUsernameLower(lower, nil); err == nil {
		return nil, ErrUsernameAlreadyExists
	}

	// 削除済 account の username 再利用を弾く (upstream SignupService:87、existing →
	// used → preserved の順、#2080)。
	if s.isUsedUsername(lower) {
		return nil, ErrUsernameUsed
	}

	// meta.preservedUsernames チェック。初回セットアップ (root ユーザー作成) は
	// admin / root が予約ワードに含まれうるので除外する。meta fetch 失敗時は
	// ベストエフォートで通過させる (オンライン性を優先)。
	//
	// **最小文字数 (#3015) も同じブロックで見る。** 初回セットアップを除外する
	// のは、n を大きくすると最初のアカウントを作れなくなり構築が詰むため。
	// admin 経路の除外は `policy` が担う (位置では区別が付かない)。
	if !isInitialSetup {
		if meta, err := s.metaRepo.Fetch(); err == nil {
			if violatesMinimumUsernameLength(username, meta, policy) {
				return nil, ErrUsernameTooShort
			}
			if isReservedUsername(lower, meta.PreservedUsernames) {
				return nil, ErrUsernameReserved
			}
			// #2106 N20: meta.prohibitedWordsForNameOfUser に該当する username を弾く
			// (upstream SignupService:97-100、rootUserId!=null = 非初回セットアップ時)。
			// upstream は 'USED_USERNAME' を throw するので USED_USERNAME にマップされる
			// ErrUsernameUsed を返す (preserved の DENIED_USERNAME とは別コード)。
			if keyword.IsKeyWordIncluded(lower, meta.ProhibitedWordsForNameOfUser) {
				return nil, ErrUsernameUsed
			}
		}
	}

	// bcrypt は 73 byte 以上の password で ErrPasswordTooLong を返す。Node の
	// bcrypt が silent truncation するのと異なり、Go の bcrypt は error にする
	// ため、ここで拾わないと空 hash が DB に書かれて user が永久 login 不能に
	// なる (#1075)。
	hash, err := passwordpkg.Hash(password)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return nil, ErrPasswordTooLong
		}
		return nil, fmt.Errorf("signup: bcrypt hash password: %w", err)
	}

	// native token 生成 (16文字hex)
	token := generateToken()

	now := time.Now()
	userID := s.idGen.Generate(now)
	user := &model.User{
		ID:                userID,
		Username:          username,
		UsernameLower:     lower,
		Token:             &token,
		IsExplorable:      true,
		AvatarDecorations: []byte("[]"),
		// drop-in 互換 (#785): Misskey TS と同じ意味で初回 signup user を
		// isRoot=true でマークしておく。mk-go pure 経路では meta.rootUserId
		// で root を判定するので冗長だが、TS から引き継いだ DB の root と同じ
		// 形に揃えておく。
		IsRoot: isInitialSetup,
		Host:   host,
	}
	if err := s.userRepo.Create(user); err != nil {
		return nil, err
	}

	// user_profile 作成
	hashStr := hash
	profile := &model.UserProfile{
		UserID:             userID,
		Password:           &hashStr,
		AutoAcceptFollowed: true,
		PreventAiLearning:  true,
		PublicReactions:    true,
		// DB 側の default と同値。in-memory の profile をそのまま packer へ
		// 渡すので、明示しないとレスポンスだけ false になる。
		InjectFeaturedNote:       true,
		ReceiveAnnouncementEmail: true,
	}
	if err := s.userRepo.CreateProfile(profile); err != nil {
		return nil, err
	}

	// #2106 N23: upstream SignupService は全 signup で used_username に記録し、account
	// 削除後も同名 username の再取得を恒久的に防ぐ (SignupService.ts:151-154)。
	s.recordUsedUsername(lower)

	// RSA keypair (federation 用)。keypairRepo が未設定の場合はスキップする
	// (従来の単体テストのため)。
	if s.keypairRepo != nil {
		privPEM, pubPEM, err := activitypub.GenerateRSAKeypair()
		if err != nil {
			return nil, err
		}
		if err := s.keypairRepo.Create(&model.UserKeypair{
			UserID:     userID,
			PublicKey:  pubPEM,
			PrivateKey: privPEM,
		}); err != nil {
			return nil, err
		}
	}
	// Ed25519 keypair (FEP-521a Multikey 用)。keypairExtraRepo wire 時のみ。
	if err := s.maybeCreateEd25519Keypair(nil, userID); err != nil {
		return nil, err
	}

	// 初回セットアップの場合は rootUserId を設定
	if isInitialSetup {
		_ = s.metaRepo.Update(map[string]any{"rootUserId": userID})
	}

	// システムWebhook発火（`userCreated` 相当）。ベストエフォート。
	if s.webhookHook != nil {
		s.webhookHook.OnUserCreated(user)
	}

	return &SignupResult{User: user, Token: token, Profile: profile}, nil
}

// CreatePluginManaged creates a local account that the named plugin manages
// (#3468). The account has no password, is always a bot, and records
// pluginName in user.managedByPlugin.
//
// username の検査は運営者の経路 (UsernamePolicyOperator) と同じにする。
// プラグインを組み込むのは運営者なので最小文字数は見ないが、予約語
// (preservedUsernames)・禁止語・削除済みアカウントの名前は通常の登録と同じく
// 弾く。
//
// **meta を読めないときは作らない。** 通常の登録は可用性を優先して素通しするが、
// こちらはプラグインが後で作り直せるので、予約語の検査を飛ばした名前を残す
// 方を避ける。
//
// **tx の中で作る** (createUserTx)。プラグインが同じ名前で並行に呼んでも、
// usernameLower の一意制約で片方だけが残る。
func (s *Service) CreatePluginManaged(pluginName, username string) (*model.User, error) {
	if pluginName == "" {
		return nil, errors.New("signup: plugin name is required")
	}
	if s.db == nil {
		return nil, errors.New("signup: plugin-managed accounts need the database handle (SetDB)")
	}
	username, err := normalizeAndValidateUsername(username)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(username)
	if s.isUsedUsername(lower) {
		return nil, ErrUsernameUsed
	}
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return nil, fmt.Errorf("signup: fetch meta: %w", err)
	}
	if isReservedUsername(lower, meta.PreservedUsernames) {
		return nil, ErrUsernameReserved
	}
	if keyword.IsKeyWordIncluded(lower, meta.ProhibitedWordsForNameOfUser) {
		return nil, ErrUsernameUsed
	}

	name := pluginName
	var user *model.User
	err = s.db.Transaction(func(tx *gorm.DB) error {
		u, _, err := s.createUserTx(tx, createUserInput{
			userID:          s.idGen.Generate(time.Now()),
			username:        username,
			lower:           lower,
			token:           generateToken(),
			managedByPlugin: &name,
		})
		if err != nil {
			return err
		}
		user = u
		return nil
	})
	if err != nil {
		// 並行に同じ名前で作られたときは、一意制約の違反が返る。重複として扱う。
		if errors.Is(err, ErrUsernameAlreadyExists) || repository.IsUniqueViolation(err) {
			return nil, ErrUsernameAlreadyExists
		}
		return nil, err
	}
	if s.webhookHook != nil {
		s.webhookHook.OnUserCreated(user)
	}
	return user, nil
}

// generateToken creates a native session token.
//
// 生成の実体と「なぜ 16 文字なのか」は misc.NewNativeToken 側に置いてある。
func generateToken() string {
	return misc.NewNativeToken()
}

// generatePendingCode creates a 32-char (16 byte) hex code used both as the
// user_pending.code column and the email confirmation link path segment.
// signup native token と区別するため長め。
func generatePendingCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreatePending stores a pending signup row and returns it. Username重複と
// 予約名チェックは通常 Signup と揃え、password は bcrypt で hash 済を保管
// (PromotePending 時に再 hash しない)。emailRequiredForSignup=true 用。
//
// invitationTicketID が non-nil なら pending row に保存し、PromotePending
// 成功時に SignupResult.InvitationTicketID 経由で handler に伝える
// (招待制 + email 確認制併用時の MarkUsed 用)。
func (s *Service) CreatePending(username, email, password string, invitationTicketID *string) (*model.UserPending, error) {
	return s.CreatePendingForApplication(username, email, password, invitationTicketID, nil)
}

// CreatePendingForApplication is CreatePending carrying the approval
// application this signup belongs to (#2571).
//
// 承認制 + メール必須のとき、確認完了まではアカウントが無いので申請を completed
// にできない。pending row に申請 ID を持たせ、PromotePending の戻り値で返す。
func (s *Service) CreatePendingForApplication(username, email, password string, invitationTicketID, signupApplicationID *string) (*model.UserPending, error) {
	username, err := normalizeAndValidateUsername(username)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(username)

	// 確定済 user との衝突チェック (この段階で押さえておかないと、後段の
	// PromotePending 直前まで気付けず無駄なメール送信になる)。
	if _, err := s.userRepo.FindByUsernameLower(lower, nil); err == nil {
		return nil, ErrUsernameAlreadyExists
	}
	// 削除済 account の username 再利用を弾く (upstream SignupApiService:178、#2080)。
	if s.isUsedUsername(lower) {
		return nil, ErrUsernameUsed
	}
	if meta, err := s.metaRepo.Fetch(); err == nil {
		// 最小文字数 (#3015)。この 2 経路は公開登録と申請経由なので、
		// 常に制限を適用する (admin が通る経路ではない)。
		if violatesMinimumUsernameLength(username, meta, UsernamePolicyPublic) {
			return nil, ErrUsernameTooShort
		}
		if isReservedUsername(lower, meta.PreservedUsernames) {
			return nil, ErrUsernameReserved
		}
		// #2106 N20: prohibited words for username (Signup と同じ guard、email 確認経路)。
		if keyword.IsKeyWordIncluded(lower, meta.ProhibitedWordsForNameOfUser) {
			return nil, ErrUsernameUsed
		}
	}

	hash, err := passwordpkg.Hash(password)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return nil, ErrPasswordTooLong
		}
		return nil, fmt.Errorf("signup: bcrypt hash password: %w", err)
	}
	now := time.Now()
	row := &model.UserPending{
		ID:                  s.idGen.Generate(now),
		Code:                generatePendingCode(),
		Username:            username,
		Email:               email,
		Password:            hash,
		InvitationTicketID:  invitationTicketID,
		SignupApplicationID: signupApplicationID,
	}
	if err := s.pendingRepo.Create(row); err != nil {
		return nil, err
	}
	return row, nil
}

// PromotePending finalizes a pending signup: looks up by code, confirms the
// row hasn't expired, then creates the user using the stored hashed password
// (再 hash しない)。成功時に user_pending row を delete する。
//
// db が SetDB で wire されている場合は GORM transaction 内で処理し、partial
// failure を rollback で巻き戻す (#600 item 2)。さらに invitation 経由なら
// ticket を SELECT FOR UPDATE で lock して concurrent burst による「1 招待で
// 複数 user 作成」 race を完全に閉じる (#604)。MarkUsed も同 tx で完了する。
//
// db 未配線時は repo-based 非 tx パスに fallback (mock テスト互換)。
//
// 失敗パターン:
//   - ErrPendingNotFound: code が無い (**DB 障害はこれに丸めず生の error を返す**、#2799)
//   - ErrPendingExpired: ID (ULID) timestamp が PendingSignupTTL を超過
//   - ErrUsernameAlreadyExists: 確認 link 待ちの間に同名 user が登録されたケース
//   - ErrInvitationAlreadyUsed: tx 経路で ticket がすでに別 user に消費済 (#604)
//   - ErrInvitationRevoked: tx 経路で ticket が admin に削除済 (#610 item 2)
//   - ErrRegistrationClosed: 登録を受け付けていない (#3186)。コードを引く前に返す
//   - ErrApplicationNotApproved: 承認制が有効なのに申請に紐付いていない (#2804)、
//     または紐付く申請が使用済み / 期限切れ / 不在 (#2576)
func (s *Service) PromotePending(code string) (*SignupResult, error) {
	if err := s.checkRegistrationOpen(); err != nil {
		return nil, err
	}
	pending, err := s.pendingRepo.FindByCode(code)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrPendingNotFound
	}
	// ID (ULID) から作成時刻を引き、TTL を過ぎていれば拒否。row 自体は
	// 残しておく (cron での bulk cleanup を前提)。
	if t, err := s.idGen.ParseTime(pending.ID); err == nil {
		if time.Since(t) > PendingSignupTTL {
			return nil, ErrPendingExpired
		}
	}
	if err := s.checkApprovalGate(pending); err != nil {
		return nil, err
	}

	if s.db != nil && s.ticketRepo != nil {
		return s.promotePendingTx(pending)
	}
	return s.promotePendingNoTx(pending)
}

// checkRegistrationOpen rejects finalizing a pending signup while the server
// accepts no registrations (#3186).
//
// **確認待ちの登録も止める。** 「受け付けない」は荒らしの最中に緊急で閉じる用途で、
// 閉じる直前に確認メールを受け取った登録が通ると閉じた意味が無い。行は消さないので、
// `PendingSignupTTL` 内に解除されればそのまま完了できる。
//
// **コードを引く前に判定する。** 閉じている間はコードの有無も答えない。
//
// **meta が読めなければ通さない** (checkApprovalGate と同じ理由。DB 障害を迂回路に
// しない)。
func (s *Service) checkRegistrationOpen() error {
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return fmt.Errorf("signup: fetch meta for registration gate: %w", err)
	}
	if meta.RegistrationClosed {
		return ErrRegistrationClosed
	}
	return nil
}

// checkApprovalGate rejects a pending signup that carries no approval
// application while approval-based signup is enabled (#2804).
//
// 見るのは**申請 ID を持つかどうか**だけで、その申請が承認済みかは見ない。
// そちらは settleApplicationTx (#2576) の担当。
//
// **承認制は「承認を経ていないローカルアカウントは存在しない」ことの主張。**
// 申請に紐付かない `user_pending` は #2576 の確定処理を通らないので、ゲートが
// 無いと承認を経ずにアカウントになる。`PendingSignupTTL` は 30 分なので、承認制へ
// 切り替える直前 30 分に発行された確認メールがそのまま通っていた。窓が開くのは
// 切り替え**前**に `emailRequiredForSignup` が ON だった構成だけ (OFF なら
// `/api/signup` が即座にアカウントを作るので待ち行列が無い)。
//
// **入口に置く。** `promotePendingTx` / `promotePendingNoTx` はここからしか
// 呼ばれないので、1 箇所で両経路が塞がる。tx の中に置くと mirror を保つ箇所が
// 増えるうえ (#610 item 3)、`MetaRepository` は tx を受けないので tx が接続を
// 掴んだまま別の接続を取ることになる。本番配線は TTL 5 分のキャッシュ付きなので
// tx 内で読んでも原子性は得られず、得られるのは「ユーザーを作る前に弾く」ことだけで、
// それは入口でも成立する。
//
// **meta が読めなければ通さない。** 既存の username 検査は読めなければ素通しするが、
// ゲートで同じ形にすると DB 障害が承認の迂回路になる。error はそのまま返して
// ErrApplicationNotApproved に丸めない — DB 障害をドメインの答えに化けさせない
// (#2799)。**障害が 30 分 (`PendingSignupTTL`) を超えると確認リンクは失効する**
// ので「復旧後にやり直せる」とは限らないが、結論は変わらない — 承認を経ない
// アカウントを作るより、申請からやり直してもらうほうがよい。
func (s *Service) checkApprovalGate(pending *model.UserPending) error {
	if pending.SignupApplicationID != nil {
		// 本番経路 (promotePendingTx) は settleApplicationTx で申請を確定させる
		// (#2576)。noTx 経路は申請に触らないが、これは db 未配線の mock 用で、
		// 本番では router の配線検査 (signup.applicationSettlement) が通ることを
		// 保証している。
		return nil
	}
	meta, err := s.metaRepo.Fetch()
	if err != nil {
		return fmt.Errorf("signup: fetch meta for approval gate: %w", err)
	}
	if meta.ApprovalRequiredForSignup {
		slog.Warn("promote pending: rejected unapproved pending signup",
			"pendingId", pending.ID)
		return ErrApplicationNotApproved
	}
	return nil
}

// promotePendingTx は db.Transaction 内で user 作成 / ticket 消費 / pending
// 削除を atomic に行う本番経路。
//
// IMPORTANT: promotePendingTx と promotePendingNoTx は **mirror で維持** する。
// username collision check / user-profile create / keypair gen / webhook hook /
// SignupResult build の順序や意味が両者でずれると挙動が divergence する。
// 片方を変更したら必ずもう片方も同じ shape に揃えること (#610 item 3)。
// tx 経路は raw GORM (tx.Create, tx.Where 等) を直接呼び、noTx 経路は repo
// 経由 (mock 互換のため)。
func (s *Service) promotePendingTx(pending *model.UserPending) (*SignupResult, error) {
	lower := strings.ToLower(pending.Username)
	token := generateToken()
	now := time.Now()
	userID := s.idGen.Generate(now)

	var resultUser *model.User
	var resultProfile *model.UserProfile
	appSettled := false
	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		// invitation ticket がある場合は最初に SELECT FOR UPDATE で lock し、
		// 既に usedById がセットされていれば早期 return。これで concurrent
		// promote 試行は直列化される (#604 race fix)。
		var lockedTicketID string
		if pending.InvitationTicketID != nil {
			ticket, err := s.ticketRepo.FindByIDForUpdateTx(tx, *pending.InvitationTicketID)
			if err != nil {
				// gorm.ErrRecordNotFound は admin が ticket を revoke したケース
				// なので "Revoked" として明示。それ以外 (DB error) は internal
				// として上に伝える (handler が 500 を返す)。
				// 観測用 slog.Warn で運用側に signal を出す (#610 item 2)。
				if errors.Is(err, gorm.ErrRecordNotFound) {
					slog.Warn("promote pending: invitation ticket missing",
						"pendingId", pending.ID, "ticketId", *pending.InvitationTicketID)
					return ErrInvitationRevoked
				}
				return err
			}
			if ticket.UsedByID != nil {
				return ErrInvitationAlreadyUsed
			}
			lockedTicketID = ticket.ID
		}

		// 承認制 (#2571) の申請も同じ tx 内で行ロックして確定させる (#2576)。
		//
		// **アカウント作成と別々にすると 1 つの承認から 2 アカウント作れる。**
		// 申請者が確認リンクを踏むのと登録フォームの再送が重なると、2 つの
		// pending が別々の ticket を持って両方生き残る。ticket ロック (#604) は
		// ticket が違うので直列化できない。申請行を掴んで初めて閉じる。
		if pending.SignupApplicationID != nil && s.appRepo != nil {
			if err := s.settleApplicationTx(tx, *pending.SignupApplicationID, userID, lockedTicketID, now); err != nil {
				return err
			}
			appSettled = true
		}

		verified := true
		user, profile, err := s.createUserTx(tx, createUserInput{
			userID:        userID,
			username:      pending.Username,
			lower:         lower,
			token:         token,
			passwordHash:  pending.Password,
			email:         &pending.Email,
			emailVerified: verified,
		})
		if err != nil {
			return err
		}
		resultProfile = profile

		// pending row 削除
		if err := tx.Where("id = ?", pending.ID).Delete(&model.UserPending{}).Error; err != nil {
			return err
		}

		// ticket 消費 (lock 中なので 100% 自分が最初)
		if lockedTicketID != "" {
			if err := s.ticketRepo.MarkUsedTx(tx, lockedTicketID, userID); err != nil {
				return err
			}
		}

		resultUser = user
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	// webhook hook は tx 完了後 (ベストエフォート、commit 後の non-critical 副作用)
	if s.webhookHook != nil {
		s.webhookHook.OnUserCreated(resultUser)
	}
	return &SignupResult{
		User:               resultUser,
		Token:              token,
		InvitationTicketID: pending.InvitationTicketID,
		// 招待 ticket がある場合のみ tx 内で MarkUsedTx 済 → consumed = true。
		// 非招待 pending では nil なので consumed = false で返し、handler 側でも
		// 何もしない (InvitationTicketID nil で早期 return)。
		InvitationTicketConsumed:   pending.InvitationTicketID != nil,
		SignupApplicationID:        pending.SignupApplicationID,
		SignupApplicationCompleted: appSettled,
		Profile:                    resultProfile,
	}, nil
}

// promotePendingNoTx は db 未配線の mock テスト経路 (transaction 無し)。
// production では使わない (router 配線時に SetDB 必須)。
//
// IMPORTANT: promotePendingTx と mirror で維持すること。詳細は
// promotePendingTx の doc コメント参照 (#610 item 3)。
// invitation ticket の lock / consume は tx 経路でしか行えないため、本経路
// では InvitationTicketID を SignupResult に伝搬するだけで MarkUsed しない
// (handler が non-tx 経路と判定して fallback consume する)。
func (s *Service) promotePendingNoTx(pending *model.UserPending) (*SignupResult, error) {
	lower := strings.ToLower(pending.Username)
	if _, err := s.userRepo.FindByUsernameLower(lower, nil); err == nil {
		return nil, ErrUsernameAlreadyExists
	}

	token := generateToken()
	now := time.Now()
	userID := s.idGen.Generate(now)
	user := &model.User{
		ID:                userID,
		Username:          pending.Username,
		UsernameLower:     lower,
		Token:             &token,
		IsExplorable:      true,
		AvatarDecorations: []byte("[]"),
	}
	if err := s.userRepo.Create(user); err != nil {
		return nil, err
	}
	storedHash := pending.Password
	profile := &model.UserProfile{
		UserID:             userID,
		Email:              &pending.Email,
		EmailVerified:      true,
		Password:           &storedHash,
		AutoAcceptFollowed: true,
		PreventAiLearning:  true,
		PublicReactions:    true,
		// DB 側の default と同値。in-memory の profile をそのまま packer へ
		// 渡すので、明示しないとレスポンスだけ false になる。
		InjectFeaturedNote:       true,
		ReceiveAnnouncementEmail: true,
	}
	if err := s.userRepo.CreateProfile(profile); err != nil {
		return nil, err
	}
	// #2106 N23: used_username に記録 (non-tx 経路。Signup と mirror)。
	s.recordUsedUsername(lower)
	if s.keypairRepo != nil {
		privPEM, pubPEM, err := activitypub.GenerateRSAKeypair()
		if err != nil {
			return nil, err
		}
		if err := s.keypairRepo.Create(&model.UserKeypair{
			UserID:     userID,
			PublicKey:  pubPEM,
			PrivateKey: privPEM,
		}); err != nil {
			return nil, err
		}
	}
	if err := s.maybeCreateEd25519Keypair(nil, userID); err != nil {
		return nil, err
	}
	_ = s.pendingRepo.Delete(pending.ID)

	if s.webhookHook != nil {
		s.webhookHook.OnUserCreated(user)
	}
	return &SignupResult{
		User:                user,
		Token:               token,
		InvitationTicketID:  pending.InvitationTicketID,
		SignupApplicationID: pending.SignupApplicationID,
	}, nil
}

// SignupForApplication creates an account for an approved application, settling
// the application in the same transaction (#2580).
//
// **状態確認とアカウント作成を分けると 1 つの承認から複数アカウント作れる。**
// 承認済みの申請者が別々の username で同時に登録すると、両方が「承認済み」を
// 読んでから両方がアカウントを作ってしまう。申請行を掴んだまま作れば、負けた側は
// ユーザー作成ごと巻き戻る。
//
// db / 申請 repo が未配線なら通常の Signup にフォールバックし、
// SignupApplicationCompleted=false を返す (呼び出し側が従来どおり完了記録する)。
func (s *Service) SignupForApplication(username, password, applicationID, ticketID string) (*SignupResult, error) {
	if s.db == nil || s.appRepo == nil {
		// 申請経由なので公開登録と同じ制限を適用する (#3015)。
		return s.Signup(username, password, false, UsernamePolicyPublic)
	}

	username, err := normalizeAndValidateUsername(username)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(username)
	// tx 内でも collision は見るが、**メールも鍵も作る前に弾ける分はここで弾く**
	// (Signup と同じ順序: existing → used → preserved)。
	if _, err := s.userRepo.FindByUsernameLower(lower, nil); err == nil {
		return nil, ErrUsernameAlreadyExists
	}
	if s.isUsedUsername(lower) {
		return nil, ErrUsernameUsed
	}
	if meta, err := s.metaRepo.Fetch(); err == nil {
		// 最小文字数 (#3015)。この 2 経路は公開登録と申請経由なので、
		// 常に制限を適用する (admin が通る経路ではない)。
		if violatesMinimumUsernameLength(username, meta, UsernamePolicyPublic) {
			return nil, ErrUsernameTooShort
		}
		if isReservedUsername(lower, meta.PreservedUsernames) {
			return nil, ErrUsernameReserved
		}
		if keyword.IsKeyWordIncluded(lower, meta.ProhibitedWordsForNameOfUser) {
			return nil, ErrUsernameUsed
		}
	}

	hash, err := passwordpkg.Hash(password)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return nil, ErrPasswordTooLong
		}
		return nil, fmt.Errorf("signup: bcrypt hash password: %w", err)
	}

	token := generateToken()
	now := time.Now()
	userID := s.idGen.Generate(now)

	var resultUser *model.User
	var resultProfile *model.UserProfile
	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		// 申請行を先に掴む。**ここで弾いた場合はユーザー作成ごと巻き戻る。**
		if err := s.settleApplicationTx(tx, applicationID, userID, ticketID, now); err != nil {
			return err
		}
		user, profile, err := s.createUserTx(tx, createUserInput{
			userID:       userID,
			username:     username,
			lower:        lower,
			token:        token,
			passwordHash: hash,
			// メール確認を通っていないので、プロフィールにメールは持たせない
			// (Signup と同じ)。
		})
		if err != nil {
			return err
		}
		resultUser, resultProfile = user, profile
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	if s.webhookHook != nil {
		s.webhookHook.OnUserCreated(resultUser)
	}
	appID := applicationID
	return &SignupResult{
		User:                       resultUser,
		Token:                      token,
		Profile:                    resultProfile,
		SignupApplicationID:        &appID,
		SignupApplicationCompleted: true,
	}, nil
}

// createUserInput carries everything createUserTx needs to build one account.
type createUserInput struct {
	userID        string
	username      string
	lower         string
	token         string
	passwordHash  string
	email         *string
	emailVerified bool
	// managedByPlugin が non-nil なら、プラグインが管理するアカウント (#3468)
	// として作る。passwordHash は使わず (パスワードを持たせない)、isBot を立てる。
	managedByPlugin *string
}

// createUserTx creates the user, profile, used_username entry and keypairs
// inside the caller's transaction.
//
// **promotePendingTx と承認済み登録 (#2580) が共有する。** 生成の中身を経路ごとに
// 書き写すと、片方だけ直したときに「この経路で作った account だけ鍵が無い」類の
// ずれが生まれる。非 tx 経路 (Signup / promotePendingNoTx) は repo 越しなので
// 別実装のままだが、**tx 経路はここ 1 本に集約する**。
func (s *Service) createUserTx(tx *gorm.DB, in createUserInput) (*model.User, *model.UserProfile, error) {
	// username collision チェック (tx の visibility で in-flight 行も見える)
	var existing model.User
	switch err := tx.Where(`"usernameLower" = ? AND "host" IS NULL`, in.lower).First(&existing).Error; {
	case err == nil:
		return nil, nil, ErrUsernameAlreadyExists
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 続行
	default:
		return nil, nil, err
	}

	user := &model.User{
		ID:                in.userID,
		Username:          in.username,
		UsernameLower:     in.lower,
		Token:             &in.token,
		IsExplorable:      true,
		AvatarDecorations: []byte("[]"),
		ManagedByPlugin:   in.managedByPlugin,
		// 管理するアカウントは必ず bot として作る (#3468)。人が中に入らない
		// アカウントなので、相手に bot であることを示す。
		IsBot: in.managedByPlugin != nil,
	}
	if err := tx.Create(user).Error; err != nil {
		return nil, nil, err
	}

	// profile 作成 (failure 時は user 含めて tx rollback で消える)
	//
	// **管理するアカウントにはパスワードを持たせない (#3468)。** 空文字の hash を
	// 入れると「パスワードはあるが一致しない」状態になり、パスワードを要する
	// endpoint の分岐が経路ごとにずれる。NULL なら全経路が「未設定」で揃う。
	var storedHash *string
	if in.managedByPlugin == nil {
		h := in.passwordHash
		storedHash = &h
	}
	profile := &model.UserProfile{
		UserID:             in.userID,
		Email:              in.email,
		EmailVerified:      in.emailVerified,
		Password:           storedHash,
		AutoAcceptFollowed: true,
		PreventAiLearning:  true,
		PublicReactions:    true,
		// DB 側の default と同値 (非 tx 経路と揃える)。
		InjectFeaturedNote:       true,
		ReceiveAnnouncementEmail: true,
	}
	if err := tx.Create(profile).Error; err != nil {
		return nil, nil, err
	}

	// #2106 N23: used_username に同 tx 内で記録する (upstream SignupService:151-154、
	// 削除後の username 再取得を恒久的に防ぐ)。fresh username のみ到達するため
	// (Exists guard が既使用を事前に弾く) conflict は起きない想定。
	if err := tx.Create(&model.UsedUsername{Username: in.lower}).Error; err != nil {
		return nil, nil, err
	}

	// keypair (federation 用) — keypairRepo が wire されているときのみ
	if s.keypairRepo != nil {
		privPEM, pubPEM, err := activitypub.GenerateRSAKeypair()
		if err != nil {
			return nil, nil, err
		}
		if err := tx.Create(&model.UserKeypair{
			UserID:     in.userID,
			PublicKey:  pubPEM,
			PrivateKey: privPEM,
		}).Error; err != nil {
			return nil, nil, err
		}
	}
	// Ed25519 keypair も同 tx 内で発行 (#1067 / #1068)
	if err := s.maybeCreateEd25519Keypair(tx, in.userID); err != nil {
		return nil, nil, err
	}
	return user, profile, nil
}

// settleApplicationTx locks the approval application and marks it completed
// inside the caller's transaction (#2576).
//
// core/signupapplication.Service.MarkCompleted と同じ判定を、アカウント作成と
// 同じ tx で行う。**ここで弾いた場合はユーザー作成ごと巻き戻る**のが要点で、
// 「作ってから完了記録に失敗する」形にすると承認の記録に残らないアカウントが
// 残る。
func (s *Service) settleApplicationTx(tx *gorm.DB, applicationID, userID, ticketID string, now time.Time) error {
	app, err := s.appRepo.FindByIDForUpdateTx(tx, applicationID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			slog.Warn("promote pending: signup application missing",
				"applicationId", applicationID)
			return ErrApplicationNotApproved
		}
		return err
	}
	if app.Status != model.SignupApplicationApproved {
		return ErrApplicationNotApproved
	}
	// 期限切れは承認済みでも通さない。MarkCompleted と同じ判定。
	if !now.Before(app.ExpiresAt) {
		return ErrApplicationNotApproved
	}
	fields := map[string]any{
		"status":    model.SignupApplicationCompleted,
		"usedById":  userID,
		"updatedAt": now,
	}
	if ticketID != "" {
		fields["ticketId"] = ticketID
	}
	return s.appRepo.UpdateFieldsTx(tx, applicationID, fields)
}

// isReservedUsername reports whether lower (already lowercased) matches any
// entry in reserved case-insensitively. Entries in reserved are trimmed and
// lowercased before comparison so DB-side whitespace noise does not defeat
// the check.
func isReservedUsername(lower string, reserved []string) bool {
	for _, r := range reserved {
		if strings.EqualFold(strings.TrimSpace(r), lower) {
			return true
		}
	}
	return false
}

// IsReservedUsername reports whether lower (already lowercased) matches any
// entry in reserved case-insensitively. Exposed for the username/available
// endpoint so it shares signup's preservedUsernames check (#1551)。
func IsReservedUsername(lower string, reserved []string) bool {
	return isReservedUsername(lower, reserved)
}

// EffectiveMinimumUsernameLength returns the value the API should advertise
// (#3015)。列が範囲外の値を持っていても、**実際に効く値**を返す。
//
// **生の列値をそのまま公開しない。** frontend は事前チェックにこれを使うので、
// 0 や 999 が出ると「1 文字で通るのに弾かれる」「何を入れても弾かれる」と
// 表示がずれる。
func EffectiveMinimumUsernameLength(meta *model.Meta) int {
	return minimumUsernameLength(meta)
}

// ViolatesMinimumUsernameLength is the exported form used by
// `POST /api/username/available` (#3015).
//
// **`IsReservedUsername` と同じ理由で export する。** 判定を available 側に
// 書き写すと、値の解釈 (0 や範囲外の倒し方) が 2 箇所に分かれ、
// 「空いています」と案内した名前が登録で弾かれる形になる。
func ViolatesMinimumUsernameLength(username string, meta *model.Meta, policy UsernamePolicy) bool {
	return violatesMinimumUsernameLength(username, meta, policy)
}

// HasTicketConsumption reports whether the transactional promote path is wired.
//
// 未配線だと promotePending が `promotePendingNoTx` を選ぶ。**失われるのは
// 消費そのものではなく直列化**: handler 側が非 tx 経路を検知して
// best-effort に MarkUsed するので (`internal/api/signup/handler.go`)、
// ticket は最終的に消費される。落ちるのは tx 内の `SELECT FOR UPDATE` と
// usedById の再確認 (#604) で、その結果 promote 前に作られた複数の pending が
// 全て通る TOCTOU の窓が開く。ErrInvitationAlreadyUsed も返らなくなる。
// 本番では必ず配線される (#2682 review M-B)。
//
// **承認制 (#2571) の申請とは別の話**。こちらが見るのは
// registration_ticket で、申請側は appRepo が要る
// ([Service.HasApplicationSettlement])。片方が配線されていても
// もう片方は保証されない (#2682 review M-3)。
func (s *Service) HasTicketConsumption() bool { return s.db != nil && s.ticketRepo != nil }

// HasApplicationSettlement reports whether approved signup applications are
// settled in the same transaction as user creation.
//
// 未配線だと 2 経路が同時に緩む。PromotePending は申請行の
// `FOR UPDATE` を飛ばし、SignupForApplication は通常の Signup へ落ちて
// トランザクションの主張ごと捨てる。どちらも**承認 1 件から複数
// アカウント**を作れる窓が開く (コードのコメントは
// promotePendingTx / SignupForApplication の両方にある)。
//
// handler 側の事後 MarkCompleted が残るので不可逆な取り放題ではなく
// TOCTOU に留まるが、承認制は「1 承認 = 1 アカウント」を売りにする機能
// なので一回性の tier として検査する (#2682 review M-3)。
func (s *Service) HasApplicationSettlement() bool { return s.db != nil && s.appRepo != nil }
