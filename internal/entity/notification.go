package entity

// Note: this file intentionally imports `internal/core/notification` for
// the Notification struct definition, which is the only exception to the
// entity→{model,misc}-only convention in this package. Moving the struct
// to `internal/model` would touch 20+ call sites; the direct import keeps
// the packer API ergonomic and is limited to this one file. Callers that
// want to avoid the transitive dependency should pass primitives instead.

import (
	"github.com/shiroha-a/mk/internal/core/notification"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
)

// NotificationItem bundles a notification record with its already-resolved
// notifier user and referenced note. PackNotifications takes a slice of
// these so the caller can pre-fetch users / notes once and the packer can
// build InstanceResolver / EmojiResolver once for the whole batch (#428)。
type NotificationItem struct {
	N    *notification.Notification
	User *model.User
	Note *model.Note
}

// RoleLookup resolves a role ID to its packed representation, returning false
// when the role no longer exists. Used to embed `role` on roleAssigned
// notifications and to drop the notification when the role was deleted
// (upstream NotificationEntityService が role==null で null を返すのと同じ)。
type RoleLookup func(roleID string) (map[string]any, bool)

// ChatInvitationLookup resolves a chat room invitation ID to its packed
// representation for the given viewer, returning false when the invitation no
// longer exists. Used to embed `invitation` on chatRoomInvitationReceived
// notifications and to drop the notification when the invitation was deleted
// (upstream NotificationEntityService が invitation==null で null を返すのと同じ)。
type ChatInvitationLookup func(invitationID, viewerID string) (map[string]any, bool)

// EmojiApplicationStatus is the read-time state of a custom emoji request
// referenced by an emojiApplicationProcessed notification (#2934).
//
// **通知には applicationId しか積んでいない。** 結果と却下理由を通知へ複製すると、
// 申請を消しても文面が Redis に残り続ける。abuseReport (#2868) と同じ形。
type EmojiApplicationStatus struct {
	// Name is the requested emoji name, e.g. "sushi".
	Name string
	// Status is "approved" / "rejected" / その他の終端状態。
	Status string
	// RejectReason is shown to the applicant as-is. 却下でなければ空。
	RejectReason string
}

// EmojiApplicationLookup resolves an application ID to its current state,
// returning false when the row no longer exists.
type EmojiApplicationLookup func(applicationID string) (EmojiApplicationStatus, bool)

// SignupApplicationStatus is the read-time state of an account signup request
// referenced by a signupApplicationReceived notification (#2987).
//
// **回答は持たない。** 申請フォームの回答は運営者が定義した任意の項目で、
// 氏名や連絡先が入りうる。通知欄では読めないうえ、申請を消しても複製が残る。
// モデレーターが通知欄で知る必要があるのは「まだ処理されていないか」だけで、
// 中身は管理画面で見る。
type SignupApplicationStatus struct {
	// Status is "pending" / "approved" / "rejected" / "expired"。
	Status string
}

// SignupApplicationLookup resolves an application ID to its current state,
// returning false when the row no longer exists (#2987).
type SignupApplicationLookup func(applicationID string) (SignupApplicationStatus, bool)

// AbuseReportStatus is the read-time state of a report referenced by an
// abuseReport notification (#2868).
type AbuseReportStatus struct {
	// Resolved reports whether a moderator has already dealt with it.
	Resolved bool
	// ResolvedAs is "accept" / "reject" / "" (その他)。Resolved が false の
	// ときは空。
	ResolvedAs string
	// AssigneeID is the moderator who resolved it, if recorded.
	AssigneeID string
	// UnresolvedCount is the number of reports nobody has dealt with yet,
	// across the whole instance. nil when it could not be determined.
	//
	// **通知をまとめているので要る (#3200)。** 未読がある間は新しい通報の通知を
	// 作らないので、この通知が指す 1 件が対処済みでも後続が残っていることを
	// ここで出す。分からないときに 0 を出すと「未対応なし」と読めてしまうので、
	// 出さない。
	UnresolvedCount *int64
}

// AbuseReportLookup resolves a report ID to its current state, returning false
// when the report no longer exists (#2868).
//
// **read 時に引く。** 通知は作成時点の状態しか持たないので、他のモデレーターが
// 対処しても通知欄は「未対応」のまま残る。roleAssigned が role を引き直すのと
// 同じ形で、削除済みなら通知ごと drop する。
type AbuseReportLookup func(reportID string) (AbuseReportStatus, bool)

// packOptions carries the optional read-time lookups required by notification
// types that embed a related entity which must be packed fresh. Threaded via
// functional options so existing call sites stay unchanged.
type packOptions struct {
	role              RoleLookup
	abuseReport       AbuseReportLookup
	emojiApplication  EmojiApplicationLookup
	signupApplication SignupApplicationLookup
	chatInvitation    ChatInvitationLookup
	// viewer は invitation pack の視点 (= notifiee)。chatRoomInvitationReceived
	// の room.isMuted / invitationExists を viewer 視点で算出するために渡す。
	viewer string
	// noteResolver は埋め込み note の後段 field (files / channel / myReaction /
	// poll の isVoted) を埋める。nil なら files が空配列のままになる (#2735)。
	// viewer は上の `viewer` を共用する (notifiee は 1 人しかいない)。
	noteResolver *NoteFieldResolver
}

// NotificationOption configures optional notification packing behavior.
type NotificationOption func(*packOptions)

// WithAbuseReportLookup supplies the AbuseReportLookup used to pack the current
// state of abuseReport notifications (#2868)。
func WithAbuseReportLookup(fn AbuseReportLookup) NotificationOption {
	return func(o *packOptions) { o.abuseReport = fn }
}

// WithEmojiApplicationLookup supplies the lookup used to pack the current state
// of emojiApplicationProcessed notifications (#2934)。
func WithEmojiApplicationLookup(fn EmojiApplicationLookup) NotificationOption {
	return func(o *packOptions) { o.emojiApplication = fn }
}

// WithSignupApplicationLookup supplies the lookup used to pack the current state
// of signupApplicationReceived notifications (#2987)。
func WithSignupApplicationLookup(fn SignupApplicationLookup) NotificationOption {
	return func(o *packOptions) { o.signupApplication = fn }
}

// WithRoleLookup supplies the RoleLookup used to pack roleAssigned
// notifications (#1559)。
func WithRoleLookup(fn RoleLookup) NotificationOption {
	return func(o *packOptions) { o.role = fn }
}

// WithChatInvitationLookup supplies the ChatInvitationLookup used to pack
// chatRoomInvitationReceived notifications (#1559)。
func WithChatInvitationLookup(fn ChatInvitationLookup) NotificationOption {
	return func(o *packOptions) { o.chatInvitation = fn }
}

// WithViewer sets the viewer (= notifiee) used by viewer-dependent embeds such
// as the chat invitation's room flags (#1559)。
func WithViewer(viewerID string) NotificationOption {
	return func(o *packOptions) { o.viewer = viewerID }
}

// WithNoteFieldResolver supplies the NoteFieldResolver used to populate the
// embedded note's post-pack fields: `files` above all, plus channel, myReaction
// and the poll's isVoted (#2735)。
//
// packNoteAtDepth は Files を空スライスで初期化するだけなので、これを渡さない
// 限り通知の note は添付メディアを持たない。upstream NotificationEntityService は
// note を `noteEntityService.pack(noteId, { id: meId }, { detail: true })` で
// pack するので、viewer 依存 field まで含めて解決するのが parity 側。
//
// viewer は WithViewer で渡した notifiee ID をそのまま使う。**resolver が viewer
// から読むのは ID だけ** (ResolveViewerFields) なので、pack のためだけに user 行を
// 引き直さない。この前提は TestResolveViewerFields_UsesViewerIDOnly が固定する。
func WithNoteFieldResolver(r *NoteFieldResolver) NotificationOption {
	return func(o *packOptions) { o.noteResolver = r }
}

func buildPackOptions(opts []NotificationOption) *packOptions {
	o := &packOptions{}
	for _, fn := range opts {
		if fn != nil {
			fn(o)
		}
	}
	return o
}

// PackNotification converts a Notification record into the map shape
// emitted over the `main` WebSocket channel and returned by
// /api/i/notifications. Matches Misskey's NotificationEntityService.pack
// output: id / createdAt / type / userId / user (PackUserLite) / note
// (PackNote) / reaction / choice. Optional user/note arguments are only
// embedded when non-nil so the caller can pre-fetch once and re-use.
//
// instLookup / emojiLookup を渡すと top-level user と note.user に
// Instance / 絵文字 URL を埋める。nil なら no-op リゾルバとして扱われ、
// 従来互換の挙動になる (#415 notifications page InstanceTicker)。
//
// **Single-item only.** 内部で 1 件用の InstanceResolver / EmojiResolver を
// 都度作るので、ループから呼ぶと N+1 クエリになる。複数件をまとめて pack
// する callsite (例: /api/i/notifications) は PackNotifications を使うこと
// (#428)。
func PackNotification(n *notification.Notification, user *model.User, note *model.Note, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, opts ...NotificationOption) map[string]any {
	if n == nil {
		return nil
	}
	out := PackNotifications([]NotificationItem{{N: n, User: user, Note: note}}, idGen, instLookup, emojiLookup, opts...)
	if len(out) == 0 {
		return nil
	}
	return out[0]
}

// PackNotifications batches PackNotification across a slice of items with a
// single InstanceResolver / EmojiResolver shared across all items. Callers
// must pre-resolve notifier User and target Note (FindByID / FindByIDWithUser)
// per item; the packer is responsible only for resolver-level batching
// (Instance / 絵文字)。
//
// 通知一覧 hot-path (#428) で 1 件ごとに resolver を作っていた N+1 を解消する。
// 20 件ページで Instance lookup × 2 + Emoji lookup × 2 (note 用 + user 用) =
// 約 80 クエリ → 各 1 回に削減される (host / 絵文字名は内部で uniq 化)。
//
// items 内で N == nil の要素は単純に skip する。残りは入力順序を保って返す。
func PackNotifications(items []NotificationItem, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, opts ...NotificationOption) []map[string]any {
	packOpts := buildPackOptions(opts)
	// 全 item の notifier user + 埋め込み note.user / Renote/Reply author を
	// flatten して resolver の入力にする。
	notifierUsers := make([]*model.User, 0, len(items))
	notes := make([]*model.Note, 0, len(items))
	// EmojiResolver は notes ベースで host 別に集約するので、notifier user の
	// User.Emojis を拾うために合成 note shell ({User: u}) を別途 push する
	// (単品ケース packNotificationCore でも同じ shell を経由する形)。
	syntheticUserOnlyNotes := make([]*model.Note, 0, len(items))
	for _, it := range items {
		// it.N == nil の要素は最終的に skip されるので resolver 入力にも
		// 含めない。User / Note だけが詰まった「亡霊」item を渡された場合に
		// 無駄な host / 絵文字 fetch を起こさないための防御。
		if it.N == nil {
			continue
		}
		if it.User != nil {
			notifierUsers = append(notifierUsers, it.User)
			syntheticUserOnlyNotes = append(syntheticUserOnlyNotes, &model.Note{User: it.User})
		}
		if it.Note != nil {
			notes = append(notes, it.Note)
		}
	}
	flat := flattenNotesPlusRelations(notes)
	noteAuthors := CollectNoteAuthors(flat)
	// notifierUsers の backing array は cap=len(items) で確保済み。素朴な
	// append(notifierUsers, ...) だと余剰 cap 内で同じ backing array に書く
	// 可能性があり、後で notifierUsers を再利用したくなった時に aliasing
	// バグになりうる。意識的に新規 backing array を確保しておく。
	allUsers := make([]*model.User, 0, len(notifierUsers)+len(noteAuthors))
	allUsers = append(allUsers, notifierUsers...)
	allUsers = append(allUsers, noteAuthors...)
	instResolver := NewInstanceResolver(instLookup, allUsers...)
	emojiInput := append(append([]*model.Note(nil), flat...), syntheticUserOnlyNotes...)
	emojiResolver := NewEmojiResolver(emojiLookup, emojiInput)

	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if packed := packNotificationCore(it.N, it.User, it.Note, idGen, instResolver, emojiResolver, packOpts); packed != nil {
			out = append(out, packed)
		}
	}
	resolveNoteFields(out, packOpts)
	return out
}

// resolveNoteFields runs the NoteFieldResolver over every embedded `note` of the
// packed batch in a SINGLE pass, so a 20-item notification page costs one
// drive_file / channel / reaction lookup rather than one per notification.
//
// Apply は []NoteEntity の要素を `&notes[i]` 経由で mutate するため、map から値を
// 取り出した slice に対して走らせて書き戻す (notehide.hideNotificationNotesAt と
// 同型)。Renote / Reply は pointer なので embed 側の解決はそのまま反映される。
func resolveNoteFields(packed []map[string]any, opts *packOptions) {
	if opts == nil || opts.noteResolver == nil || len(packed) == 0 {
		return
	}
	notes := make([]NoteEntity, 0, len(packed))
	idx := make([]int, 0, len(packed))
	for i := range packed {
		n, ok := packed[i]["note"].(NoteEntity)
		if !ok {
			continue
		}
		notes = append(notes, n)
		idx = append(idx, i)
	}
	if len(notes) == 0 {
		return
	}
	// viewer は notifiee (= WithViewer)。空なら viewer 非依存の field だけ解決する。
	var viewer *model.User
	if opts.viewer != "" {
		viewer = &model.User{ID: opts.viewer}
	}
	opts.noteResolver.Apply(notes, viewer)
	for j, i := range idx {
		packed[i]["note"] = notes[j]
	}
}

// packNotificationCore is the resolver-aware body shared by the single-item
// PackNotification entry point and the batched PackNotifications. Resolvers
// are passed in so the same instance can be re-used across many items.
func packNotificationCore(n *notification.Notification, user *model.User, note *model.Note, idGen id.Generator, instResolver *InstanceResolver, emojiResolver *EmojiResolver, opts *packOptions) map[string]any {
	if n == nil {
		return nil
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	out := map[string]any{
		"id":        n.ID,
		"createdAt": n.CreatedAt.UTC().Format(tsFormat),
		"type":      string(n.Type),
	}
	// roleAssigned は Extra["roleId"] を read 時に packed role へ解決する。
	// role 削除済 / lookup 未配線なら通知ごと drop する (TS
	// NotificationEntityService が role==null で null を返すのと同じ挙動)。
	if n.Type == notification.TypeRoleAssigned {
		roleID, _ := n.Extra["roleId"].(string)
		if opts == nil || opts.role == nil {
			return nil
		}
		packed, ok := opts.role(roleID)
		if !ok {
			return nil
		}
		out["role"] = packed
	}
	// chatRoomInvitationReceived は Extra["invitationId"] を read 時に packed
	// invitation へ解決する。削除済 / lookup 未配線なら通知ごと drop する。
	// abuseReport は Extra["reportId"] を read 時に引き直して現在の状態を出す
	// (#2868)。通知は作成時点しか持たないので、他のモデレーターが対処しても
	// 通知欄が「未対応」のまま残る。削除済 / lookup 未配線なら通知ごと drop
	// する (roleAssigned と同じ形)。
	// emojiApplicationProcessed は Extra["applicationId"] を read 時に引き直す
	// (#2934)。承認/却下と却下理由を通知へ複製すると、申請を消しても文面が
	// Redis に残る。**申請が消えていたら通知ごと drop する** — 経緯を辿れない
	// 「処理されました」だけの通知は読んだ人に何も伝えない (roleAssigned と同じ形)。
	// emojiApplicationReceived (#2987) は審査する側へ出す通知で、向きは逆だが
	// 引くものは同じ (同じ行の現在の状態)。**処理済みかどうかが要る** — 通知は
	// 作成時点しか持たないので、無いと他の人が処理済みの申請に二重で当たる。
	if n.Type == notification.TypeEmojiApplicationProcessed || n.Type == notification.TypeEmojiApplicationReceived {
		applicationID, _ := n.Extra["applicationId"].(string)
		if opts == nil || opts.emojiApplication == nil {
			return nil
		}
		st, ok := opts.emojiApplication(applicationID)
		if !ok {
			return nil
		}
		out["emojiApplication"] = map[string]any{
			"id":     applicationID,
			"name":   st.Name,
			"status": st.Status,
		}
		if st.RejectReason != "" {
			out["emojiApplication"].(map[string]any)["rejectReason"] = st.RejectReason
		}
	}
	if n.Type == notification.TypeSignupApplicationReceived {
		applicationID, _ := n.Extra["applicationId"].(string)
		if opts == nil || opts.signupApplication == nil {
			return nil
		}
		st, ok := opts.signupApplication(applicationID)
		if !ok {
			return nil
		}
		out["signupApplication"] = map[string]any{
			"id":     applicationID,
			"status": st.Status,
		}
	}
	if n.Type == notification.TypeAbuseReport {
		reportID, _ := n.Extra["reportId"].(string)
		if opts == nil || opts.abuseReport == nil {
			return nil
		}
		status, ok := opts.abuseReport(reportID)
		if !ok {
			return nil
		}
		out["resolved"] = status.Resolved
		if status.ResolvedAs != "" {
			out["resolvedAs"] = status.ResolvedAs
		}
		if status.AssigneeID != "" {
			out["assigneeId"] = status.AssigneeID
		}
		if status.UnresolvedCount != nil {
			out["unresolvedCount"] = *status.UnresolvedCount
		}
	}
	if n.Type == notification.TypeChatRoomInvitationReceived {
		invID, _ := n.Extra["invitationId"].(string)
		if opts == nil || opts.chatInvitation == nil {
			return nil
		}
		packed, ok := opts.chatInvitation(invID, opts.viewer)
		if !ok {
			return nil
		}
		out["invitation"] = packed
	}
	if n.NotifierID != "" {
		// TS本家はnotifierIdを"userId"として返す。
		out["userId"] = n.NotifierID
		if user != nil {
			lite := PackUserLite(user)
			instResolver.FillUserLite(&lite)
			emojiResolver.PopulateUserEmojis(user, &lite)
			out["user"] = lite
		}
	}
	if n.NoteID != "" {
		// reply/mention/reaction/renote/quote/pollEnded等、noteを要する
		// 通知タイプ向け。REST API互換のため noteId は常に含める。
		// 加えて note object が fetch できていればpacked Noteも入れる。
		// applyNoteResolvers で note.user / Renote/Reply の embed にも
		// Instance / 絵文字を適用する。
		out["noteId"] = n.NoteID
		if note != nil && idGen != nil {
			packed := PackNote(note, idGen)
			applyNoteResolvers(note, &packed, instResolver, emojiResolver)
			out["note"] = packed
		}
	}
	if n.Reaction != "" {
		out["reaction"] = n.Reaction
	}
	if n.Choice != nil {
		out["choice"] = *n.Choice
	}
	// extraはTS本家では role/invitation/achievement 等の個別フィールド
	// として展開されるが、Go側はmap[string]anyで透過的にmergeする。
	// core keyと衝突する場合は extra 側をskip (packerの契約を壊さない)。
	for k, v := range n.Extra {
		if _, collides := out[k]; collides {
			continue
		}
		// roleAssigned の roleId / chatRoomInvitationReceived の invitationId は
		// packed entity (out["role"] / out["invitation"]) に解決済なので raw ID を
		// surface しない (TS は packed entity のみ返す)。
		if k == "roleId" || k == "invitationId" {
			continue
		}
		// abuseReport の comment は #2868 以降は積んでいないが、それ以前に
		// 永続化された通知は本文を持つ (Redis stream は MaxPerUser で回転する
		// まで残る)。通知に本文を出さない方針は既存分にも及ぼす。
		// emojiApplicationProcessed は applicationId を out["emojiApplication"].id
		// として出しているので、Extra から素通りさせると二重に出る
		// (roleId / invitationId を落としているのと同じ)。
		if k == "applicationId" && (n.Type == notification.TypeEmojiApplicationProcessed ||
			n.Type == notification.TypeEmojiApplicationReceived ||
			n.Type == notification.TypeSignupApplicationReceived) {
			continue
		}
		if k == "comment" && n.Type == notification.TypeAbuseReport {
			continue
		}
		// exportCompleted 通知の exportedEntity は misskey_dart / Misskey TS が
		// singular / camelCase の enum で受ける。作成時に core/transfer 側で正規化
		// 済み (#1249) だが、それ以前に永続化された通知は内部値 ("notes" 等) を
		// 保持しており、misskey_dart の enum decode を全件巻き込んで落とす (#1391)。
		// read 時にも正規化して既存データでクライアントが crash しないようにする。
		if k == "exportedEntity" {
			if s, ok := v.(string); ok {
				out[k] = normalizeExportedEntity(s)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// exportedEntityAliases maps mk-go 内部の export type 名 (plural / kebab-case) を
// exportCompleted 通知の exportedEntity が取るべき Misskey enum 値 (singular /
// camelCase) へ変換する。core/transfer.misskeyExportedEntity (create 時) と同じ
// 対応で、read 時の defense-in-depth として持つ。
var exportedEntityAliases = map[string]string{
	"notes":         "note",
	"favorites":     "favorite",
	"user-lists":    "userList",
	"antennas":      "antenna",
	"clips":         "clip",
	"mute":          "muting",
	"custom-emojis": "customEmoji",
}

// normalizeExportedEntity returns the Misskey enum value for an exportedEntity,
// translating mk-go internal aliases. Already-canonical / unknown values pass
// through unchanged (= following / blocking は元から一致、未知値は壊さない)。
func normalizeExportedEntity(v string) string {
	if canon, ok := exportedEntityAliases[v]; ok {
		return canon
	}
	return v
}
