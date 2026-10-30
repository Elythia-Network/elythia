package admin

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// AccountsCreate handles POST /api/admin/accounts/create.
// 初回セットアップ (rootUserId未設定) の場合は認証不要。
// それ以外はadmin権限が必要。
func (h *Handler) AccountsCreate(c echo.Context) error {
	var req struct {
		Username         string                 `json:"username"`
		Password         string                 `json:"password"`
		SetupPassword    *string                `json:"setupPassword"`
		FederationStatus model.FederationStatus `json:"federationStatus"`
	}
	if err := c.Bind(&req); err != nil || req.Username == "" || req.Password == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if req.FederationStatus != "" && req.FederationStatus != model.FederationStatusOn && req.FederationStatus != model.FederationStatusOff && req.FederationStatus != model.FederationStatusPending {
		return apierr.JSONInvalidParam(c)
	}

	meta, err := h.metaRepo.Fetch()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	user := middleware.GetUser(c)
	isInitialSetup := meta.RootUserID == nil && user == nil

	// **利用者が既に居るなら初回セットアップではない。**
	//
	// `meta.rootUserId` を書くのはコードベース全体で 1 箇所 (signup service の
	// `isInitialSetup` 分岐) しか無く、公開 `/api/signup` は本番でそこを通らない
	// (`internal/api/signup/handler.go` が `h.testMode && ...` で判定している。
	// 開放した瞬間に誰でも root を取れてしまうのを避けるための、それ自体は
	// 妥当な判断)。その結果、**運営者が登録を開放して通常の signup で最初の
	// アカウントを作ると `rootUserId` は永久に NULL のまま**になり、この
	// endpoint が未認証のまま開き続ける。`update-meta` は `rootUserId` を
	// protected として落とすので、管理者が手で閉じることもできない。
	//
	// upstream は `SignupService` が経路を問わず「`rootUserId` が NULL なら
	// 作ったアカウントを root にする」ので最初の 1 人で窓が閉じる。mk-go は
	// そちらを採らない代わりに、ここで閉じる。
	//
	// **数えられなければ初回セットアップとして扱わない (fail-closed)。**
	// 判定できないことを理由に未認証の窓を開けるわけにいかない。正当な初回
	// セットアップは再試行すれば通るし、原因が分かるよう 500 で返す
	// (ACCESS_DENIED に潰すと「権限が足りない」と誤読される)。
	if isInitialSetup {
		if h.userRepo == nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		// プラグインが管理するアカウント (#3468) とシステムアカウントは数えない。起動時に bot を作る
		// プラグインがあると、最初の管理者を作る前に窓が閉じるため。
		n, cerr := h.userRepo.CountLocalUsersForSetup()
		if cerr != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		if n > 0 {
			isInitialSetup = false
		}
	}

	if isInitialSetup {
		// TS互換: setupPassword検証。configにsetupPasswordが設定されている場合は
		// クライアントの値と一致させる。未設定なのにクライアントが非空値を送った場合も拒否。
		clientPW := ""
		if req.SetupPassword != nil {
			clientPW = strings.TrimSpace(*req.SetupPassword)
		}
		if h.configSetupPassword != "" {
			// **定数時間で比較する。** ここは未認証で到達できる唯一の
			// credential 検査で、素の != は一致した先頭バイト数だけ時間が延びる。
			if subtle.ConstantTimeCompare([]byte(clientPW), []byte(h.configSetupPassword)) != 1 {
				return c.JSON(http.StatusBadRequest, apierr.Error("INCORRECT_INITIAL_PASSWORD", "Initial password is incorrect.", "97147c55-1ae1-4f6f-91d6-e1c3e0e76d62"))
			}
		} else if clientPW != "" {
			return c.JSON(http.StatusBadRequest, apierr.Error("INCORRECT_INITIAL_PASSWORD", "Initial password is incorrect.", "97147c55-1ae1-4f6f-91d6-e1c3e0e76d62"))
		}
	} else {
		// 初回セットアップ以外はadministrator権限必須。upstream 2026.7.0 (#17783)
		// で「rootUserId 本人のみ」から「任意の administrator」に緩和された
		// (初期設定で作った root 以外の admin がこの API を使えなかった修正)。
		// roleService 未配線時は旧仕様の root 本人比較に fallback (fail-closed)。
		if user == nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
		if h.roleService != nil {
			if !h.roleService.IsAdministrator(user.ID) {
				return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
			}
		} else if meta.RootUserID == nil || *meta.RootUserID != user.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
		// #2106 S2: upstream create.ts の inline `token !== null` gate を再現する。
		// この endpoint は route middleware を持たず paramDef に kind も無いため、
		// app/OAuth access token (read:account 等の狭い scope) でも root に到達して
		// アカウント作成できる scope 越境になる。native login token のみ許可する。
		if sc := middleware.GetAuthScope(c); sc != nil && sc.IsApp {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
	}

	// **admin は最小文字数の制限を受けない (#3015)。** 運営が公式アカウントに
	// 短い ID を配れるようにするため。`preservedUsernames` は引き続き効くので、
	// 「予約は効くが最小長は効かない」という非対称になる。
	result, err := h.signupService.SignupWithFederationStatus(req.Username, req.Password, isInitialSetup, signup.UsernamePolicyOperator, func() model.FederationStatus {
		if req.FederationStatus == "" {
			return model.FederationStatusOn
		}
		return req.FederationStatus
	}())
	if err != nil {
		if err == signup.ErrUsernameAlreadyExists {
			return c.JSON(http.StatusConflict, apierr.Error("USERNAME_ALREADY_EXISTS", "Username already exists.", "0a504947-b888-4a99-9f62-8c4a0f3a3dab"))
		}
		if err == signup.ErrInvalidUsername {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid username.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		if err == signup.ErrUsernameReserved {
			return c.JSON(http.StatusBadRequest, apierr.Error("USED_USERNAME", "That username is reserved.", "4b54bee6-2c25-42c3-a10f-7d0d1fbd91f9"))
		}
		slog.Error("admin/accounts/create: signup failed", "username", req.Username, "err", err)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	u := result.User
	detailed := h.entityPacker.PackUserDetailed(u, nil)

	isAdmin := false
	isMod := false
	userPolicies := role.DefaultPolicies()
	if h.roleService != nil {
		isAdmin = h.roleService.IsAdministrator(u.ID)
		isMod = h.roleService.IsModerator(u.ID)
		userPolicies = h.roleService.GetUserPolicies(u.ID)
	}

	out := map[string]any{
		// UserLite
		"id":                u.ID,
		"name":              detailed.Name,
		"username":          detailed.Username,
		"host":              detailed.Host,
		"avatarUrl":         detailed.AvatarURL,
		"avatarBlurhash":    detailed.AvatarBlurhash,
		"avatarDecorations": detailed.AvatarDecorations,
		"isBot":             detailed.IsBot,
		"isCat":             detailed.IsCat,
		"emojis":            detailed.Emojis,
		"onlineStatus":      detailed.OnlineStatus,
		"federationStatus":  detailed.FederationStatus,
		"badgeRoles":        badgeRolesForMap(detailed.BadgeRoles),
		// UserDetailed
		"bannerUrl":      detailed.BannerURL,
		"bannerBlurhash": detailed.BannerBlurhash,
		"isLocked":       detailed.IsLocked,
		"isSilenced":     false,
		"isSuspended":    detailed.IsSuspended,
		"description":    detailed.Description,
		"location":       detailed.Location,
		"birthday":       detailed.Birthday,
		"lang":           detailed.Lang,
		"fields":         detailed.Fields,
		"verifiedLinks":  detailed.VerifiedLinks,
		"followersCount": detailed.FollowersCount,
		"followingCount": detailed.FollowingCount,
		"notesCount":     detailed.NotesCount,
		"uri":            detailed.URI,
		"url":            detailed.URL,
		"movedTo":        u.MovedToURI,
		"alsoKnownAs":    u.AlsoKnownAs,
		"updatedAt":      detailed.UpdatedAt,
		"lastFetchedAt":  nil,
		// MeDetailed (新規ユーザーなのでゼロ値が多い)
		"avatarId":                        u.AvatarID,
		"bannerId":                        u.BannerID,
		"followersVisibility":             detailed.FollowersVisibility,
		"followingVisibility":             detailed.FollowingVisibility,
		"chatScope":                       u.ChatScope,
		"canChat":                         true,
		"followedMessage":                 nil,
		"memo":                            nil,
		"moderationNote":                  nil,
		"hideOnlineStatus":                u.HideOnlineStatus,
		"isAdmin":                         isAdmin,
		"isModerator":                     isMod,
		"isDeleted":                       u.IsDeleted,
		"isExplorable":                    u.IsExplorable,
		"hasUnreadNotification":           false,
		"hasPendingReceivedFollowRequest": false,
		"hasUnreadAnnouncement":           false,
		"hasUnreadAntenna":                false,
		"hasUnreadChannel":                false,
		"hasUnreadMentions":               false,
		"hasUnreadSpecifiedNotes":         false,
		"hasUnreadChatMessages":           false,
		"unreadNotificationsCount":        0,
		"unreadAnnouncements":             []any{},
		"pinnedNoteIds":                   []string{},
		"pinnedNotes":                     []any{},
		"pinnedPageId":                    nil,
		"pinnedPage":                      nil,
		"policies":                        userPolicies,
		"roles":                           []any{},
		"securityKeysList":                []any{},
		"mutingNotificationTypes":         []any{},
		"notificationRecieveConfig":       map[string]any{},
		"emailNotificationTypes":          []string{"follow", "receiveFollowRequest"},
		"twoFactorEnabled":                false,
		"usePasswordLessLogin":            false,
		"securityKeys":                    false,
		"twoFactorBackupCodesStock":       "none",
		"autoAcceptFollowed":              true,
		"noCrawle":                        false,
		"preventAiLearning":               true,
		"alwaysMarkNsfw":                  false,
		"autoSensitive":                   false,
		"carefulBot":                      false,
		"injectFeaturedNote":              true,
		"receiveAnnouncementEmail":        true,
		"publicReactions":                 true,
		"loggedInDays":                    0,
		"achievements":                    []any{},
		// token (MeDetailed にない追加フィールド)
		"token": result.Token,
	}

	// createdAt は ID から復元
	if s, err := aidxCreatedAtString(h.idGen, u.ID); err == nil {
		out["createdAt"] = s
	}

	return c.JSON(http.StatusOK, out)
}

// ShowUser handles POST /api/admin/show-user.
func (h *Handler) ShowUser(c echo.Context) error {
	var req struct {
		UserID      string `json:"userId"`
		WithSignins *bool  `json:"withSignins"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		// **DB 障害を「そんな利用者は居ない」にしない** (#2792)。障害を 404 で
		// 返すと「対象が消えた」と読めてしまい、監視でも 5xx が立たない。
		if !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
	}

	// 非 administrator (= moderator) は他 administrator の情報を閲覧できない
	// (upstream show-user.ts:223-226 の 'cannot show info of admin' guard)。
	//
	// **対象側は判定できないときに通さない (#3037 レビュー 2 周目)。**
	// `IsAdministrator` は判定できないときに false を返すので、素で使うと
	// `ListByUser` が一時的に失敗する窓で**管理者の email / 2FA / サインイン
	// 履歴が見える** (#2792)。実行者側は false = 「管理者ではない」に倒れる
	// ので、そちらは元から安全側。
	me := middleware.GetUser(c)
	if h.roleService != nil {
		if me != nil && !h.roleService.IsAdministrator(me.ID) {
			// **root も同じ理由で `targetIsRoot` を通す (レビュー 3 周目)。**
			switch isRoot, undet := h.targetIsRoot(user); {
			case undet:
				return apierr.JSONInternalError(c)
			case isRoot:
				return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot show info of admin.", "0d4e3a3e-2c1f-4d8b-9d2a-7a0c1c1b2f3a"))
			}
			targetAdmin, _, err := h.roleService.RolePrivileges(user.ID)
			if err != nil {
				slog.Error("admin/show-user: cannot determine the target's privileges", "userId", user.ID, "err", err)
				return apierr.JSONInternalError(c)
			}
			if targetAdmin {
				return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot show info of admin.", "0d4e3a3e-2c1f-4d8b-9d2a-7a0c1c1b2f3a"))
			}
		}
	}

	profile, _ := h.userRepo.FindProfileByUserID(user.ID)

	// **IP を出してよいのは `canSearchIpHistory` を持つ相手だけ** (#3114)。
	// `HasRolePolicy` は管理者を短絡するので、既定 (policy false) の構成では
	// 管理者だけが通る = `admin/ip/*` と同じ条件になる。
	//
	// **roleService 未配線なら伏せる側へ倒す** (fail-closed。この packer の
	// 他の field と同じ方針)。
	showIPs := h.roleService != nil && me != nil &&
		h.roleService.HasRolePolicy(me.ID, role.PolicyCanSearchIPHistory)
	if h.roleService == nil {
		// **黙って伏せない。** 配線が落ちると IP が永久に空になるだけで
		// 誰も気付けない (`middleware/role_policy.go` も同じ状況で鳴らす)。
		slog.Error("admin/show-user: roleService is not wired; signin IPs are withheld")
	}

	// Omitted keeps the upstream-compatible response shape and behaviour for
	// existing clients. The bundled control-panel page (fork 2026.10.0-mk.1 and
	// later) always opts out and loads admin/get-user-ips lazily (admins only)
	// when its IP folder is opened, so merely viewing the user does not read or
	// audit IPs.
	withSignins := req.WithSignins == nil || *req.WithSignins
	resp := h.packAdminUser(user, profile, showIPs, withSignins)
	// **内部連絡用のキーは wire に出さない。** `signins` を引けたかどうかは
	// 監査の判断にだけ使う。
	signinsOK, _ := resp[signinsLoadedKey].(bool)
	delete(resp, signinsLoadedKey)
	if showIPs && withSignins {
		// **実際に IP を返したときだけ記録する** (#3114 / #3106)。伏せた応答も、
		// **引けずに空になった応答も**開示が起きていないので残さない — 記録すると
		// 「本当に 0 件だった」と「DB が落ちていて何も返していない」が
		// `ip_lookup_log` 上で区別できなくなる (`admin/get-user-ips` と同じ判断)。
		if signinsOK && me != nil && h.ipLookupAudit != nil {
			signins, _ := resp["signins"].([]map[string]any)
			h.ipLookupAudit.Record(iplookuplog.Entry{
				UserID: me.ID, Kind: model.IPLookupKindSignins,
				TargetUserID: user.ID, ResultCount: len(signins),
			})
		}
	}
	// admin/show-user also contains other private moderation fields. Preserve
	// the existing cache protection for authorized viewers even when this
	// request intentionally omitted sign-in rows.
	if showIPs {
		noStoreIPLookup(c)
	}
	return c.JSON(http.StatusOK, resp)
}

// ShowUsers handles POST /api/admin/show-users.
func (h *Handler) ShowUsers(c echo.Context) error {
	var req struct {
		State    string     `json:"state"`
		Origin   string     `json:"origin"`
		Hostname string     `json:"hostname"`
		Username string     `json:"username"`
		Sort     string     `json:"sort"`
		Limit    *jsnum.Int `json:"limit"`
		Offset   *jsnum.Int `json:"offset"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	offset := 0
	if p := req.Offset.IntPtr(); p != nil {
		offset = *p
	}
	users, err := h.userRepo.ListUsers(model.UserListFilter{
		State:    req.State,
		Origin:   req.Origin,
		Hostname: req.Hostname,
		Username: req.Username,
		Sort:     req.Sort,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// Profile を per-row FindProfileByUserID で引いていた N+1 (#300 1-4) を
	// FindProfilesByUserIDs (#503 で追加済) の 1 batch query に置換。
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	profiles, err := h.userRepo.FindProfilesByUserIDs(ids)
	if err != nil {
		// Profile fetch 失敗は ShowUsers 全体の致命扱いではないので、空 map で
		// fallback する (各 user は profile=nil で pack される)。
		profiles = nil
	}
	profileByUser := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		profileByUser[p.UserID] = p
	}

	// upstream show-users.ts:117 は packMany(users, me, {schema:'UserDetailed'})
	// で UserDetailed を返す。admin detail packer (packAdminUser) を使うと
	// email / signins / roleAssigns / notificationRecieveConfig といった admin
	// 専用 field が一覧経由で漏れ、show-user の admin guard を迂回できてしまう
	// (moderator が admin の連絡先や signin IP を取得可能)。一覧は UserDetailed
	// に揃えて過剰露出を防ぐ。
	//
	// 本家と同じく packMany(users, me) で組む。モデレーターにはカウントと
	// moderationNote・2FA の項目を見せ、関係・ピン留め・移行先・instance・絵文字も
	// まとめて埋める。自分自身は MeDetailed になる (#3330)。
	ctx := c.Request().Context()
	viewer := middleware.GetUser(c)
	packed := h.packUsersMany(ctx, viewer, users, profileByUser)
	result := make([]any, 0, len(users))
	for i, u := range users {
		result = append(result, h.selfPacker.Pack(ctx, packed[i], u, profileByUser[u.ID], viewer))
	}
	return c.JSON(http.StatusOK, result)
}

// badgeRolesForMap returns badgeRoles for admin map-based responses. The packer
// now returns *[]any (nil = absent for remote users with
// showRoleBadgesOfRemoteUsers off, #1653), but admin endpoints serialise via
// map[string]any which ignores omitempty — a nil pointer would emit
// schema-invalid `null` (UserLite.badgeRoles is nullable:false). Coerce nil to
// `[]` so the admin response keeps its previous `[]` shape.
func badgeRolesForMap(br *[]any) []any {
	if br == nil {
		return []any{}
	}
	return *br
}

// signinsLoadedKey は packAdminUser が「signins を引けたか」を呼び出し元へ
// 渡すための内部キー。**wire へ出す前に必ず delete する。**
const signinsLoadedKey = "__signinsLoaded"

// packAdminUser returns a MeDetailed-equivalent response for admin endpoints.
func (h *Handler) packAdminUser(u *model.User, profile *model.UserProfile, showIPs, withSignins bool) map[string]any {
	// upstream admin/show-user (show-user.ts:233-261) が返すのはこの 24 key
	// だけで、UserLite / UserDetailed / MeDetailed は含まない。旧実装は
	// PackUserDetailed をベースに 70 key 近くを返しており、id をはじめ
	// upstream に無い field を多数含んでいた (#2287)。frontend の
	// admin-user.vue は users/show の結果を別途持っていて、こちらからは
	// email / lastActiveDate / policies / roleAssigns / roles / isModerator /
	// isSilenced / isSuspended / moderationNote しか読まないので、upstream の
	// key 集合に絞っても UI は壊れない。
	resp := map[string]any{
		"isSuspended":  u.IsSuspended,
		"isHibernated": u.IsHibernated,
		// roleService 未配線時は false / 既定 policy に倒す (fail-closed)。
		"isModerator": false,
		"isSilenced":  false,
		"policies":    role.DefaultPolicies(),
		"roles":       []any{},
		// profile 未取得時でも key は必ず生やす (upstream は必ず返す)。
		"email":                     nil,
		"emailVerified":             false,
		"followedMessage":           nil,
		"autoAcceptFollowed":        false,
		"noCrawle":                  false,
		"preventAiLearning":         false,
		"alwaysMarkNsfw":            false,
		"autoSensitive":             false,
		"carefulBot":                false,
		"injectFeaturedNote":        false,
		"receiveAnnouncementEmail":  false,
		"mutedWords":                []any{},
		"mutedInstances":            []any{},
		"notificationRecieveConfig": map[string]any{},
		"moderationNote":            "",
	}
	if profile != nil {
		resp["email"] = profile.Email
		resp["emailVerified"] = profile.EmailVerified
		resp["autoAcceptFollowed"] = profile.AutoAcceptFollowed
		resp["noCrawle"] = profile.NoCrawle
		resp["preventAiLearning"] = profile.PreventAiLearning
		resp["alwaysMarkNsfw"] = profile.AlwaysMarkNsfw
		resp["autoSensitive"] = profile.AutoSensitive
		resp["carefulBot"] = profile.CarefulBot
		resp["injectFeaturedNote"] = profile.InjectFeaturedNote
		resp["receiveAnnouncementEmail"] = profile.ReceiveAnnouncementEmail
		resp["mutedWords"] = profile.MutedWords
		resp["mutedInstances"] = profile.MutedInstances
		resp["followedMessage"] = profile.FollowedMessage
		if profile.ModerationNote != nil {
			resp["moderationNote"] = *profile.ModerationNote
		}
		resp["notificationRecieveConfig"] = metaJSONValue(profile.NotificationRecieveConfig, map[string]any{})
	}
	if h.roleService != nil {
		resp["isModerator"] = h.roleService.IsModerator(u.ID)
		// isSilenced は role policy 由来 (canPublicNote を否定する role を持つか)。
		resp["isSilenced"] = h.roleService.IsSilenced(u.ID)
		// GetUserRoles は expired 除外済みの active role list。err 時は
		// frontend の `.map(...)` が例外を吐かないよう空配列に倒して観測する。
		if userRoles, rerr := h.roleService.GetUserRoles(u.ID); rerr == nil {
			resp["roles"] = userRoles
		} else {
			slog.Warn("admin/show-user: failed to load roles", "userId", u.ID, "err", rerr)
			resp["roles"] = []any{}
		}
		resp["policies"] = h.roleService.GetUserPolicies(u.ID)
	}
	// signins / roleAssigns は repo / service 未配線や lookup 失敗時も
	// 空配列に fallback する (roles と同じ扱い、#888 / #1198)。
	// withSignins=false は repository 自体を読まない。読み終えてから捨てる
	// 実装では「IP 欄を開いた時だけ照会する」という境界にならない。
	resp["signins"] = []map[string]any{}
	resp[signinsLoadedKey] = false
	if withSignins {
		signins, signinsOK := h.packUserSignins(u.ID, showIPs)
		resp["signins"] = signins
		resp[signinsLoadedKey] = signinsOK
	}
	resp["roleAssigns"] = h.packUserRoleAssigns(u.ID)
	if u.LastActiveDate != nil {
		resp["lastActiveDate"] = u.LastActiveDate.UTC().Format("2006-01-02T15:04:05.000Z")
	} else {
		resp["lastActiveDate"] = nil
	}
	// **Elythia 独自の項目 (#3468)。** そのアカウントを管理しているプラグインの
	// 名前。普通のアカウントは null。本家の応答に項目を足すだけなので、本家の
	// クライアントは読み飛ばす。
	resp["managedByPlugin"] = u.ManagedByPlugin
	return resp
}

// packUserSignins returns the user's signin history in the admin/show-user
// `signins` shape. Upstream returns every row (signinsRepository.findBy);
// we pass limit=-1 so GORM omits the LIMIT clause and matches that. Rows come
// back newest-first (ListByUserID's default order); upstream leaves the order
// unspecified so this is a benign deviation. Returns an empty slice (never
// nil) so the JSON field is always `[]` for callers without a wired signin
// repository or on lookup failure.
//
// **showIPs が false なら `ip` を伏せる (#3114)。** ここは `signin` テーブルの
// ログイン IP をそのまま返す口で、**`admin/ip/*` が要求する `canSearchIpHistory`
// を通らない**。#3104 が「IP とアカウントの対応は既定でモデレーターに開かない」
// と決めた以上、同じ種類の情報をモデレーター権限だけで全件返すのは、mk-go が
// 自分で作った権限境界と食い違う。**upstream からの意図的な逸脱**
// (docs/divergence/security.md §7)。
//
// **空文字にする。** `Signin` の json-schema は `ip` を
// `optional: false, nullable: false` と宣言しているので、key を消すことも
// null にすることもできない。
func (h *Handler) packUserSignins(userID string, showIPs bool) ([]map[string]any, bool) {
	out := []map[string]any{}
	if h.signinRepo == nil {
		return out, false
	}
	signins, err := h.signinRepo.ListByUserID(userID, -1, "", "")
	if err != nil {
		slog.Warn("admin/show-user: failed to load signins", "userId", userID, "err", err)
		return out, false
	}
	for _, s := range signins {
		packed := entity.PackSignin(s, h.idGen)
		if packed == nil {
			continue
		}
		if !showIPs {
			// **`PackSignin` 側は触らない。** あれは本人向けの main stream の
			// `signin` イベントでも使う (`api/signin/handler.go`)。自分の IP を
			// 自分が見るのは正当なので、伏せるのは admin の経路だけ。
			packed["ip"] = ""
			// **`headers` も伏せる。** ここには保存時の HTTP header がそのまま
			// 入っており、**本番構成の nginx が必ず付ける `X-Real-IP` /
			// `X-Forwarded-For`** が含まれる (`deploy/uds/nginx/mkgo.conf`)。
			// `ip` だけ潰しても**キー 1 つ隣で同じ IP が読める**ので、伏せる
			// 意味が無くなる (敵対的レビューで実測)。`sanitizeHeaders` が落とす
			// のは `Authorization` / `Cookie` / `Set-Cookie` の 3 つだけ。
			//
			// golden schema は `headers` を `type: other` / 非 null / 必須と
			// 宣言しているので、`ip` と同じく**空の object** に置き換える。
			packed["headers"] = map[string]any{}
		}
		out = append(out, packed)
	}
	return out, true
}

// packUserRoleAssigns returns the user's active role assignments in the
// admin/show-user `roleAssigns` shape ({createdAt, expiresAt, roleId}).
// createdAt is derived from the aidx-encoded assignment ID. Mirrors upstream
// which maps roleService.getUserAssigns (expired assignments excluded).
func (h *Handler) packUserRoleAssigns(userID string) []map[string]any {
	out := []map[string]any{}
	if h.roleService == nil {
		return out
	}
	assigns, err := h.roleService.GetUserAssigns(userID)
	if err != nil {
		slog.Warn("admin/show-user: failed to load role assignments", "userId", userID, "err", err)
		return out
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	for _, a := range assigns {
		entry := map[string]any{
			"roleId":    a.RoleID,
			"expiresAt": nil,
		}
		if t, perr := h.idGen.ParseTime(a.ID); perr == nil {
			entry["createdAt"] = t.UTC().Format(tsFormat)
		}
		if a.ExpiresAt != nil {
			entry["expiresAt"] = a.ExpiresAt.UTC().Format(tsFormat)
		}
		out = append(out, entry)
	}
	return out
}

// SuspendUser handles POST /api/admin/suspend-user.
func (h *Handler) SuspendUser(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		// **DB 障害を「そんな利用者は居ない」にしない** (#2792)。障害を 404 で
		// 返すと「対象が消えた」と読めてしまい、監視でも 5xx が立たない。
		if !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
	}

	// upstream suspend-user.ts: モデレーター/管理者 (root を含む) アカウントは
	// 凍結できない。moderator が他の moderator/admin を凍結する権限昇格を防ぐ。
	//
	// **判定できないときは通さない (#3037 レビュー 2 周目)。** `IsModerator` は
	// 判定できないときに false を返すので、素で使うと `ListByUser` が一時的に
	// 失敗する窓で**他のモデレーターを凍結できる** (#2792)。
	//
	// **root は `targetIsRoot` で見る (#3037 レビュー 3 周目)。** `user.IsRoot`
	// だけでは足りない — `isRoot` 列が入った migration より前に作られた root は
	// `false` のままで、**本番の root がまさにそれ**。`RolePrivileges` が内部で
	// 呼ぶ `isRootUser` は meta の失敗を握り潰して `isRoot` へ落ちるので、
	// meta を読めない窓では `(false, false, nil)` = 「特権なし」を返し、
	// **モデレーターが root を凍結できた**。資格情報のリセット側と同じ判定へ揃える。
	switch isRoot, undet := h.targetIsRoot(user); {
	case undet:
		return apierr.JSONInternalError(c)
	case isRoot:
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot suspend a moderator account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}
	if h.roleService != nil {
		_, targetMod, err := h.roleService.RolePrivileges(user.ID)
		if err != nil {
			slog.Error("admin/suspend-user: cannot determine the target's privileges", "userId", user.ID, "err", err)
			return apierr.JSONInternalError(c)
		}
		if targetMod {
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot suspend a moderator account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		}
	}
	// **system アカウントは凍結させない。**
	//
	// `instance.actor` / `relay.actor` / `proxy.actor` を凍結すると、下の
	// `OnUserDeleted` が**既知の全リモート inbox へ `Delete(actor)` を配る**。
	// system アカウントはロールを持たないので `RolePrivileges` は
	// `(false, false, nil)` を返し、上の判定を素通りしていた。
	//
	// 削除経路 (`isProtectedAccount`) と資格情報リセット経路
	// (`isSystemAccountUser`) は既に塞いであり、**凍結経路だけが残っていた**。
	if isSystemAccountUser(user) {
		return c.JSON(http.StatusBadRequest,
			apierr.Error("ACCESS_DENIED", "Cannot suspend a system account.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}

	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"isSuspended": true}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **モデレーターの判断を local として刻む** (#2973)。これが無いと、リモート
	// actor が `toot:suspended` を下ろした時点で凍結が解除されてしまう。
	h.recordLocalSuspensionOrigin(req.UserID)
	// 凍結直後の auth bypass 防止 (#965)。target の全 token cache entry を
	// 即時削除し、middleware 通過後の P2 gate (#964) に依存せず確実に弾く。
	h.invalidateUserTokenCache(req.UserID)
	// 既に張られている WebSocket も閉じる (mk-go 独自、docs/divergence.md)。
	h.revokeUserStreams(req.UserID)
	// upstream UserSuspendService.suspend: local user なら全 sharedInbox へ
	// Delete(actor) を配信する (#1759)。best-effort (queue 経由)。
	if h.userModerationFed != nil {
		h.userModerationFed.OnUserDeleted(user)
	}
	// upstream postSuspend + unFollowAll: 双方向 followRequest 削除 + outgoing
	// follow の全 unfollow (#1759)。best-effort。
	h.cleanupSuspendedUserRelations(req.UserID)
	h.logUserActionWithUser(c, moderationlog.LogSuspend, user)
	return c.NoContent(http.StatusNoContent)
}

// UnsuspendUser handles POST /api/admin/unsuspend-user.
func (h *Handler) UnsuspendUser(c echo.Context) error {
	var req struct {
		UserID string `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.UserID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "userId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	user, err := h.userRepo.FindByID(req.UserID)
	if err != nil {
		// **DB 障害を「そんな利用者は居ない」にしない** (#2792)。障害を 404 で
		// 返すと「対象が消えた」と読めてしまい、監視でも 5xx が立たない。
		if !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.JSON(http.StatusNotFound, apierr.Error("NO_SUCH_USER", "No such user.", "2b730f78-1179-461b-88ad-d24c9af1a5ce"))
	}
	if user.IsDeleted {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Cannot unsuspend a deleted user.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
	}

	if err := h.userRepo.UpdateUser(req.UserID, map[string]any{"isSuspended": false}); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **解除も local として刻む** (#2973)。これが無いと、発信元が
	// `toot:suspended` を立て続けている限り次の actor refresh で無言で戻る。
	//
	// **ここは失敗を 500 で返す。** 凍結側と違い、記録できないまま解除すると
	// 「解除したはずが戻っている」という、この issue が塞ぎに来た状態そのものに
	// なる。resolver 側が「記録できないなら触らない」に倒しているのと揃える。
	if h.suspensionOriginRepo != nil {
		if err := h.suspensionOriginRepo.Set(req.UserID, model.SuspensionOriginLocal); err != nil {
			slog.Warn("admin: failed to record unsuspension origin", "userId", req.UserID, "err", err)
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
	}
	// 凍結解除直後に target の全 token cache entry を invalidate する (#965)。
	// cache 内に isSuspended=true な stale user が残っていると middleware の
	// P2 gate (#964) が cache hit 経路でも fire してしまい、解除済 user が
	// 認証通らない逆方向の bug になる。
	h.invalidateUserTokenCache(req.UserID)
	// upstream UserSuspendService.unsuspend: local user なら全 sharedInbox へ
	// Undo(Delete) を配信する (#1759)。
	if h.userModerationFed != nil {
		h.userModerationFed.OnUserRestored(user)
	}
	h.logUserActionWithUser(c, moderationlog.LogUnsuspend, user)
	return c.NoContent(http.StatusNoContent)
}
