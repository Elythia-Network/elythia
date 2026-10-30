package i

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Me handles POST /api/i - returns the authenticated user's info.
//
// MeDetailed (UserLite + UserDetailed + 11 self-view + notification 3) は
// (*entity.Packer).PackMeDetailed 経由で一括展開し、`/api/i` 固有の private field
// (email / mutedWords / twoFactor* / clientData / role / unread 等) を
// resp map に merge する design (#971)。MeDetailed packer に新 field が
// 追加されたとき自動追従する。
// recordLoginDay appends today's date to the profile's loggedInDates when not
// already present and persists it, mirroring upstream i.ts. The date format is
// server-local `YYYY/M/D` (no zero-padding, 1-indexed month) to match
// `${getFullYear()}/${getMonth()+1}/${getDate()}`. Updates profile in place so
// the response's loggedInDays reflects the new count. Best-effort.
func (h *Handler) recordLoginDay(userID string, profile *model.UserProfile) {
	now := time.Now()
	today := fmt.Sprintf("%d/%d/%d", now.Year(), int(now.Month()), now.Day())
	for _, d := range profile.LoggedInDates {
		if d == today {
			return
		}
	}
	newDates := append(append(model.StringArray{}, profile.LoggedInDates...), today)
	if err := h.userService.UpdateProfileFields(userID, map[string]any{"loggedInDates": newDates}); err != nil {
		slog.Warn("i: record login day failed", "user", userID, "err", err)
		return
	}
	profile.LoggedInDates = newDates
}

func (h *Handler) Me(c echo.Context) error {
	return h.me(c, true)
}

// me packs the MeDetailed response. allowSecrets=false forces the
// includeSecrets ブロック (email / emailVerified / securityKeysList) を落とす。
// upstream の i/pin / i/unpin は `pack(me, me, {schema: 'MeDetailed'})` を
// includeSecrets 無しで呼ぶので、同じ応答を使い回す mk-go 側もここで落とす。
func (h *Handler) me(c echo.Context, allowSecrets bool) error {
	u := middleware.GetUser(c)

	// middleware が渡す user は tokenCache (token → user、30 秒 TTL) 由来で、
	// フォローや投稿による followingCount / notesCount の更新に追随しない。
	// upstream にこのキャッシュは無く、/api/i は常に最新のカウントを返す。
	//
	// /api/i は自分のプロフィールそのものを返す endpoint で per-session 数回の
	// 頻度なので、repository から取り直して差を埋める。CachedUserRepository は
	// Increment*Count で invalidate されるのでここでは fresh な行が返る。
	if fresh, err := h.userService.ShowByID(u.ID); err == nil && fresh != nil && fresh.User != nil {
		u = fresh.User
	}

	profile := h.userService.GetProfile(u.ID)

	// upstream i.ts:67-72: /api/i アクセスごとに当日を loggedInDates に追記する
	// (loggedInDays と login-streak achievement の唯一の source、#1767)。
	// best-effort: 永続化失敗でも応答は返す (upstream も update を await しない)。
	if profile != nil {
		h.recordLoginDay(u.ID, profile)
	}

	// MeDetailed を JSON round-trip で map に展開して base にする。
	// 1 request あたり Marshal + Unmarshal が 1 回ずつ走るが、/api/i は
	// per-session 数回の頻度なので影響は無視できる。順序保証は不要
	// (JSON object は順序不定)。
	//
	// errors are infeasible: MeDetailed struct は JSON-safe な primitive と
	// slice / map のみで構成され、`any` 型 field を含まない。万一将来 non-
	// marshalable な field が追加されても、(*json.UnsupportedTypeError) は
	// CI の TestMe_PreservesMeDetailedFields で即検出される (= empty resp
	// で field 不在 assertion が落ちる)。silent leak には繋がらない。
	me := h.entityPacker.PackMeDetailed(u, profile, h.idGen)
	// movedTo / alsoKnownAs を URI→ローカルID 解決して埋める (#1255)。self
	// view も他 path と同じ shape にするため marshal 前に enrich する。
	me.ResolveMoveTargets(u, h.resolveUserIDByURI)
	b, _ := json.Marshal(me)
	resp := map[string]any{}
	_ = json.Unmarshal(b, &resp)

	// MeDetailed packer に乗っていない /api/i 固有 field を上書き / 追加。
	// avatarId / bannerId / chatScope は PackUserDetailed 経由で同 value が
	// 既に乗っているので override 不要 (#987 review で重複削除)。
	// movedTo / alsoKnownAs / lastFetchedAt は PackUserDetailed +
	// ResolveMoveTargets (上で実施) が golden 互換 shape (movedTo: string|null /
	// alsoKnownAs: string[]|null / lastFetchedAt: string|null) で乗せるので
	// override しない。旧 override は alsoKnownAs を comma-joined string のまま
	// 出していて golden 非互換だった。
	// canChat は PackMeDetailed → UserLite 経由で role policy
	// chatAvailability === 'available' を見るようになった (#988)。
	// 旧 self-view 用 hardcode `resp["canChat"] = true` を撤去し、
	// upstream と同じ role policy ベースの判定に揃える。
	// memo / moderationNote は UserDetailed.Memo (omitempty) / MeDetailed
	// memo は PackMeDetailed (UserDetailed.Memo, #1259) が null で乗せるので
	// override 不要。moderationNote は golden で `moderationNote?: string`
	// (optional・null 不可、moderator が他者を見るとき専用) なので self-view
	// (/api/i) では出さない。旧実装は `resp["moderationNote"]=nil` で null を出し
	// golden 非互換だった (#1270 L3 で検出)。
	// isSilenced は role policy 由来 (MeDetailed/UserDetailedNotMe 共通 field)。
	// PackUserDetailed も Packer の silenced lookup で同値を埋めるが、/api/i は
	// handler 層の roleProvider を権威ソースとして明示的に上書きする (両者とも
	// 同一 roleService なので値は一致)。
	resp["isSilenced"] = h.isSilenced(u.ID)

	// isSecure: native login token (cookie / i param) のときだけ true。OAuth /
	// MiAuth の app access token では false (RequireSecure と同一判定)。upstream
	// i.ts の `isSecure = token == null` 相当で、email / emailVerified /
	// securityKeysList を includeSecrets ブロックとして gate する (#1824)。これが
	// 無いと read:account scope の app token だけで email・WebAuthn キー一覧が読めた。
	isSecure := allowSecrets && u.Token != nil && *u.Token == middleware.GetToken(c)

	// Private fields from profile (MeDetailed scope 外)
	if profile != nil {
		if isSecure {
			// email / emailVerified は includeSecrets 限定 (upstream
			// UserEntityService.ts:624-626)。app token には出さない。
			resp["email"] = profile.Email
			resp["emailVerified"] = profile.EmailVerified
		}
		resp["twoFactorEnabled"] = profile.TwoFactorEnabled
		resp["usePasswordLessLogin"] = profile.UsePasswordLessLogin
		// mutedWords / mutedInstances は PackMeDetailed (AsMeDetailed) が profile
		// から非null array で乗せる。raw profile 値で override すると空カラム時に
		// null になり golden (string[][] / string[]、非null) 非互換 (#1270 L3 検出)。
		resp["publicReactions"] = profile.PublicReactions
		resp["loggedInDays"] = len(profile.LoggedInDates)
		resp["achievements"] = jsonbArray(profile.Achievements)
		// upstream UserEntityService.ts:580 と同じく securityKeys は 2FA 有効時のみ
		// 真。PackMeDetailed も同 gate だが、ここで profile 値を override するため
		// 揃える (#1921)。
		resp["securityKeys"] = profile.TwoFactorEnabled && profile.SecurityKeysAvailable
		// hardMutedWords / twoFactorBackupCodesStock は PackMeDetailed (AsMeDetailed)
		// が profile から正しい shape で乗せるので override 不要 (#1258 follow-up)。
	}

	// フロントエンド互換性フィールド (Phase 4.5c / Phase 5)
	isAdmin, isMod, userPolicies, userRoles := h.rolePayload(u.ID)
	resp["isAdmin"] = isAdmin
	resp["isModerator"] = isMod
	// upstream UserEntityService.ts:574: moderationNote は moderator viewer のみに
	// emit する (空なら '')。/api/i は self-view なので viewer==self、self が moderator
	// のとき自身の profile.moderationNote を返す。非 moderator では field 自体を出さない
	// (upstream の undefined 相当、#2094)。
	if isMod && profile != nil {
		mn := ""
		if profile.ModerationNote != nil {
			mn = *profile.ModerationNote
		}
		resp["moderationNote"] = mn
	}
	// isDeleted / isExplorable は PackMeDetailed 由来で base 展開済 (#971)。
	// 未読系フィールドを依存する repo / service から実際に引く
	// (hasUnreadAntenna / hasUnreadChannel は antennaUnreadRepo /
	// channelUnreadRepo、hasUnreadSpecifiedNotes は notificationSvc.UnreadSummary
	// 経由で fillUnreadFields が populate する)。未wireのものは false/0/[] に
	// フォールバックする (テスト互換)。
	h.fillUnreadFields(c.Request().Context(), u, resp)
	h.fillPinnedFields(c.Request().Context(), u, profile, resp)
	resp["policies"] = userPolicies
	resp["roles"] = userRoles
	// securityKeysList: WebAuthnキーの一覧。includeSecrets 限定 (upstream
	// UserEntityService.ts:627-639) なので native session のときだけ出す。app token
	// では key 自体を省く (#1824、misskey-js golden で optional)。さらに upstream は
	// `twoFactorEnabled ? find() : []` で 2FA 有効時のみ実リストを返すため、無効時は
	// repo を引かず [] にする (#1921)。
	if isSecure {
		if profile != nil && profile.TwoFactorEnabled {
			resp["securityKeysList"] = h.securityKeysList(u.ID)
		} else {
			resp["securityKeysList"] = []any{}
		}
	}
	// MeDetailed の notification-related 3 field (#985) は PackMeDetailed 経由で
	// base 展開済 (#971 で集約)。ここで追加 override は不要。

	// createdAt は ID から復元
	if t, err := h.idGen.ParseTime(u.ID); err == nil {
		resp["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
	}

	return c.JSON(http.StatusOK, resp)
}

// rolePayload resolves the role-dependent MeDetailed fields (isAdmin /
// isModerator / policies / roles) for the given user.
//
// PackMeDetailed は role を知らないので、これらは handler 層で埋める必要がある。
// /api/i と /api/i/update の両方が同じ値を返さないと、フロントの
// updateCurrentAccount が `$i` に部分マージしたときに policies が
// 消えてしまう。
func (h *Handler) rolePayload(userID string) (isAdmin bool, isMod bool, policies map[string]any, roles []any) {
	policies = role.DefaultPolicies()
	if h.roleProvider != nil {
		isAdmin = h.roleProvider.IsAdministrator(userID)
		isMod = h.roleProvider.IsModerator(userID)
		policies = h.roleProvider.GetUserPolicies(userID)
		if rs, err := h.roleProvider.GetUserRoles(userID); err == nil {
			// 公開ロールだけを表示順に並べる (#3240)。本家 UserEntityService も
			// 本人かどうかに関係なく isPublic で絞る。絞らないと、運用上の分類や
			// モデレーション目的の非公開ロールの名前が、割り当てられた本人に見える。
			// 権限 (isAdmin / isModerator / policies) は非公開ロールも含めて上で
			// 計算済みなので変わらない。
			// iconUrl は entity (packPublicRoles、meUpdated の経路) と同じく media
			// proxy を通す。通さないと $i.roles[].iconUrl が経路ごとに変わる。
			visible := make([]*model.Role, 0, len(rs))
			for _, r := range rs {
				if r.IsPublic {
					visible = append(visible, r)
				}
			}
			sort.SliceStable(visible, func(i, j int) bool { return visible[i].DisplayOrder > visible[j].DisplayOrder })
			for _, r := range visible {
				roles = append(roles, map[string]any{
					"id":              r.ID,
					"name":            r.Name,
					"color":           r.Color,
					"iconUrl":         h.entityPacker.ProxyMediaURLPtr(r.IconURL),
					"description":     r.Description,
					"isModerator":     r.IsModerator,
					"isAdministrator": r.IsAdministrator,
					"displayOrder":    r.DisplayOrder,
				})
			}
		}
	}
	if roles == nil {
		roles = []any{}
	}
	return isAdmin, isMod, policies, roles
}

// meDetailedWithUnread packs a MeDetailed for self-view stream/return paths
// (i/update, meUpdated) and enriches it with authoritative unread values via
// fillUnreadFields. Raw PackMeDetailed would emit the struct defaults
// (unreadNotificationsCount:0 / unreadAnnouncements:[] / hasUnread*:false),
// which the frontend's updateCurrentAccount would merge into `$i`,
// clobbering the real badge/unread state. Going through fillUnreadFields keeps
// those fields correct on every path (the same shape /api/i returns).
func (h *Handler) meDetailedWithUnread(ctx context.Context, u *model.User, profile *model.UserProfile) map[string]any {
	me := h.entityPacker.PackMeDetailed(u, profile, h.idGen)
	b, _ := json.Marshal(me)
	resp := map[string]any{}
	_ = json.Unmarshal(b, &resp)
	h.fillUnreadFields(ctx, u, resp)
	// policies / roles はここでは足さない。この helper は meUpdated
	// (ストリーミングの部分更新) でも使われ、そこに policies を混ぜると
	// frontend の updateCurrentAccount が `$i.policies` を空で clobber
	// する (#1240 の不変条件、handler_webauthn_test.go が回帰ガード)。
	// API 応答である i/update 側は handler で明示的に足す。
	return resp
}

// appendIncludeSecrets adds the includeSecrets-gated MeDetailed fields (email /
// emailVerified / securityKeysList) to resp when the request is a native login
// session, mirroring upstream pack(..., {includeSecrets: isSecure}). isSecure =
// the request's raw token equals users.token (= RequireSecure と同一判定);
// OAuth/MiAuth の app token (token != users.token) では出さない。
//
// /api/i (Me) は同等の判定を inline で持つ (#1824)。/api/i/update もこの helper で
// native session に email 系を返す (#1919)。両者が drift しないよう、変更時は
// Me handler の isSecure ブロックと揃えること。
func (h *Handler) appendIncludeSecrets(c echo.Context, u *model.User, profile *model.UserProfile, resp map[string]any) {
	if u == nil || u.Token == nil || *u.Token != middleware.GetToken(c) {
		return
	}
	if profile != nil {
		resp["email"] = profile.Email
		resp["emailVerified"] = profile.EmailVerified
	}
	// upstream UserEntityService.ts:627 は `twoFactorEnabled ? find() : []` なので
	// 2FA 有効時のみ実リスト、無効時は [] (#1921)。Me handler の isSecure ブロックと
	// 同 gate を保つこと。
	if profile != nil && profile.TwoFactorEnabled {
		resp["securityKeysList"] = h.securityKeysList(u.ID)
	} else {
		resp["securityKeysList"] = []map[string]any{}
	}
}

// securityKeysList returns the user's WebAuthn key list for the includeSecrets
// block, returning [] when no repo is wired or no keys exist.
func (h *Handler) securityKeysList(userID string) []map[string]any {
	list := []map[string]any{}
	if h.securityKeyRepo == nil {
		return list
	}
	keys, err := h.securityKeyRepo.ListByUser(userID)
	if err != nil {
		return list
	}
	for _, k := range keys {
		list = append(list, map[string]any{
			"id":       k.ID,
			"name":     k.Name,
			"lastUsed": k.LastUsed.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	return list
}

// EnrichSelf implements meself.Enricher. 他 handler が self を MeDetailed へ
// 昇格させたとき、entity 層では出せない field (未読 / role policies / roles /
// pinned) をここで埋める。/api/i と同じ source を使うので値も一致する。
//
// includeSecrets 相当 (email / emailVerified / securityKeysList) は入れない。
// upstream も users/show や hashtags/users では includeSecrets を渡さない。
func (h *Handler) EnrichSelf(ctx context.Context, u *model.User, profile *model.UserProfile, resp map[string]any) {
	if u == nil || resp == nil {
		return
	}
	h.fillUnreadFields(ctx, u, resp)
	h.fillPinnedFields(ctx, u, profile, resp)
	isAdmin, isMod, policies, roles := h.rolePayload(u.ID)
	resp["isAdmin"] = isAdmin
	resp["isModerator"] = isMod
	resp["policies"] = policies
	resp["roles"] = roles
	resp["isSilenced"] = h.isSilenced(u.ID)
}

// fillUnreadFields writes the unread-related flags/counts onto resp.
// 依存が未wireのフィールドはdefault (false/0/[]) にフォールバックする。
func (h *Handler) fillUnreadFields(ctx context.Context, u *model.User, resp map[string]any) {
	// Defaults
	resp["hasUnreadNotification"] = false
	resp["unreadNotificationsCount"] = 0
	resp["hasUnreadMentions"] = false
	resp["hasPendingReceivedFollowRequest"] = false
	resp["hasUnreadAnnouncement"] = false
	resp["unreadAnnouncements"] = []any{}
	resp["hasUnreadChatMessages"] = false
	resp["hasUnreadAntenna"] = false
	resp["hasUnreadChannel"] = false
	resp["hasUnreadSpecifiedNotes"] = false

	// Notification (Redis Streams): 1 回の UnreadSummary で 3 値を集約する
	// (#321)。従来は UnreadCount / HasUnreadOfTypes / HasUnreadSpecifiedNotes
	// の 3 XRevRange scan に分かれていたが、hot path 最適化のため統合した。
	// err 時も summary に格納済みの部分結果は採用する (Redis 成功 + DB 失敗
	// で mentions 判定までは保持する partial resilience)。
	//
	// 表ごとに 1 本ずつ引くと note_unread を含めて 5 本になるので、まとめて
	// 引ける reader があれば先に 1 文で引く (#3608)。失敗したら nil。
	var flags *repository.MeUnreadFlags
	if h.unreadFlags != nil {
		flags = h.readUnreadFlags(u.ID)
	}
	if h.notificationSvc != nil {
		mentionTypes := []notification.Type{notification.TypeMention, notification.TypeReply}
		var (
			summary notification.UnreadSummary
			err     error
		)
		if stream, ok := h.notificationSvc.(UnreadStreamSource); ok && h.unreadFlags != nil && stream.SpecifiedNoteFromDB() {
			// note_unread は reader の 1 文で引いた。UnreadSummary は stream の
			// 読み取りに失敗すると note_unread を見ずに返す (false のまま) ので、
			// そのときは reader の値も使わない。
			summary, err = stream.UnreadStreamSummary(ctx, u.ID, mentionTypes)
			if err == nil && flags != nil {
				summary.HasSpecifiedNote = flags.HasUnreadSpecifiedNotes
			}
		} else {
			summary, err = h.notificationSvc.UnreadSummary(ctx, u.ID, mentionTypes)
		}
		if err != nil {
			slog.Warn("UnreadSummary partial failure", "userID", u.ID, "err", err)
		}
		resp["unreadNotificationsCount"] = summary.TotalCount
		resp["hasUnreadNotification"] = summary.TotalCount > 0
		resp["hasUnreadMentions"] = summary.HasMentions
		resp["hasUnreadSpecifiedNotes"] = summary.HasSpecifiedNote
	}

	// 出すかどうかは従来どおり各 repo の配線で決める。
	if h.unreadFlags != nil {
		if flags != nil {
			h.applyUnreadFlags(*flags, resp)
		}
	} else {
		h.fillUnreadFlagsEach(u.ID, resp)
	}

	// Announcement (DB)
	if h.announcementRepo != nil {
		if ann, err := h.announcementRepo.UnreadForUser(u.ID); err == nil {
			resp["hasUnreadAnnouncement"] = len(ann) > 0
			out := make([]map[string]any, 0, len(ann))
			for _, a := range ann {
				// PackAnnouncement で createdAt / updatedAt / icon / display 等を
				// 含む upstream 互換の shape に揃える。独自 map だと createdAt が
				// 欠落し、misskey_dart の AnnouncementsResponse.fromJson が
				// createdAt を非null String として cast して落ちる (#1224)。
				// #2106 L52: UnreadForUser は viewer u 宛て/global の announcement。viewerID=u.ID。
				packed := h.entityPacker.PackAnnouncement(a, h.idGen, false, u.ID)
				if packed == nil {
					// nil entry を配列に入れると [null] になり、misskey_dart が
					// 各要素を fromJson する際 null で落ちるため除外する。
					continue
				}
				out = append(out, packed)
			}
			resp["unreadAnnouncements"] = out
		}
	}

}

// readUnreadFlags reads the unread flags through h.unreadFlags, or returns
// nil when the read fails. 失敗したときは各項目を既定の false のままにする
// (表ごとに引く経路も、失敗した項目は false のままにしている)。
func (h *Handler) readUnreadFlags(userID string) *repository.MeUnreadFlags {
	f, err := h.unreadFlags.ReadMeUnreadFlags(userID)
	if err != nil {
		// 1 文なので全項目が同時に消える。障害の原因を追えるよう記録する。
		slog.Warn("i: unread flags read failed", "userID", userID, "err", err)
		return nil
	}
	return &f
}

// applyUnreadFlags writes the four yes/no unread fields of f whose
// repository is wired (hasUnreadSpecifiedNotes is written with the
// notification summary).
func (h *Handler) applyUnreadFlags(f repository.MeUnreadFlags, resp map[string]any) {
	if h.followRequestRepo != nil {
		resp["hasPendingReceivedFollowRequest"] = f.HasPendingReceivedFollowRequest
	}
	if h.chatRepo != nil {
		resp["hasUnreadChatMessages"] = f.HasUnreadChatMessages
	}
	if h.antennaUnreadRepo != nil {
		resp["hasUnreadAntenna"] = f.HasUnreadAntenna
	}
	if h.channelUnreadRepo != nil {
		resp["hasUnreadChannel"] = f.HasUnreadChannel
	}
}

// fillUnreadFlagsEach is fillUnreadFlags with one query per field, used when
// no MeUnreadFlagsReader is wired.
func (h *Handler) fillUnreadFlagsEach(userID string, resp map[string]any) {
	if h.followRequestRepo != nil {
		if n, err := h.followRequestRepo.CountReceived(userID); err == nil {
			resp["hasPendingReceivedFollowRequest"] = n > 0
		}
	}
	// chat_message は DB の表 (ChatRepository.CountUnread)。
	if h.chatRepo != nil {
		if n, err := h.chatRepo.CountUnread(userID); err == nil {
			resp["hasUnreadChatMessages"] = n > 0
		}
	}
	if h.antennaUnreadRepo != nil {
		if ok, err := h.antennaUnreadRepo.HasAnyByUser(userID); err == nil {
			resp["hasUnreadAntenna"] = ok
		}
	}
	if h.channelUnreadRepo != nil {
		if ok, err := h.channelUnreadRepo.HasAnyByUser(userID); err == nil {
			resp["hasUnreadChannel"] = ok
		}
	}
}

// fillPinnedFields populates pinnedNoteIds / pinnedNotes / pinnedPageId /
// pinnedPage onto resp using the wired repos. Missing repos fall back to
// default empty / nil (tests that skip wiring keep passing).
func (h *Handler) fillPinnedFields(ctx context.Context, u *model.User, profile *model.UserProfile, resp map[string]any) {
	// Defaults
	resp["pinnedNoteIds"] = []string{}
	resp["pinnedNotes"] = []any{}
	resp["pinnedPageId"] = nil
	resp["pinnedPage"] = nil

	// Pinned notes: user_note_pining の行から noteId 一覧を取り、note 本体を
	// NoteRepository で fetch して pack する。
	if h.piningRepo != nil {
		pinings, err := h.piningRepo.ListByUser(u.ID)
		if err == nil && len(pinings) > 0 {
			ids := make([]string, 0, len(pinings))
			for _, p := range pinings {
				ids = append(ids, p.NoteID)
			}
			resp["pinnedNoteIds"] = ids

			if h.noteRepo != nil {
				if notes, err := h.noteRepo.FindManyByIDsWithUser(ids); err == nil {
					entities := h.entityPacker.PackNotes(ctx, notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
					// /api/i は認証された自身を返す path なので u 自身を viewer
					// として渡し、pinned note の myReaction も含めて埋める (#426)。
					h.fieldRes.Apply(entities, u)
					h.noteHider.HideEmbeds(u, entities)
					packed := make([]any, 0, len(entities))
					for _, pn := range entities {
						packed = append(packed, pn)
					}
					resp["pinnedNotes"] = packed
				}
			}
		}
	}

	// Pinned page: user_profile.pinnedPageId からの展開。
	if profile != nil && profile.PinnedPageID != nil && *profile.PinnedPageID != "" {
		resp["pinnedPageId"] = profile.PinnedPageID
		if h.pageRepo != nil {
			if p, err := h.pageRepo.FindByID(*profile.PinnedPageID); err == nil {
				// golden Page は user 必須。pinnedPage は自分の page なので
				// owner=u を渡して user (UserLite) を埋める (#1266 follow-up)。
				resp["pinnedPage"] = h.entityPacker.PackPageWithContext(p, entity.PackPageContext{IDGen: h.idGen, Owner: u})
			}
		}
	}
}
