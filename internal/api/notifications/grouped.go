package notifications

import (
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Grouped handles POST /api/i/notifications-grouped.
//
// 本家 Misskey TS i/notifications-grouped.ts:107-155 の grouping ロジック準拠:
// 連続する同一 note への reaction を `reaction:grouped` (reactions[] 集約)、
// 連続する同一 target note への renote を `renote:grouped` (users[] 集約) に
// 畳み込んでから packGroupedMany 相当の shape で返す。それ以外の通知は
// i/notifications と同じ単体 shape のまま残す。
//
// fetch / cursor / type filter / CanSeeNote ゲート (#1444) は Show と共通の
// collectNotifications を使うので、grouped 側でも note embed の可視性は保たれる。
func (h *Handler) Grouped(c echo.Context) error {
	user := middleware.GetUser(c)
	req, ok := h.bindListRequest(c)
	if !ok {
		return apierr.JSONInvalidParam(c)
	}

	// upstream i/notifications-grouped.ts も getNotifications の前に同じ 2 つの
	// 早期 return を持つ (#2835)。
	//
	// **下の `len(all) > 0` では止まらない。** svc.List の exclude filter は
	// `excludeSet[n.Type]` の一致しか見ないので、**notificationTypeList に無い
	// type の行は excludeTypes 全指定でも生き残る**。該当するのは obsolete
	// (`pollVote` / `groupInvited`) と mk-go 固有 (`importCompleted` /
	// `abuseReport`)。前者の producer は無いが、それ以前に積まれた行は
	// ストリームに残りうる。後者の `abuseReport` は現に produce される (#2868)。
	// 1 件あるだけで `len(all) > 0` が成立し既読化まで走ってしまう。
	// upstream は早期 return するので `[]` が正。
	if emptyByTypeFilter(req) {
		return c.JSON(http.StatusOK, []any{})
	}
	// 早期 return の**後**に obsolete を除去する (#2837)。順序が結果を分ける。
	req.IncludeTypes = stripObsoleteTypes(req.IncludeTypes)
	req.ExcludeTypes = stripObsoleteTypes(req.ExcludeTypes)

	// **drop 前の全列を受け取る** (#2739)。grouping は全列で行い、drop 済みは
	// グループの中身から外す。drop 済みを列から抜いてから畳むと、挟まった通知が
	// 区切りとして働かず両隣が 1 グループになる。
	all, dropped, notifierByID, noteByID, err := h.collectNotificationsWithDropped(c, user, req, true)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	filtered := survivors(all, dropped)

	// 各通知を単体 pack して packed map を得る。packed の各要素には note / user /
	// reaction が既に埋まっている (#1444 の可視性ゲート適用済み) ので、grouping では
	// それらを再利用するだけで良い。
	//
	// **pack するのは survivor だけ。** 全列を pack すると mute した相手の user が
	// 引かれて packed に載り、grouping 後の subtract をすり抜ける。
	//
	// **packed は filtered と 1:1 とも限らない。** PackNotifications は roleAssigned
	// (role 削除済) / chatRoomInvitationReceived (invitation 削除済) を通知ごと drop
	// する。突き合わせは groupNotifications 側で id を使って行う (#2736)。
	items := make([]entity.NotificationItem, 0, len(filtered))
	for _, n := range filtered {
		items = append(items, entity.NotificationItem{
			N:    n,
			User: notifierByID[n.NotifierID],
			Note: noteByID[n.NoteID],
		})
	}
	opts, err := h.notificationOptions(user.ID, items)
	if err != nil {
		slog.Error("notifications-grouped: resolve abuse report states failed", "err", err)
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	packed := entity.PackNotifications(items, h.idGen, h.instanceLookup(), h.emojiLookup(), opts...)
	// depth-2 embed hide (#1570): grouping の前に通知 note の renote/reply embed と
	// 著者設定ゲートを viewer 可視性で適用する。Show と同じく #1444 CanSeeNote gate は
	// 落とすだけで embed に再帰しないため、ここで hide しないと grouped 経由で
	// 非可視 embed 本文が漏れる (#1546 で混入しかけた IDOR を塞ぐ)。
	notehide.HideNotificationNotes(user, packed)

	grouped := groupNotifications(all, packed, noteByID)
	// 本家 i/notifications-grouped.ts:153 と同じく grouping 後に limit で slice する。
	// **現状は到達しない** — collectNotifications が svc.List に limit を渡して既に
	// 切っており、grouping は件数を増やさないため。upstream と同じ位置に置くことで、
	// 将来 fetch 側が多めに取るようになっても upstream と同じ行が返るようにしてある。
	if len(grouped) > (*req.Limit) {
		grouped = grouped[:(*req.Limit)]
	}

	// **取得が 0 件なら既読化しない** (#2833)。upstream i/notifications-grouped.ts は
	// `notifications.length === 0` で `readAllNotification` を呼ぶ前に return する
	// (i/notifications 側には無い、grouped だけの早期 return)。
	//
	// MarkAllAsRead が進める先は「fetch が返した行」ではなく**ストリームの最新
	// エントリ**なので、0 件の fetch で呼ぶと、ユーザーが一度も受け取っていない
	// 通知まで既読位置が飛ぶ。
	//
	// 判定は `all` = svc.List の生の結果で、upstream の `getNotifications` 戻り値に
	// あたる。**grouped ではなく all を見る** — pack / 可視性 drop の後で判定すると
	// upstream より後ろの位置になり、逆向きの乖離を作る (upstream はそこでは
	// 既読化する)。
	if len(all) > 0 {
		h.maybeMarkAsRead(c, user, req)
	}
	return c.JSON(http.StatusOK, grouped)
}

// groupNotifications folds consecutive reaction / renote notifications into the
// grouped shape, mirroring upstream Misskey TS i/notifications-grouped.ts.
//
//   - 連続する reaction で同一 note (= 同 NoteID) なら `reaction:grouped` に
//     畳み込み、reactions[] に {user, reaction} を push する。
//   - 連続する renote で同一 target note (= renote note の RenoteID) なら
//     `renote:grouped` に畳み込み、users[] に notifier user を push する。
//
// それ以外は単体 packed map をそのまま残す。
//
// **all は drop 前の全列** (#2739)。upstream は raw 列に対して grouping してから
// pack で null を落とすので、grouping 条件に当たらない通知 (mute した相手の
// follow など) を挟んだ 2 件は畳まれない。drop 済みの列を渡すと挟まった通知が
// 区切りとして働かず、両隣が 1 グループになる。
//
// **畳んでから中身を引く。** upstream は grouping 条件に当たる通知を畳んだうえ
// で pack させるため、`reaction:grouped` が notifierId を持たないことを利用して
// mute した相手の reaction が reactions[] に残る (`#validateNotifier` の
// `if (!('notifierId' in notification)) return true`)。mk-go はグループを作った
// あとで drop 済みメンバーを中身から外すので、そこは漏らさない。
//
// **生き残りが 1 件だけになったグループは単体行として出す。これは upstream との
// 意図的な差。** upstream は grouping 段では 1 件のグループを作らないが、pack 段で
// メンバーが落ちた結果 1 件になったグループはそのまま返す (`#packInternal` が
// null にするのは `reactions.length === 0` / `users.length === 0` のときだけ)。
// mk-go は mute / suspended でもメンバーを外すので該当頻度がはるかに高く、
// 1 件の `reaction:grouped` を返し続けるより単体 shape のほうが素直。
//
// packed は survivor だけを pack したもので、id で引く。pack 段の drop
// (roleAssigned で role 削除済 / chatRoomInvitationReceived で invitation 削除済、
// #2736) も同じ扱いになる — グループの中身から外れ、単独なら行ごと消えて区切り
// として働く。
//
// **id / createdAt / note は生存メンバーから採る。これも意図的な差。** upstream は
// raw 列基準 (`id` は最後のメンバー、reaction の `createdAt` は先頭、renote は
// 2 番目、`noteId` は先頭) で drop の有無を見ないので、先頭や末尾が drop された
// グループでは値がずれる。
//
// 生存基準にするのは**落とした通知の id / 時刻を出さないため**。id は aidx で
// 生成時刻を含むので、mute した相手がいつ reaction したかが漏れる。
// ページングが壊れるからではない — `untilId` は行の lookup ではなく
// `n.ID >= untilID` の辞書順フィルタ (notification_service.go) なので、
// drop 済みの id を渡しても境界として機能する。
//
// **不可視 note の drop は種類が違う。** upstream は hideNote で blank するだけで
// 行を残すが、mk-go は行ごと落とす (#1444 / #1953)。reaction grouping は通知が持つ
// NoteID だけで判定するので畳み方は upstream と一致する。renote grouping は target
// note を noteByID から引くので、**note を解決できない行だけ**は畳めず区切りになる
// (mute / suspended などで drop された行の note は collectNotifications が全列ぶん
// 引いているので解決できる。#2739)。
func groupNotifications(all []*notification.Notification, packed []map[string]any, noteByID map[string]*model.Note) []map[string]any {
	byID := make(map[string]map[string]any, len(packed))
	for _, p := range packed {
		byID[asString(p["id"])] = p
	}

	out := make([]map[string]any, 0, len(packed))
	for _, g := range buildGroups(all, noteByID) {
		// **drop 済みメンバーはここで外す。** グループを作る段では外さない
		// (外すと区切りとして働いてしまう)。
		kept := make([]*notification.Notification, 0, len(g.members))
		for _, n := range g.members {
			if byID[n.ID] != nil {
				kept = append(kept, n)
			}
		}
		switch {
		case len(kept) == 0:
			// 全員 drop。行は出さないが、グループが分かれていた分だけ
			// 両隣は別行のままになる (= 区切りとして働く)。
		case len(kept) == 1:
			out = append(out, byID[kept[0].ID])
		case g.kind == notification.TypeReaction:
			out = append(out, buildReactionGroup(kept, byID))
		default:
			out = append(out, buildRenoteGroup(kept, byID))
		}
	}
	return out
}

// notificationGroup is a run of consecutive notifications that upstream would
// fold into one row. kind is the notification type that seeded the run
// (single-member runs keep the member's own type).
type notificationGroup struct {
	kind    notification.Type
	members []*notification.Notification
}

// buildGroups partitions all into runs using the same pair predicate upstream
// applies, **without** looking at whether a row survives packing.
func buildGroups(all []*notification.Notification, noteByID map[string]*model.Note) []notificationGroup {
	groups := make([]notificationGroup, 0, len(all))
	for i, cur := range all {
		if i > 0 && continuesGroup(all[i-1], cur, noteByID) {
			last := &groups[len(groups)-1]
			last.members = append(last.members, cur)
			continue
		}
		groups = append(groups, notificationGroup{kind: cur.Type, members: []*notification.Notification{cur}})
	}
	return groups
}

// continuesGroup reports whether cur folds into the run seeded by prev.
// upstream i/notifications-grouped.ts:107-155 と同じ pair 述語。
func continuesGroup(prev, cur *notification.Notification, noteByID map[string]*model.Note) bool {
	if prev.Type == notification.TypeReaction && cur.Type == notification.TypeReaction {
		// `prev.NoteID != ""` は upstream に無い防御。NoteID 空の reaction 通知は
		// 作られないので外してもテストは落ちないが、空キー同士が畳まれるのを
		// 防ぐ (fail-closed)。
		return prev.NoteID != "" && prev.NoteID == cur.NoteID
	}
	if prev.Type == notification.TypeRenote && cur.Type == notification.TypeRenote {
		// `pt != ""` も同じく防御。target を解決できない行は note-missing で
		// drop されるので、production 配線下では観測差が出ない。
		pt, ct := renoteTargetID(prev, noteByID), renoteTargetID(cur, noteByID)
		return pt != "" && pt == ct
	}
	return false
}

// buildReactionGroup materializes a reaction:grouped row from the surviving
// members. id は最後のメンバー、createdAt は先頭のメンバーのものを採る。
func buildReactionGroup(kept []*notification.Notification, byID map[string]map[string]any) map[string]any {
	g := newReactionGroup(kept[0], byID[kept[0].ID])
	for _, n := range kept[1:] {
		appendReaction(g, n, byID[n.ID])
	}
	g["id"] = kept[len(kept)-1].ID
	return g
}

// buildRenoteGroup materializes a renote:grouped row from the surviving
// members.
//
// #2106 L23: upstream の renote group は seed の createdAt に**2 番目**の通知
// (一覧の並びで 1 つ後。降順ページなら古い方) の時刻を採る (reaction group は
// 先頭で一致)。
func buildRenoteGroup(kept []*notification.Notification, byID map[string]map[string]any) map[string]any {
	g := newRenoteGroup(kept[0], byID[kept[0].ID])
	g["createdAt"] = byID[kept[1].ID]["createdAt"]
	for _, n := range kept[1:] {
		appendRenoteUser(g, byID[n.ID])
	}
	g["id"] = kept[len(kept)-1].ID
	return g
}

// renoteTargetID returns the target note id of a renote notification (= the
// RenoteID of the renote note stored under the notification's NoteID). Returns
// "" when the renote note is not visible / fetchable so non-resolvable renotes
// never group (fail-closed: distinct rows stay separate rather than collapsing
// on an empty key).
func renoteTargetID(n *notification.Notification, noteByID map[string]*model.Note) string {
	if n.NoteID == "" {
		return ""
	}
	note := noteByID[n.NoteID]
	if note == nil || note.RenoteID == nil {
		return ""
	}
	return *note.RenoteID
}

// newReactionGroup seeds a reaction:grouped entry from the first packed
// reaction notification. note は packed map から引き継ぐ (可視性ゲート済み)。
func newReactionGroup(first *notification.Notification, firstPacked map[string]any) map[string]any {
	g := map[string]any{
		"id":        first.ID,
		"createdAt": firstPacked["createdAt"],
		"type":      "reaction:grouped",
		"reactions": []map[string]any{reactionEntry(firstPacked)},
	}
	if note, ok := firstPacked["note"]; ok {
		g["note"] = note
	}
	return g
}

// appendReaction pushes one {user, reaction} entry onto a reaction:grouped.
func appendReaction(group map[string]any, _ *notification.Notification, packed map[string]any) {
	arr, _ := group["reactions"].([]map[string]any)
	group["reactions"] = append(arr, reactionEntry(packed))
}

// reactionEntry builds the {user, reaction} pair from a single packed reaction
// notification map.
func reactionEntry(packed map[string]any) map[string]any {
	e := map[string]any{}
	if u, ok := packed["user"]; ok {
		e["user"] = u
	}
	if r, ok := packed["reaction"]; ok {
		e["reaction"] = r
	}
	return e
}

// newRenoteGroup seeds a renote:grouped entry from the first packed renote
// notification. users[] holds packed notifier users; note は packed map から
// 引き継ぐ。
func newRenoteGroup(first *notification.Notification, firstPacked map[string]any) map[string]any {
	g := map[string]any{
		"id":        first.ID,
		"createdAt": firstPacked["createdAt"],
		"type":      "renote:grouped",
		"users":     renoteUsers(firstPacked),
	}
	if note, ok := firstPacked["note"]; ok {
		g["note"] = note
	}
	return g
}

// appendRenoteUser pushes one packed notifier user onto a renote:grouped.
func appendRenoteUser(group map[string]any, packed map[string]any) {
	arr, _ := group["users"].([]any)
	if u, ok := packed["user"]; ok {
		arr = append(arr, u)
	}
	group["users"] = arr
}

// renoteUsers builds the initial users[] slice for a renote:grouped from the
// first packed notification's user (may be empty when the notifier user could
// not be resolved).
func renoteUsers(packed map[string]any) []any {
	if u, ok := packed["user"]; ok {
		return []any{u}
	}
	return []any{}
}

// asString returns the string value of v or "" when v is not a string.
func asString(v any) string {
	s, _ := v.(string)
	return s
}
