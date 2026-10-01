package notes

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/core/timeline"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// TimelineRequest is the common request body for the four timeline endpoints.
type TimelineRequest struct {
	Limit                 *int   `json:"limit"`
	SinceID               string `json:"sinceId"`
	UntilID               string `json:"untilId"`
	SinceDate             *int64 `json:"sinceDate"`
	UntilDate             *int64 `json:"untilDate"`
	WithFiles             bool   `json:"withFiles"`
	WithRenotes           *bool  `json:"withRenotes"`
	WithReplies           *bool  `json:"withReplies"`
	IncludeMyRenotes      *bool  `json:"includeMyRenotes"`
	IncludeRenotedMyNotes *bool  `json:"includeRenotedMyNotes"`
	IncludeLocalRenotes   *bool  `json:"includeLocalRenotes"`
	AllowPartial          bool   `json:"allowPartial"`
}

// normalize validates limit against upstream の paramDef。ok=false は範囲外で、
// 呼び出し側は 400 INVALID_PARAM を返す。
func (r *TimelineRequest) normalize() bool {
	limit, ok := pagination.ResolveLimit(r.Limit, 10, 100)
	if !ok {
		return false
	}
	r.Limit = &limit
	return true
}

// timelineAvailable reports whether the timeline endpoint gated by the
// given policy key (= "ltlAvailable" / "gtlAvailable") is enabled for the
// current viewer. upstream Misskey TS handler は `getUserPolicies(me ?
// me.id : null)` で匿名でも base policies (DefaultPolicies + meta.policies
// merge) を返す pattern なので、mk-go も viewer が nil なら userID="" を
// 渡して base policies を引く (= core/role.Service.GetUserPolicies の
// 匿名経路と同 semantics、#1026)。
//
// policyProvider 未配線時は gate skip (test fixture / 旧挙動互換)。policy
// が bool true でない場合は **fail-closed で reject** する: upstream の
// `if (!policies.ltlAvailable)` 評価 (= undefined / false どちらも falsy
// 扱い) と挙動を揃える。production 経路では DefaultPolicies が常に bool を
// 返すのでこの差は発火しないが、厳密互換性のため fail-closed で揃える
// (#1038 review)。
func (h *Handler) timelineAvailable(c echo.Context, policyKey string) bool {
	if h.policyProvider == nil {
		return true
	}
	var userID string
	if viewer := middleware.GetUser(c); viewer != nil {
		userID = viewer.ID
	}
	policies := h.policyProvider.GetUserPolicies(userID)
	v, ok := policies[policyKey].(bool)
	return ok && v
}

// Timeline handles POST /api/notes/timeline (home timeline).
func (h *Handler) Timeline(c echo.Context) error {
	return h.serveTimeline(c, "home", func(viewer *model.User, req TimelineRequest) ([]*model.Note, error) {
		f := timeline.TimelineFilter{
			WithFiles:             req.WithFiles,
			WithRenotes:           req.WithRenotes,
			IncludeMyRenotes:      req.IncludeMyRenotes,
			IncludeRenotedMyNotes: req.IncludeRenotedMyNotes,
			IncludeLocalRenotes:   req.IncludeLocalRenotes,
			AllowPartial:          req.AllowPartial,
			MutedChannelIDs:       h.loadMutedChannelIDs(viewer),
			MutedUserIDs:          h.loadMutedUserIDs(viewer),
			RenoteMutedUserIDs:    h.loadRenoteMutedUserIDs(viewer),
			BlockerIDs:            h.loadBlockerIDs(viewer),
			MutedInstances:        h.loadMutedInstances(viewer),
			FollowedChannelIDs:    h.loadFollowedChannelIDs(viewer),
			FollowingIDs:          h.loadFollowingIDs(viewer),
		}
		return h.timelineService.HomeTimeline(c.Request().Context(), viewer, req.UntilID, req.SinceID, *req.Limit, f)
	}, true)
}

// LocalTimeline handles POST /api/notes/local-timeline.
func (h *Handler) LocalTimeline(c echo.Context) error {
	if !h.timelineAvailable(c, role.PolicyLtlAvailable) {
		return apierr.JSONLtlDisabled(c)
	}
	return h.serveTimeline(c, "local", func(viewer *model.User, req TimelineRequest) ([]*model.Note, error) {
		f := timeline.TimelineFilter{
			WithFiles:          req.WithFiles,
			WithRenotes:        req.WithRenotes,
			WithReplies:        req.WithReplies,
			AllowPartial:       req.AllowPartial,
			MutedChannelIDs:    h.loadMutedChannelIDs(viewer),
			MutedUserIDs:       h.loadMutedUserIDs(viewer),
			RenoteMutedUserIDs: h.loadRenoteMutedUserIDs(viewer),
			BlockerIDs:         h.loadBlockerIDs(viewer),
			MutedInstances:     h.loadMutedInstances(viewer),
		}
		return h.timelineService.LocalTimeline(c.Request().Context(), viewer, req.UntilID, req.SinceID, *req.Limit, f)
	}, false)
}

// GlobalTimeline handles POST /api/notes/global-timeline.
func (h *Handler) GlobalTimeline(c echo.Context) error {
	if !h.timelineAvailable(c, role.PolicyGtlAvailable) {
		return apierr.JSONGtlDisabled(c)
	}
	return h.serveTimeline(c, "global", func(viewer *model.User, req TimelineRequest) ([]*model.Note, error) {
		f := timeline.TimelineFilter{
			WithFiles:          req.WithFiles,
			WithRenotes:        req.WithRenotes,
			AllowPartial:       req.AllowPartial,
			MutedChannelIDs:    h.loadMutedChannelIDs(viewer),
			MutedUserIDs:       h.loadMutedUserIDs(viewer),
			RenoteMutedUserIDs: h.loadRenoteMutedUserIDs(viewer),
			BlockerIDs:         h.loadBlockerIDs(viewer),
			MutedInstances:     h.loadMutedInstances(viewer),
		}
		return h.timelineService.GlobalTimeline(c.Request().Context(), viewer, req.UntilID, req.SinceID, *req.Limit, f)
	}, false)
}

// HybridTimeline handles POST /api/notes/hybrid-timeline.
func (h *Handler) HybridTimeline(c echo.Context) error {
	// upstream: hybrid-timeline は ltlAvailable で gate する (gtl ではなく
	// ltl 側 policy を見るのは「ローカルタイムライン + social の hybrid」だから)。
	// ただしエラーコードは STL_DISABLED (Social TimeLine) で local の LTL_DISABLED
	// とは別 UUID を返す (#1554、upstream hybrid-timeline.ts stlDisabled)。
	if !h.timelineAvailable(c, role.PolicyLtlAvailable) {
		return apierr.JSONStlDisabled(c)
	}
	return h.serveTimeline(c, "hybrid", func(viewer *model.User, req TimelineRequest) ([]*model.Note, error) {
		f := timeline.TimelineFilter{
			WithFiles:             req.WithFiles,
			WithRenotes:           req.WithRenotes,
			WithReplies:           req.WithReplies,
			IncludeMyRenotes:      req.IncludeMyRenotes,
			IncludeRenotedMyNotes: req.IncludeRenotedMyNotes,
			IncludeLocalRenotes:   req.IncludeLocalRenotes,
			AllowPartial:          req.AllowPartial,
			MutedChannelIDs:       h.loadMutedChannelIDs(viewer),
			MutedUserIDs:          h.loadMutedUserIDs(viewer),
			RenoteMutedUserIDs:    h.loadRenoteMutedUserIDs(viewer),
			BlockerIDs:            h.loadBlockerIDs(viewer),
			MutedInstances:        h.loadMutedInstances(viewer),
			// upstream hybrid-timeline も getFromDb で
			// `channelId IN (followingChannelIds) OR channelId IS NULL` を
			// 付ける。渡さないと DB 経路 (FTT 無効時・cache miss 時) で
			// フォロー中チャンネルの投稿が STL から丸ごと落ちる。
			FollowedChannelIDs: h.loadFollowedChannelIDs(viewer),
			FollowingIDs:       h.loadFollowingIDs(viewer),
		}
		return h.timelineService.HybridTimeline(c.Request().Context(), viewer, req.UntilID, req.SinceID, *req.Limit, f)
	}, true)
}

// loadMutedChannelIDs returns the channel IDs that viewer has muted. Returns
// nil for anonymous viewers or when the repo is not wired. On repository
// error the mute list is skipped (best-effort) and a warning is logged so
// operators can see silent filter failures.
func (h *Handler) loadMutedChannelIDs(viewer *model.User) []string {
	if viewer == nil || h.channelMutingRepo == nil {
		return nil
	}
	rows, err := h.channelMutingRepo.ListByUser(viewer.ID)
	if err != nil {
		slog.Warn("timeline: failed to load muted channels", "userId", viewer.ID, "err", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ChannelID)
	}
	return ids
}

// loadFollowedChannelIDs returns the channel IDs the viewer follows, excluding
// muted channels (#1686, upstream timeline.ts の followingChannelIds から
// mutingChannelIds を除く分岐)。home timeline でのみ使い、空なら home は
// channelId IS NULL の note のみ。anonymous viewer / repo 未配線 / 取得失敗時は
// nil (best-effort、warning ログ)。
func (h *Handler) loadFollowedChannelIDs(viewer *model.User) []string {
	if viewer == nil || h.channelFollowingRepo == nil {
		return nil
	}
	// 1 user の follow channel 数は通常少ないが、暴走防止に上限で打ち切る。
	const pageSize = 100
	const maxFollowedChannels = 500
	var followed []string
	for offset := 0; offset < maxFollowedChannels; offset += pageSize {
		rows, err := h.channelFollowingRepo.ListFollowed(viewer.ID, "", "", pageSize, offset)
		if err != nil {
			slog.Warn("timeline: failed to load followed channels", "userId", viewer.ID, "err", err)
			return nil
		}
		for _, f := range rows {
			followed = append(followed, f.FolloweeID)
		}
		if len(rows) < pageSize {
			break
		}
	}
	if len(followed) == 0 {
		return nil
	}
	muted := h.loadMutedChannelIDs(viewer)
	if len(muted) == 0 {
		return followed
	}
	mutedSet := make(map[string]struct{}, len(muted))
	for _, id := range muted {
		mutedSet[id] = struct{}{}
	}
	out := followed[:0]
	for _, id := range followed {
		if _, m := mutedSet[id]; !m {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// loadMutedUserIDs returns userIDs that viewer has muted (active mutes only,
// non-expired). nil for anonymous viewers or when the repo is not wired.
// repository error は best-effort で nil 扱いにし slog.Warn で観測する
// (= channel mute 同様の pattern、#874)。
func (h *Handler) loadMutedUserIDs(viewer *model.User) []string {
	if viewer == nil || h.mutingRepo == nil {
		return nil
	}
	ids, err := h.mutingRepo.ListMuteeIDs(viewer.ID)
	if err != nil {
		slog.Warn("timeline: failed to load muted users", "userId", viewer.ID, "err", err)
		return nil
	}
	return ids
}

// loadRenoteMutedUserIDs returns userIDs that viewer has renote-muted.
// nil for anonymous viewers or when the repo is not wired (#903)。
// MutedUserIDs と異なり投稿者の plain note は除外せず、pure renote のみ
// filter する (logic は ApplyFilter / applyTimelineFilter 側で実装)。
func (h *Handler) loadRenoteMutedUserIDs(viewer *model.User) []string {
	if viewer == nil || h.renoteMutingRepo == nil {
		return nil
	}
	ids, err := h.renoteMutingRepo.ListMuteeIDs(viewer.ID)
	if err != nil {
		slog.Warn("timeline: failed to load renote-muted users", "userId", viewer.ID, "err", err)
		return nil
	}
	return ids
}

// loadBlockerIDs returns the ids of users who block the viewer (被block、#1681)。
// timeline で note/reply/renote の author が viewer を block していれば除外する
// (upstream generateBlockedUserQueryForNotes)。
// loadFollowingIDs returns the set of users the viewer follows.
//
// upstream timeline.ts は HTL の noteFilter で followings を引き、「返信先が
// followers 限定の投稿で、その投稿者をフォローしていない」ノートを弾く。
// fanout 側にも同等のガードはあるが DB fallback には効かないので、取得側でも
// 同じ判定ができるようにする。
func (h *Handler) loadFollowingIDs(viewer *model.User) map[string]struct{} {
	if viewer == nil || h.userFollowingRepo == nil {
		return nil
	}
	ids, err := h.userFollowingRepo.ListFolloweeIDs(viewer.ID)
	if err != nil {
		slog.Warn("timeline: failed to load followings", "userId", viewer.ID, "err", err)
		return nil
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func (h *Handler) loadBlockerIDs(viewer *model.User) []string {
	if viewer == nil || h.blockingRepo == nil {
		return nil
	}
	ids, err := h.blockingRepo.ListBlockerIDs(viewer.ID)
	if err != nil {
		slog.Warn("timeline: failed to load blockers", "userId", viewer.ID, "err", err)
		return nil
	}
	return ids
}

// loadMutedInstances returns the viewer's muted instance hosts (lowercase、#1681)。
// user_profile.mutedInstances (jsonb host 配列) を parse する。profile 不在 /
// 破損 jsonb は空扱い (best-effort、timeline を閉塞させない)。
func (h *Handler) loadMutedInstances(viewer *model.User) []string {
	if viewer == nil || h.userRepo == nil {
		return nil
	}
	profile, err := h.userRepo.FindProfileByUserID(viewer.ID)
	if err != nil || profile == nil || len(profile.MutedInstances) == 0 {
		return nil
	}
	var hosts []string
	if err := json.Unmarshal(profile.MutedInstances, &hosts); err != nil {
		slog.Warn("timeline: failed to parse muted instances", "userId", viewer.ID, "err", err)
		return nil
	}
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if host != "" {
			out = append(out, strings.ToLower(host))
		}
	}
	return out
}

// serveTimeline factors out the common parsing and error handling for the four
// timeline endpoints. requireAuthがtrueのときviewer==nilで401相当を返す。
func (h *Handler) serveTimeline(
	c echo.Context,
	kind string,
	fn func(*model.User, TimelineRequest) ([]*model.Note, error),
	requireAuth bool,
) error {
	var req TimelineRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if !req.normalize() {
		return apierr.JSONInvalidParam(c)
	}

	// upstream local/hybrid-timeline は withReplies && withFiles 同時指定を
	// BOTH_WITH_REPLIES_AND_WITH_FILES で弾く (endpoint 固有 UUID、#1554)。
	// timeline(home)/global は upstream に該当エラーが無いので対象外。
	if req.WithReplies != nil && *req.WithReplies && req.WithFiles {
		switch kind {
		case "local":
			return c.JSON(http.StatusBadRequest, apierr.Error("BOTH_WITH_REPLIES_AND_WITH_FILES", "Specifying both withReplies and withFiles is not supported", "dd9c8400-1cb5-4eef-8a31-200c5f933793"))
		case "hybrid":
			return c.JSON(http.StatusBadRequest, apierr.Error("BOTH_WITH_REPLIES_AND_WITH_FILES", "Specifying both withReplies and withFiles is not supported", "dfaa3eb7-8002-4cb7-bcc4-1095df46656f"))
		}
	}

	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。旧実装は
	// idGen.Generate(time) で完全 ID (= time prefix + random nodeID + counter)
	// を生成していたが、SQL `id > Generate(time)` 比較では同 msec の早期 ID
	// を取りこぼすバグがあった。AidxCutoffPrefix (= time prefix + "00000000")
	// で deterministic + 同 msec 内全 ID を含む正しい cutoff に修正。
	cursorSince, cursorUntil, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	req.SinceID, req.UntilID = cursorSince, cursorUntil

	viewer := middleware.GetUser(c)
	if requireAuth && viewer == nil {
		return c.JSON(http.StatusUnauthorized, apierr.Error("CREDENTIAL_REQUIRED", "Credential required.", "1384574d-a912-4b81-8601-c7b1c4085df1"))
	}

	// UGC visibility: 未ログインユーザーの閲覧を制限する (meta.ugcVisibilityForVisitor)。
	// "none" → 空リスト、"local" → local timeline のみ許可 (global はブロック)。
	if viewer == nil && h.ugcVisibilityNow() == "none" {
		return c.JSON(http.StatusOK, []any{})
	}

	// experiment: first-page (cursor 無し) のみ JSON cache を引く。hit 時は DB +
	// pack + encode を丸ごとスキップ。cache 無効時 / cursor 付きは cacheKey="" で
	// 従来通り c.JSON 経路を通る (= default 挙動は byte 一致のまま)。
	var cacheKey string
	if h.timelineCache != nil && req.SinceID == "" && req.UntilID == "" {
		vid := "anon"
		if viewer != nil {
			vid = viewer.ID
		}
		cacheKey = timelineCacheKey(kind, vid, req)
		if body, ok := h.timelineCache.get(cacheKey, time.Now()); ok {
			return c.JSONBlob(http.StatusOK, body)
		}
	}

	notes, err := fn(viewer, req)
	if err != nil {
		// requireAuthでviewer nilチェックは事前に行っているので、Service層からの
		// ErrUnauthenticatedはここには到達しない。残りはRedis等の障害のみ。
		return apierr.JSONInternalError(c)
	}
	// リノート先まで辿る mute / block 判定を最後に通す。SQL と
	// timeline.ApplyFilter は note 自身の userId / replyUserId / renoteUserId しか
	// 見ないため、「ミュート相手への返信」を第三者がリノートしたケースを取り
	// こぼす。upstream FanoutTimelineEndpointService は note と note.renote の
	// 両方に isUserRelated を適用しており (#2345 の調査で判明)、mk-go でも
	// 同じ判定を持つ ApplyMuteBlockChannel を timeline 経路に通して揃える。
	if notes, err = h.applyMuteBlock(viewer, notes); err != nil {
		return apierr.JSONInternalError(c)
	}
	packed := h.packMany(c.Request().Context(), notes, viewer)
	if cacheKey != "" {
		if body, mErr := json.Marshal(packed); mErr == nil {
			// echo の c.JSON は json.Encoder 経由で末尾に改行を付ける。cache 経路も
			// 同じ byte 列にして default (c.JSON) 経路と応答を一致させる。
			body = append(body, '\n')
			h.timelineCache.set(cacheKey, body, time.Now())
			return c.JSONBlob(http.StatusOK, body)
		}
		// marshal 失敗時は通常の c.JSON 経路へ fallthrough。
	}
	return c.JSON(http.StatusOK, packed)
}
