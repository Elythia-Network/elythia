// Package user provides core business logic services for users.
package user

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub/mfm"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/hashtag"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/idnhost"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"

	"github.com/shiroha-a/mk/internal/core/role"
)

// MaxPinnedNotes is the upper limit on pinned notes per user.
// Misskey本家のデフォルトと同じく5件。
const MaxPinnedNotes = 5

// Errors returned by Service.
var (
	// ErrUserNotFound is returned when the target user does not exist.
	ErrUserNotFound = errors.New("user not found")
	// ErrFailedToResolveRemoteUser is returned when ShowByUsername is called
	// with a non-local host and remote resolution fails. Handlers map this
	// to the dedicated FAILED_TO_RESOLVE_REMOTE_USER API error.
	ErrFailedToResolveRemoteUser = errors.New("failed to resolve remote user")
	// ErrInvalidParam is returned when neither userId nor username is given.
	ErrInvalidParam = errors.New("userId or username is required")
	// ErrNoteNotFound is returned when the target note does not exist or is
	// not owned by the requesting user.
	ErrNoteNotFound = errors.New("note not found")
	// ErrAlreadyPinned is returned when pinning an already-pinned note.
	ErrAlreadyPinned = errors.New("already pinned")
	// ErrPinLimitExceeded is returned when the user already has MaxPinnedNotes notes pinned.
	ErrPinLimitExceeded = errors.New("pin limit exceeded")
	// ErrPinNotFound is returned when unpinning a note that is not pinned.
	ErrPinNotFound = errors.New("pin not found")
	// ErrAvatarNotFound is returned when avatarId points to a missing or
	// non-owned drive_file. Mirrors upstream Misskey's NO_SUCH_AVATAR.
	ErrAvatarNotFound = errors.New("avatar drive file not found")
	// ErrAvatarNotImage is returned when avatarId points to a non-image
	// drive_file. Mirrors upstream's AVATAR_NOT_AN_IMAGE.
	ErrAvatarNotImage = errors.New("avatar drive file is not an image")
	// ErrBannerNotFound / ErrBannerNotImage are the banner counterparts.
	ErrBannerNotFound = errors.New("banner drive file not found")
	ErrBannerNotImage = errors.New("banner drive file is not an image")
)

// MainStreamPublisher emits real-time events to a single target user's `main`
// WebSocket channel. Used here for `meUpdated` so the frontend can reflect
// profile changes immediately. 循環依存を避けるため interface で受け取る
// (実装は internal/stream)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// RemoteUserResolver resolves a (username, host) pair into a local User row,
// fetching from the remote host via WebFinger + ActivityPub when the user is
// not yet cached locally. 循環依存を避けるため interface で受け取る (実装は
// core/federation)。
type RemoteUserResolver interface {
	ResolveByUsernameHost(username, host string) (*model.User, error)
}

// RemoteUserResyncer is implemented by a RemoteUserResolver that can re-sync a
// stored remote user whose data is stale (upstream RemoteUserResolveService.
// resolveUser re-syncs a user found in the DB after 24 hours).
type RemoteUserResyncer interface {
	ResyncIfStale(u *model.User) (*model.User, error)
}

// Service provides user-related business logic.
type Service struct {
	userRepo            repository.UserRepository
	noteRepo            repository.NoteRepository
	piningRepo          repository.UserNotePiningRepository
	idGen               id.Generator
	mainStreamPublisher MainStreamPublisher
	remoteResolver      RemoteUserResolver
	// driveFileRepo は avatar / banner SET 時に drive_file 行を引いて
	// 所有者検証 + image MIME チェックするのに使う。未配線時は media
	// 更新経路全体を skip して旧来挙動 (avatar / banner 不変) に戻す。
	driveFileRepo repository.DriveFileRepository
	// rolePolicyProvider は PinNote の上限を role policy `pinLimit` で
	// override するのに使う (#1029)。nil 時は MaxPinnedNotes 定数 fallback
	// (= 旧挙動互換)。実装は core/role.Service。
	rolePolicyProvider role.PolicyProvider
	// selfHostname は SearchByUsernameAndHost で「host==self-hostname → local
	// 限定」を判定するために保持する (upstream UserSearchService と互換、
	// #1064)。空文字なら remap を skip して "." 一致のみ local 限定にする。
	// SetSelfHostname 内で TrimSpace + lowercase に正規化して格納する
	// (search 側 per-call の ToLower を省くためと、whitespace のみ入力で
	// remap が無意味に有効化される edge case の防御)。
	selfHostname string
	// usertagHook は profile description 由来の user.tags が変わった時に
	// hashtag 集計 (attachedUsersCount) を更新する hook。未配線なら集計を
	// skip (= populate のみ、旧挙動)。実装は core/hashtag.Service。
	usertagHook UsertagHook
}

// UsertagHook reconciles the hashtag attach aggregate when a user's tags
// change. 実装は core/hashtag.Service.UpdateUsertags。
type UsertagHook interface {
	UpdateUsertags(userID string, isLocal bool, oldTags, newTags []string)
}

// SetRolePolicyProvider wires a role policy source so the user service can
// honour role-policy overrides for limits like pinLimit (#1029). nil 時は
// MaxPinnedNotes 定数 fallback の旧挙動を維持する。
func (s *Service) SetRolePolicyProvider(p role.PolicyProvider) {
	s.rolePolicyProvider = p
}

// NewService creates a new user Service.
// noteRepo, piningRepo, idGen are optional for callers that only need the
// read-only show methods (pass nil); the pin-related methods require them.
func NewService(
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
	piningRepo repository.UserNotePiningRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		userRepo:   userRepo,
		noteRepo:   noteRepo,
		piningRepo: piningRepo,
		idGen:      idGen,
	}
}

// SetMainStreamPublisher attaches a publisher used to emit `main` channel
// events (currently `meUpdated`). Optional — nil disables emit.
func (s *Service) SetMainStreamPublisher(p MainStreamPublisher) {
	s.mainStreamPublisher = p
}

// SetDriveFileRepository attaches a DriveFileRepository used by
// UpdateProfile to resolve avatarId / bannerId into the corresponding
// drive_file row (for ownership and image-MIME validation, plus
// avatarUrl / avatarBlurhash population). Optional — nil leaves the
// avatar / banner update paths inert (the input fields are silently
// ignored, matching pre-#467 behaviour).
func (s *Service) SetDriveFileRepository(repo repository.DriveFileRepository) {
	s.driveFileRepo = repo
}

// SetRemoteUserResolver attaches a resolver for remote (non-local) users.
// When set, ShowByUsername falls back to remote fetch (WebFinger + AP) for
// DB misses with non-nil host. Optional — nil disables remote fallback and
// leaves ShowByUsername DB-only.
func (s *Service) SetRemoteUserResolver(r RemoteUserResolver) {
	s.remoteResolver = r
}

// SetSelfHostname records the running instance's canonical hostname so that
// SearchByUsernameAndHost can map `host==self-hostname` to "local only" the
// same way upstream UserSearchService does (#1064). Empty string disables the
// remap (only the literal `"."` keeps the local-only shortcut).
//
// 入力は setter 内で TrimSpace + lowercase に正規化して保持する。search の
// per-call ホットパスで毎回 strings.ToLower するのを避けるためと、
// whitespace のみ入力 (= "  ") で remap が無意味に有効化される edge case を
// 防ぐ防御。実 caller は `s.config.Hostname` (= `url.Hostname()` 由来) で
// whitespace が入る経路は通常 無いが、Setter contract として境界条件を
// 確実にする。
// SetUsertagHook wires a hook that updates the hashtag attach aggregate when
// a user's profile-derived tags change (#1362).
func (s *Service) SetUsertagHook(h UsertagHook) {
	s.usertagHook = h
}

func (s *Service) SetSelfHostname(hostname string) {
	s.selfHostname = strings.ToLower(strings.TrimSpace(hostname))
}

// UserWithProfile bundles a user and its profile for handlers.
type UserWithProfile struct {
	User    *model.User
	Profile *model.UserProfile
}

// FindManyByIDs returns the bare User rows for the given ID set in a single
// query, without profile fetch. Intended for embed-context callers that just
// need UserLite shape (= username / displayName / host / avatar) — currently
// /api/pages/featured (#1134) batch-resolves owners this way.
func (s *Service) FindManyByIDs(ids []string) ([]*model.User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.userRepo.FindManyByIDs(ids)
}

// ShowByID returns the user (and profile) for the given ID.
func (s *Service) ShowByID(id string) (*UserWithProfile, error) {
	u, err := s.userRepo.FindByID(id)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrUserNotFound
	}
	// Profileの取得失敗は致命ではないので無視する
	profile, _ := s.userRepo.FindProfileByUserID(u.ID)
	return &UserWithProfile{User: u, Profile: profile}, nil
}

// ShowManyByIDs returns the users (and their profiles) for the given ID set
// in a constant number of queries (1 user batch + 1 profile batch). Order
// matches `ids`; missing rows are skipped silently. Replaces the per-ID
// ShowByID loop in the users/show bulk path (#503).
func (s *Service) ShowManyByIDs(ids []string) ([]*UserWithProfile, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	users, err := s.userRepo.FindManyByIDs(ids)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, nil
	}
	foundIDs := make([]string, 0, len(users))
	for _, u := range users {
		foundIDs = append(foundIDs, u.ID)
	}
	profiles, err := s.userRepo.FindProfilesByUserIDs(foundIDs)
	if err != nil {
		// Profile fetch 失敗は ShowByID と挙動を合わせて非致命にする
		// (個別 ShowByID も err を握りつぶしている)。
		profiles = nil
	}
	profileByUser := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		profileByUser[p.UserID] = p
	}
	userByID := make(map[string]*model.User, len(users))
	for _, u := range users {
		userByID[u.ID] = u
	}
	out := make([]*UserWithProfile, 0, len(ids))
	for _, id := range ids {
		u, ok := userByID[id]
		if !ok {
			continue
		}
		out = append(out, &UserWithProfile{User: u, Profile: profileByUser[u.ID]})
	}
	return out, nil
}

// ShowByUsername returns the user (and profile) for the given username and host.
//
// host が nil もしくは空の場合はローカルユーザーとして DB を参照するのみ。
// host が指定されていてローカル DB に該当が無い場合、RemoteUserResolver が
// 設定されていれば WebFinger + ActivityPub 経由でリモート fetch を試みる。
// resolver 未設定の場合は ErrUserNotFound を返し (後方互換)、設定済みで解決
// に失敗した場合は ErrFailedToResolveRemoteUser を返す。
func (s *Service) ShowByUsername(username string, host *string) (*UserWithProfile, error) {
	return s.showByUsername(username, host, false)
}

// ResolveByUsername is ShowByUsername for users/show, which upstream answers
// with RemoteUserResolveService.resolveUser whenever a host is given: besides
// fetching an unknown remote user, a stored remote user whose lastFetchedAt is
// older than 24 hours is re-synced (WebFinger + actor fetch) before it is
// returned, and a failed re-sync is ErrFailedToResolveRemoteUser.
//
// users/followers / users/following は本家が DB だけを引くので、こちらを使わない。
func (s *Service) ResolveByUsername(username string, host *string) (*UserWithProfile, error) {
	return s.showByUsername(username, host, true)
}

func (s *Service) showByUsername(username string, host *string, resync bool) (*UserWithProfile, error) {
	// **正規化は repository に任せる。** `FindByUsernameLower` が引く直前に
	// `idnhost.Puny` を掛けるので (#2704)、ここで掛けると二度手間になるだけ。
	//
	// **非正規化のまま保存された行は引けない (#2996)。** その場合は下のリモート解決
	// (WebFinger) へ落ちる。**行は増えない** — 解決先の actor URI は変わらないので
	// `ResolveActor` の `FindByURI` が既存行に当たる。増えるのは**呼ばれるたびの
	// 外向きリクエスト**のほうで、`LookupActorURI` にキャッシュは無い。
	// `backfill-remote-host` を流していない環境で上げるとこの形になる
	// (経路ごとの症状は docs/deployment.md)。
	u, err := s.userRepo.FindByUsernameLower(username, host)
	if err == nil {
		// 本家 users/show は host 付きなら毎回 resolveUser を通り、保存から 24 時間を
		// 過ぎたリモートの利用者は WebFinger から取り直す (待ってから返す)。
		if resync && host != nil && *host != "" {
			if rs, ok := s.remoteResolver.(RemoteUserResyncer); ok {
				synced, rerr := rs.ResyncIfStale(u)
				if rerr != nil || synced == nil {
					return nil, ErrFailedToResolveRemoteUser
				}
				u = synced
			}
		}
		profile, _ := s.userRepo.FindProfileByUserID(u.ID)
		return &UserWithProfile{User: u, Profile: profile}, nil
	}
	// **DB 障害を not-found に丸めない (#2792 / #2799)。** 丸めると接続断のような
	// 一過性の障害がそのまま下の WebFinger 問い合わせに化ける。`users/show` は
	// 未認証でも叩けるので、DB が不調なあいだ外向きリクエストを外部から任意に
	// 焚き付けられることになる。`ShowByUsernameDB` は #2799 で同じ形に直してある。
	if !repository.IsNotFound(err) {
		return nil, err
	}
	// ローカル DB miss。host 指定なしや resolver 未注入の場合は従来どおり
	// ErrUserNotFound を返し、handler 側で NO_SUCH_USER にマップさせる。
	if host == nil || *host == "" || s.remoteResolver == nil {
		return nil, ErrUserNotFound
	}
	// WebFinger の宛先は punycode へ揃える (upstream RemoteUserResolveService の
	// `:57` と同じ)。Unicode 形のまま投げると名前解決に失敗する。
	resolved, resolveErr := s.remoteResolver.ResolveByUsernameHost(username, idnhost.Puny(*host))
	if resolveErr != nil || resolved == nil {
		return nil, ErrFailedToResolveRemoteUser
	}
	profile, _ := s.userRepo.FindProfileByUserID(resolved.ID)
	return &UserWithProfile{User: resolved, Profile: profile}, nil
}

// ShowByUsernameDB looks up a user by username/host from the local DB only,
// never falling back to the remote resolver.
//
// **公開 AP route (/@:acct) 用 (#2506)。** upstream の /@:acct は DB 照合のみで、
// 未知の acct は 404 を返す。ShowByUsername の remote fallback をここで使うと、
// 認証不要の GET 1 回ごとに WebFinger + actor fetch の outbound HTTP とリモート
// user 行の作成を外部から強制できてしまう (upstream に無い増幅面)。
func (s *Service) ShowByUsernameDB(username string, host *string) (*UserWithProfile, error) {
	u, err := s.userRepo.FindByUsernameLower(username, host)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrUserNotFound
	}
	profile, _ := s.userRepo.FindProfileByUserID(u.ID)
	return &UserWithProfile{User: u, Profile: profile}, nil
}

// GetProfile returns the profile for the given user ID, or nil if not found.
func (s *Service) GetProfile(userID string) *model.UserProfile {
	profile, _ := s.GetProfileErr(userID)
	return profile
}

// GetProfileErr returns the profile and the lookup error.
//
// **DB 障害と not-found を分ける必要がある呼び出し元はこちらを使う** (#2799)。
// `GetProfile` は err を捨てるので、パスワードの有無を見る経路が接続断中に
// 「パスワードが設定されていません」を返していた。
//
// プロフィールが無くても続行するのが正しい用途 (achievement / ap /
// recommendation) は `GetProfile` のままでよい。
func (s *Service) GetProfileErr(userID string) (*model.UserProfile, error) {
	profile, err := s.userRepo.FindProfileByUserID(userID)
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// GetProfilesByUserIDs returns a userID → profile map for the given IDs in
// a single batch query. Missing rows are simply omitted from the map.
// users/search 等の bulk path で per-row GetProfile loop の N+1 を消すために
// 使う (#517)。
func (s *Service) GetProfilesByUserIDs(ids []string) map[string]*model.UserProfile {
	if len(ids) == 0 {
		return map[string]*model.UserProfile{}
	}
	profiles, err := s.userRepo.FindProfilesByUserIDs(ids)
	if err != nil {
		return map[string]*model.UserProfile{}
	}
	out := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		out[p.UserID] = p
	}
	return out
}

// ListRecommendations returns locally-active explorable users the viewer does
// not already follow. Thin wrapper over UserRepository.ListUserRecommendations.
func (s *Service) ListRecommendations(viewerID string, activeSince time.Time, limit, offset int) ([]*model.User, error) {
	return s.userRepo.ListUserRecommendations(viewerID, activeSince, limit, offset)
}

// Search returns users whose username matches the prefix query.
// 空のクエリは空のリストを返す。
//
// origin は upstream Misskey TS の users/search の `origin` param 互換 (#763)。
//   - "local"    → host IS NULL のみ
//   - "remote"   → host IS NOT NULL のみ
//   - "combined" / 空 → filter なし (default)
//
// meID is the requesting user's ID ("" for anonymous), used to exclude muted
// users from the results (upstream UserSearchService、#1939)。
func (s *Service) Search(query, meID string, limit, offset int, origin string) ([]*model.User, error) {
	// raw query を repo に渡す (name ILIKE / @handle 判定 / username 部分一致は
	// repo 側で行うため、@ 除去・lowercase はここで行わない、#1939)。
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	return s.userRepo.SearchUsers(query, meID, limit, offset, origin)
}

// SearchByUsernameAndHost narrows username prefix matches with a host filter.
// upstream Misskey TS の users/search-by-username-and-host endpoint と同じ
// semantics (#766 / #1064):
//   - host=nil or empty  → host filter なし (= local + remote 両方)
//   - host="." or self-hostname → local 限定 (host IS NULL)
//   - host=その他       → 当該 host の prefix match (case-insensitive)
//
// #1064 fix: 旧来は host=nil で local 強制していたが、frontend MkAutocomplete
// は `@alice` だけ入力したとき host=undefined を送るため、これだと remote user
// 候補が一切出ない。upstream `generateUserQueryBuilder` も `params.host`
// falsy で host filter を skip するので合わせる。"." と self-hostname だけ
// local 限定への remap を Service 側で行う (= self-hostname は Service 設定
// から引く)。
func (s *Service) SearchByUsernameAndHost(username string, host *string, limit int) ([]*model.User, error) {
	// username 省略 (= host だけの検索) も upstream は許すので、空文字は
	// 「username で絞らない」を意味する。repository 側で条件を落とす。
	q := strings.TrimSpace(strings.TrimPrefix(username, "@"))
	if limit <= 0 {
		limit = 10
	}
	var hostNorm *string
	localOnly := false
	if host != nil {
		h := strings.ToLower(strings.TrimSpace(*host))
		switch {
		case h == "":
			// falsy → host filter なし。hostNorm は nil のままにする。
		case h == "." || (s.selfHostname != "" && h == s.selfHostname):
			// selfHostname は SetSelfHostname で lowercase 済みなので
			// 直接比較できる (#1064)。
			localOnly = true
		default:
			hostNorm = &h
		}
	}
	return s.userRepo.SearchByUsernameAndHost(strings.ToLower(q), hostNorm, localOnly, limit)
}

// UpdateInput represents the editable fields of a user profile.
// Each pointer is interpreted as "leave unchanged when nil".
type UpdateInput struct {
	Name              **string
	Description       **string
	Location          **string
	Birthday          **string
	Lang              **string
	FollowedMessage   **string
	PublicReactions   *bool
	IsLocked          *bool
	IsBot             *bool
	IsCat             *bool
	IsExplorable      *bool
	HideOnlineStatus  *bool
	AlwaysMarkNsfw    *bool
	AutoSensitive     *bool
	NoCrawle          *bool
	PreventAiLearning *bool
	// 以下 4 field は upstream Misskey TS i/update.ts:178-188 の paramDef に
	// あり profile 側に persist される bool 設定。response 側 MeDetailed
	// packer (#969) には含まれていたが、UpdateInput / UpdateRequest 側で
	// 抜けていて silent drop される drift があった (#972)。
	AutoAcceptFollowed       *bool
	CarefulBot               *bool
	InjectFeaturedNote       *bool
	ReceiveAnnouncementEmail *bool
	// ChatScope は誰からのチャットを許可するか (1-on-1 DM 用)。
	// 受け付けるのは "everyone" / "followers" / "following" / "mutual" / "none"
	// (CherryPick / Misskey TS と同じ enum)。検証は呼び出し側 (#692)。
	ChatScope *string
	// FollowingVisibility / FollowersVisibility はフォロー/フォロワー一覧の
	// 公開範囲 (#1546)。受け付けるのは "public" / "followers" / "private"
	// (Misskey TS と同じ enum)。enum 検証は呼び出し側 (api/i ハンドラ) で行う。
	FollowingVisibility *string
	FollowersVisibility *string
	// Room は jsonb 列に書き込む生バイト列。nil の場合は更新しない。
	// 呼び出し側で JSON として妥当であることを保証する必要がある。
	Room *json.RawMessage
	// AvatarID / BannerID は drive_file の ID。
	//   nil       → 不変 (no change)
	//   &"<id>"   → SET (drive_file 行を引いて所有権 + image MIME 検証)
	//   &""       → CLEAR (avatarId / avatarUrl / avatarBlurhash を NULL に)
	//
	// JSON `null` を CLEAR として扱う semantic はサポートしない (Go の
	// *string では null と omitted を区別できないため #467 では空文字列
	// を CLEAR の sentinel にする)。frontend が null clear を要求する
	// ようになったら json.RawMessage ベースに昇格させる別 issue。
	AvatarID *string
	BannerID *string
	// AvatarDecorations は jsonb カラム `user.avatarDecorations` に書き込む
	// 正規化済み JSON バイト列。nil なら不変、`[]` なら全外し、
	// `[{id,angle,flipH,offsetX,offsetY}, ...]` で上書き。検証 (catalog
	// 存在 / role 制限 / 個数上限) はハンドラ側で実施済 (#521)。
	AvatarDecorations *[]byte
	// MutedWords / HardMutedWords は jsonb 列にそのまま書き込む。upstream
	// Misskey TS が受ける `[["foo"], ["bar","baz"]]` 形式 (= 内側配列が AND、
	// 外側が OR) を変換せず保存する。nil = 不変、`[]` = クリア、配列値 = 上書き。
	// JSON 構造の妥当性 (= top-level array であること) はハンドラ側で検証する。
	// 反映: soft mute は frontend の check-word-mute.ts が `/api/i` レスポンスから
	// 読み取って描画時 filter する。hard mute は本 service と TL handler 経由で
	// CheckWordMute helper が note を除外する (#787)。
	MutedWords     *json.RawMessage
	HardMutedWords *json.RawMessage
	// Fields は user_profile.fields の上書き値 (#956)。nil なら不変、
	// 空 slice なら全消去、要素ありなら正規化後 (= 名前 / 値 ともに trim
	// した上で空 entry を除外) を jsonb に書き込む。upstream paramDef は
	// maxItems 16 で、本 service は呼び出し側が cap 済の slice を渡す
	// 前提 (api/i ハンドラ側で長さ検証する)。
	Fields *[]FieldItem
	// AlsoKnownAs は引越し元エイリアス (= i/move の前提となる alsoKnownAs)
	// の URI リスト (#1546)。nil なら不変。空 slice なら NULL クリア、要素
	// ありなら csv 文字列として user.alsoKnownAs に書き込む。各要素は handler
	// 側で acct/URI → canonical URI に解決済みであることを前提とする。
	// 本家 update.ts は配列で保存するが mk-go は text csv で持つ (move_service
	// 側の alsoKnownAsIncludes / appendIfMissing と同じ表現)。
	AlsoKnownAs *[]string
	// 以下は #1546 で追加した i/update 残り field。
	//
	// NotificationRecieveConfig / MutedInstances / EmailNotificationTypes は
	// user_profile の jsonb 列にそのまま書く生バイト列。nil なら不変。
	NotificationRecieveConfig *json.RawMessage
	MutedInstances            *json.RawMessage
	EmailNotificationTypes    *json.RawMessage
	// PinnedPageID は user_profile.pinnedPageId。3 状態を **string で表す:
	//   nil          → 不変 (no change)
	//   &(*string)nil → CLEAR (NULL)
	//   &&"<id>"     → SET (所有権検証はハンドラ側で済んでいる前提)
	PinnedPageID **string
	// RequireSigninToViewContents は user.requireSigninToViewContents。
	RequireSigninToViewContents *bool
	// MakeNotesFollowersOnlyBefore / MakeNotesHiddenBefore は user 列 (*int)。
	// PinnedPageID と同じ 3 状態を **int で表す (nil=不変 / *nil=NULL / **v=値)。
	MakeNotesFollowersOnlyBefore **int
	MakeNotesHiddenBefore        **int
}

// FieldItem represents one row of user_profile.fields. upstream Misskey の
// PropertyValue 連合経路にも乗る shape (= name / value ペア)。
type FieldItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// UpdateProfile applies the non-nil fields to the user and user_profile rows.
func (s *Service) UpdateProfile(userID string, in UpdateInput) (*UserWithProfile, error) {
	existing, err := s.userRepo.FindByID(userID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrUserNotFound
	}

	userFields := map[string]any{}
	profileFields := map[string]any{}
	// description 由来の user.tags が変わった場合に hashtag 集計 hook へ渡す
	// old/new。tagsChanged=false の間は hook を呼ばない。
	var oldTags, newTags []string
	tagsChanged := false

	if in.Name != nil {
		userFields["name"] = *in.Name
	}
	if in.IsLocked != nil {
		userFields["isLocked"] = *in.IsLocked
	}
	if in.IsBot != nil {
		userFields["isBot"] = *in.IsBot
	}
	if in.IsCat != nil {
		userFields["isCat"] = *in.IsCat
	}
	if in.IsExplorable != nil {
		userFields["isExplorable"] = *in.IsExplorable
	}
	if in.HideOnlineStatus != nil {
		userFields["hideOnlineStatus"] = *in.HideOnlineStatus
	}
	if in.Description != nil {
		profileFields["description"] = *in.Description
		// upstream i/update と同じく description から hashtag を抽出し、正規化して
		// user.tags に格納する (hashtags/users の containment query 用)。description が
		// null/空なら空配列になり tags がクリアされる (upstream の `updates.tags = tags`
		// と同挙動)。Description は **string なので inner nil (= 明示的 null) もケアする。
		desc := ""
		if *in.Description != nil {
			desc = **in.Description
		}
		newTags = hashtag.ExtractUserTags(desc)
		// ExtractUserTags は hashtag が無いと nil を返すが、nil の model.StringArray は
		// Updates() map 経由で SQL NULL になり user.tags (NOT NULL) 制約に違反する。
		// その場合 profile 更新全体が atomic に失敗するため空配列に倒して '{}' を書く。
		if newTags == nil {
			newTags = []string{}
		}
		userFields["tags"] = model.StringArray(newTags)
		oldTags = []string(existing.Tags)
		tagsChanged = true
	}
	if in.Location != nil {
		profileFields["location"] = *in.Location
	}
	if in.Birthday != nil {
		profileFields["birthday"] = *in.Birthday
	}
	if in.Lang != nil {
		profileFields["lang"] = *in.Lang
	}
	if in.FollowedMessage != nil {
		profileFields["followedMessage"] = *in.FollowedMessage
	}
	if in.PublicReactions != nil {
		profileFields["publicReactions"] = *in.PublicReactions
	}
	if in.AlwaysMarkNsfw != nil {
		profileFields["alwaysMarkNsfw"] = *in.AlwaysMarkNsfw
	}
	if in.AutoSensitive != nil {
		profileFields["autoSensitive"] = *in.AutoSensitive
	}
	if in.NoCrawle != nil {
		profileFields["noCrawle"] = *in.NoCrawle
	}
	if in.PreventAiLearning != nil {
		profileFields["preventAiLearning"] = *in.PreventAiLearning
	}
	// #972: upstream paramDef にあるが mk-go で抜けていた 4 field を
	// profile に persist する。
	if in.AutoAcceptFollowed != nil {
		profileFields["autoAcceptFollowed"] = *in.AutoAcceptFollowed
	}
	if in.CarefulBot != nil {
		profileFields["carefulBot"] = *in.CarefulBot
	}
	if in.InjectFeaturedNote != nil {
		profileFields["injectFeaturedNote"] = *in.InjectFeaturedNote
	}
	if in.ReceiveAnnouncementEmail != nil {
		profileFields["receiveAnnouncementEmail"] = *in.ReceiveAnnouncementEmail
	}
	if in.ChatScope != nil {
		userFields["chatScope"] = *in.ChatScope
	}
	// followingVisibility / followersVisibility は user_profile 側の enum 列
	// (#1546)。enum 検証はハンドラ側で済んでいる前提で raw string を書く。
	if in.FollowingVisibility != nil {
		profileFields["followingVisibility"] = *in.FollowingVisibility
	}
	if in.FollowersVisibility != nil {
		profileFields["followersVisibility"] = *in.FollowersVisibility
	}
	if in.Room != nil {
		// GORM は map で渡された値を jsonb 列に直接書き込む。
		// []byte を渡すと bytea 扱いされてしまうので string にキャストする。
		profileFields["room"] = string(*in.Room)
	}
	if in.MutedWords != nil {
		profileFields["mutedWords"] = string(*in.MutedWords)
	}
	if in.HardMutedWords != nil {
		profileFields["hardMutedWords"] = string(*in.HardMutedWords)
	}
	if in.AvatarDecorations != nil {
		// avatarDecorations は user (not user_profile) 側の jsonb 列。
		// Room と同じく []byte ではなく string で渡して bytea 化を防ぐ。
		userFields["avatarDecorations"] = string(*in.AvatarDecorations)
	}
	var newFields []FieldItem
	if in.Fields != nil {
		// upstream Misskey TS 互換 (#956): name / value をそれぞれ trim して
		// 両方非空の entry のみ残す。空 slice 渡しはクリア (= []) として
		// 書き込む。jsonb 列なので []byte でなく string キャストで bytea
		// 化を防ぐ。
		normalized := make([]FieldItem, 0, len(*in.Fields))
		for _, f := range *in.Fields {
			n := strings.TrimSpace(f.Name)
			v := strings.TrimSpace(f.Value)
			if n == "" || v == "" {
				continue
			}
			normalized = append(normalized, FieldItem{Name: n, Value: v})
		}
		raw, err := json.Marshal(normalized)
		if err != nil {
			return nil, err
		}
		profileFields["fields"] = string(raw)
		newFields = normalized
	}
	if in.AlsoKnownAs != nil {
		// 引越し元エイリアス (#1546)。upstream update.ts は要素 0 個なら null を
		// 書く (= クリア)。mk-go の alsoKnownAs は varchar csv なので、空なら
		// SQL NULL、要素ありなら "uri1,uri2,..." を書く。move_service の
		// alsoKnownAsIncludes / appendIfMissing が同じ csv 表現を読む。
		if len(*in.AlsoKnownAs) == 0 {
			userFields["alsoKnownAs"] = nil
		} else {
			userFields["alsoKnownAs"] = strings.Join(*in.AlsoKnownAs, ",")
		}
	}
	// #1546 i/update 残り field。jsonb 列は Room と同じく string キャストで
	// bytea 化を防ぐ。
	if in.NotificationRecieveConfig != nil {
		profileFields["notificationRecieveConfig"] = string(*in.NotificationRecieveConfig)
	}
	if in.MutedInstances != nil {
		profileFields["mutedInstances"] = string(*in.MutedInstances)
	}
	if in.EmailNotificationTypes != nil {
		profileFields["emailNotificationTypes"] = string(*in.EmailNotificationTypes)
	}
	if in.PinnedPageID != nil {
		if *in.PinnedPageID == nil {
			profileFields["pinnedPageId"] = nil
		} else {
			profileFields["pinnedPageId"] = **in.PinnedPageID
		}
	}
	if in.RequireSigninToViewContents != nil {
		userFields["requireSigninToViewContents"] = *in.RequireSigninToViewContents
	}
	if in.MakeNotesFollowersOnlyBefore != nil {
		if *in.MakeNotesFollowersOnlyBefore == nil {
			userFields["makeNotesFollowersOnlyBefore"] = nil
		} else {
			userFields["makeNotesFollowersOnlyBefore"] = **in.MakeNotesFollowersOnlyBefore
		}
	}
	if in.MakeNotesHiddenBefore != nil {
		if *in.MakeNotesHiddenBefore == nil {
			userFields["makeNotesHiddenBefore"] = nil
		} else {
			userFields["makeNotesHiddenBefore"] = **in.MakeNotesHiddenBefore
		}
	}

	// avatarId / bannerId 更新 (#467)。driveFileRepo 未配線時は media
	// 更新経路を skip して旧 behaviour に戻す (テスト互換維持)。
	if s.driveFileRepo != nil {
		if err := s.applyMediaUpdate(userID, in.AvatarID, "avatar", userFields, ErrAvatarNotFound, ErrAvatarNotImage); err != nil {
			return nil, err
		}
		if err := s.applyMediaUpdate(userID, in.BannerID, "banner", userFields, ErrBannerNotFound, ErrBannerNotImage); err != nil {
			return nil, err
		}
	}

	emojis, err := s.profileEmojis(userID, existing, in, newFields)
	if err != nil {
		return nil, err
	}
	userFields["emojis"] = model.StringArray(emojis)

	if err := s.userRepo.UpdateUser(userID, userFields); err != nil {
		return nil, err
	}
	if err := s.userRepo.UpdateProfile(userID, profileFields); err != nil {
		return nil, err
	}
	// description 由来 tags が変わったら hashtag 集計 (attachedUsersCount) を
	// 追従させる。i/update は常に local user なので isLocal=true。fire-and-forget。
	if tagsChanged && s.usertagHook != nil {
		s.usertagHook.UpdateUsertags(userID, existing.IsLocal(), oldTags, newTags)
	}

	bundle, err := s.ShowByID(userID)
	if err != nil {
		return nil, err
	}
	// TS本家 UserEntityService.updateMe() 完了後に `meUpdated` を自分の main
	// に publish する (プロフィールページ / UI 側 state を即時再描画させる
	// ため)。body は packed UserDetailed full object。
	s.publishMeUpdated(bundle)
	return bundle, nil
}

// profileEmojis returns the custom emoji names used in the user's name,
// description, profile fields and followed message after the update, like
// upstream i/update: the name and fields are read with parseSimple, the
// description and followed message with the full parser, each source is
// dedup'd on its own and the results are concatenated.
//
// 送らなかった項目は保存済みの値を使う (本家と同じく、どの項目を更新しても
// 毎回まとめて書き直す)。Person の `tag` の絵文字はこの列から描画するので、
// 書かないと連合先で名前やプロフィールの `:emoji:` が文字のまま出る (#3270)。
func (s *Service) profileEmojis(userID string, existing *model.User, in UpdateInput, newFields []FieldItem) ([]string, error) {
	profile, err := s.userRepo.FindProfileByUserID(userID)
	if err != nil {
		if !repository.IsNotFound(err) {
			return nil, err
		}
		profile = nil
	}

	name := existing.Name
	if in.Name != nil {
		name = *in.Name
	}
	var description, followedMessage *string
	if profile != nil {
		description, followedMessage = profile.Description, profile.FollowedMessage
	}
	if in.Description != nil {
		description = *in.Description
	}
	if in.FollowedMessage != nil {
		followedMessage = *in.FollowedMessage
	}
	fields := newFields
	if in.Fields == nil && profile != nil && len(profile.Fields) > 0 {
		// 読めない保存値は補足情報が無いものとして扱う (本家は jsonb の配列を前提に
		// している)。
		_ = json.Unmarshal(profile.Fields, &fields)
	}

	emojis := []string{}
	if name != nil {
		emojis = append(emojis, mfm.ExtractCustomEmojis(mfm.ParseSimple(*name))...)
	}
	if description != nil {
		emojis = append(emojis, mfm.ExtractCustomEmojis(mfm.Parse(*description))...)
	}
	for _, f := range fields {
		emojis = append(emojis, mfm.ExtractCustomEmojis(mfm.ParseSimple(f.Name))...)
		emojis = append(emojis, mfm.ExtractCustomEmojis(mfm.ParseSimple(f.Value))...)
	}
	if followedMessage != nil {
		emojis = append(emojis, mfm.ExtractCustomEmojis(mfm.Parse(*followedMessage))...)
	}
	return emojis, nil
}

// applyMediaUpdate writes prefix+"Id" / prefix+"Url" / prefix+"Blurhash"
// onto userFields based on idPtr's tri-state semantics:
//
//   - nil pointer       → no change (skip)
//   - &""               → CLEAR: set all three columns to NULL
//   - &"<drive_file_id>" → SET: lookup drive_file, validate ownership +
//     image MIME, copy URL / Blurhash to user.
//
// `prefix` is "avatar" or "banner". notFoundErr / notImageErr are the
// specific sentinel errors callers want surfaced (handler maps them to
// distinct API error codes).
func (s *Service) applyMediaUpdate(userID string, idPtr *string, prefix string, userFields map[string]any, notFoundErr, notImageErr error) error {
	if idPtr == nil {
		return nil
	}
	if *idPtr == "" {
		// CLEAR — frontend が "" を送った想定 (JSON null は Go の *string
		// で omitted と区別できないため空文字列を sentinel にする)。
		userFields[prefix+"Id"] = nil
		userFields[prefix+"Url"] = nil
		userFields[prefix+"Blurhash"] = nil
		return nil
	}
	file, err := s.driveFileRepo.FindByID(*idPtr)
	// **DB 障害を not-found に丸めない** (#2799)。呼び出し元は
	// `ErrAvatarNotFound` / `ErrBannerNotFound` を渡し handler が 4xx にするので、
	// 丸めると接続断が「そのアバターは存在しない」になる。
	if err != nil && !repository.IsNotFound(err) {
		return err
	}
	if err != nil || file == nil {
		return notFoundErr
	}
	// 他人の drive_file を avatar / banner に勝手に指定できないように
	// 所有権を検証する (upstream Misskey と同じ)。userId が NULL
	// (未紐付け) も拒否対象。
	if file.UserID == nil || *file.UserID != userID {
		return notFoundErr
	}
	if !strings.HasPrefix(file.Type, "image/") {
		return notImageErr
	}
	userFields[prefix+"Id"] = file.ID
	// **原本 (`file.URL`) を入れない。** あれは所有者にしか渡さない値で
	// (`entity.GetSelfURL` の doc)、EXIF / XMP が載ったままになる。アイコンと
	// バナーの URL はタイムライン・`users/show`・ActivityPub の actor icon に
	// 出るので、原本を入れると撮影位置を含む画像がそのまま公開される。
	// upstream も `getPublicUrl(avatar, 'avatar')` で `webpublicUrl ?? url` を
	// 通している (`DriveFileEntityService.ts`)。
	//
	// **プロキシには通さない。** ここは DB に保存する値で、mk-go の
	// `ProxiedURL` は `sig=` に HMAC を付けるため、保存するとプロキシの secret を
	// 変えた瞬間に全員のアイコン URL が無効になる。リモート origin の包み直しは
	// packer 側 (`entity.PackUserLite` → `ProxyAvatarURL`) が既に行う。
	userFields[prefix+"Url"] = entity.WebpublicOrOriginalURL(file)
	if file.Blurhash != nil {
		userFields[prefix+"Blurhash"] = *file.Blurhash
	} else {
		userFields[prefix+"Blurhash"] = nil
	}
	return nil
}

// publishMeUpdated emits `meUpdated` to the user's main channel with the
// packed UserDetailed object. No-op when no publisher is attached or the
// bundle is missing its User. PinnedNoteIDs is populated from piningRepo
// so pin/unpin changes are reflected in the event body.
func (s *Service) publishMeUpdated(bundle *UserWithProfile) {
	if s.mainStreamPublisher == nil || bundle == nil || bundle.User == nil {
		return
	}
	// profile update / pin / unpin は頻度が低く hot path ではないため
	// full pack する (UserLite ではなく UserDetailed)。
	body := entity.PackUserDetailed(bundle.User, bundle.Profile, s.idGen)
	// **本人にはカウントを見せる。** packer は `followersVisibility` /
	// `followingVisibility` が public でないカウントを既定で伏せるので、
	// self event でゲートを通さないと 0 が流れる。fork frontend は
	// `meUpdated` を `$i` にそのまま merge するので、**プロフィール更新や
	// pin のたびに自分のフォロー数の表示が 0 に化ける** (リロードまで戻らない)。
	// upstream `UserEntityService.pack` も `isMe` に実数を返す。
	entity.GateCountVisibility(&body, true, false, false)
	// meUpdated は本人自身の main channel に流す self event なので、
	// follower-only の followedMessage も本人には見せる (#1558)。PackUserDetailed
	// は followedMessage を set しない (privacy gate) ため、self event では
	// profile から補填する。これを欠くと i/update で followedMessage を変更しても
	// frontend の $i.followedMessage が即時反映されない。
	if bundle.Profile != nil {
		// self event は本人なので follower 扱いで emit (未設定でも null、#2097)。
		entity.SetFollowedMessageForFollower(&body, bundle.Profile.FollowedMessage)
	}
	// PackUserDetailed は pinnedNoteIDs を空で返すため、piningRepo が
	// あれば pin ID だけ埋める (full note pack は不要で frontend 側で
	// 必要に応じて fetch する)。piningRepo がない read-only 構成や
	// 取得失敗時はそのまま空で emit する (best-effort)。
	if s.piningRepo != nil {
		if pinings, err := s.piningRepo.ListByUser(bundle.User.ID); err == nil {
			ids := make([]string, 0, len(pinings))
			for _, p := range pinings {
				ids = append(ids, p.NoteID)
			}
			body.PinnedNoteIDs = ids
		}
	}
	s.mainStreamPublisher.PublishMainEvent(bundle.User.ID, "meUpdated", body)
}

// emitMeUpdatedForUser re-fetches the user bundle and publishes `meUpdated`.
// Returns early when no publisher is attached so the ShowByID DB read is
// avoided. contextLabel is used only in the warning log.
func (s *Service) emitMeUpdatedForUser(userID, contextLabel string) {
	if s.mainStreamPublisher == nil {
		return
	}
	bundle, err := s.ShowByID(userID)
	if err != nil {
		slog.Warn("user: ShowByID failed, skipping meUpdated emit", "context", contextLabel, "userID", userID, "err", err)
		return
	}
	s.publishMeUpdated(bundle)
}

// PinNote pins the given note to the user's profile.
// Returns ErrNoteNotFound if the note doesn't exist or isn't owned by the user,
// ErrAlreadyPinned, or ErrPinLimitExceeded.
func (s *Service) PinNote(userID, noteID string) error {
	note, err := s.noteRepo.FindByID(noteID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNoteNotFound
	}
	if note.UserID != userID {
		return ErrNoteNotFound
	}

	if _, err := s.piningRepo.FindByPair(userID, noteID); err == nil {
		return ErrAlreadyPinned
	}

	count, err := s.piningRepo.CountByUser(userID)
	if err != nil {
		return err
	}
	// role policy `pinLimit` で上限を override (#1029)。未配線 / key 不在 /
	// 非 int の場合は MaxPinnedNotes 定数 fallback (= 旧挙動互換)。
	limit := float64(MaxPinnedNotes)
	if s.rolePolicyProvider != nil {
		if v, ok := role.PolicyNumber(s.rolePolicyProvider.GetUserPolicies(userID)["pinLimit"]); ok {
			limit = v
		}
	}
	if float64(count) >= limit {
		return ErrPinLimitExceeded
	}

	p := &model.UserNotePining{
		ID:     s.idGen.Generate(time.Now()),
		UserID: userID,
		NoteID: noteID,
	}
	if err := s.piningRepo.Create(p); err != nil {
		return err
	}
	// pinnedNoteIds が変わったので main に meUpdated を publish して
	// UI を即時同期する (TS本家 UserEntityService.pinNote と同等)。
	// best-effort emit: publisher 未注入なら何もせず、ShowByID 失敗時は
	// ログのみ残して pin 自体の成功は返す。
	s.emitMeUpdatedForUser(userID, "PinNote")
	return nil
}

// UnpinNote removes a pinning entry. Returns ErrPinNotFound if the user has
// not pinned the given note.
func (s *Service) UnpinNote(userID, noteID string) error {
	p, err := s.piningRepo.FindByPair(userID, noteID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrPinNotFound
	}
	if err := s.piningRepo.Delete(p); err != nil {
		return err
	}
	s.emitMeUpdatedForUser(userID, "UnpinNote")
	return nil
}

// ListPinnedNotes returns the notes pinned by userID, in pinning order
// (most recent pin first).
func (s *Service) ListPinnedNotes(userID string) ([]*model.Note, error) {
	pinings, err := s.piningRepo.ListByUser(userID)
	if err != nil {
		return nil, err
	}
	if len(pinings) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(pinings))
	for _, p := range pinings {
		ids = append(ids, p.NoteID)
	}
	return s.noteRepo.FindManyByIDsWithUser(ids)
}

// UpdateUserFields updates arbitrary fields on the user table.
func (s *Service) UpdateUserFields(userID string, fields map[string]any) error {
	return s.userRepo.UpdateUser(userID, fields)
}

// UpdateProfileFields updates arbitrary fields on the user_profile table.
func (s *Service) UpdateProfileFields(userID string, fields map[string]any) error {
	return s.userRepo.UpdateProfile(userID, fields)
}

// RemoveBackupCode atomically deletes one single-use 2FA backup code.
//
// **読んだ配列を書き戻さない** (#2852)。別のコードを使う同時実行が互いの消費を
// 打ち消し合うため、消す操作そのものを DB 側で行う。
func (s *Service) RemoveBackupCode(userID, code string) error {
	return s.userRepo.RemoveBackupCode(userID, code)
}

// FindProfileByVerifyCode looks up a profile by emailVerifyCode.
func (s *Service) FindProfileByVerifyCode(code string) (*model.UserProfile, error) {
	return s.userRepo.FindProfileByVerifyCode(code)
}
