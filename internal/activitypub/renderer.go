package activitypub

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shiroha-a/mk/internal/activitypub/mfm"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
)

// publishedLayout is the millisecond ISO8601 layout (JS Date.toISOString()) used
// for every AP `published` / `updated` timestamp, matching upstream
// ApRendererService which formats all of them via toISOString() (#1948-11).
// stdlib time.RFC3339 produces second precision (no `.000`), diverging on the
// wire from Misskey's millisecond form.
const publishedLayout = "2006-01-02T15:04:05.000Z"

// URLBuilder constructs canonical URLs for the local instance.
// 同じデータでも複数の場所から参照されるので、ヘルパとして集約しておく。
type URLBuilder struct {
	baseURL string
}

// NewURLBuilder constructs a URLBuilder rooted at baseURL (e.g. "https://example.com").
func NewURLBuilder(baseURL string) *URLBuilder {
	return &URLBuilder{baseURL: baseURL}
}

// UserURI returns the canonical actor URI for a local user.
func (b *URLBuilder) UserURI(userID string) string {
	return b.baseURL + "/users/" + userID
}

// IsLocalURI reports whether uri is served by this instance.
//
// 判定は baseURL の接頭辞一致。直後が区切り (`/`) か終端であることまで見るのは、
// `https://example.com.evil.test/...` を local と誤判定しないため。
func (b *URLBuilder) IsLocalURI(uri string) bool {
	if b.baseURL == "" || !strings.HasPrefix(uri, b.baseURL) {
		return false
	}
	rest := uri[len(b.baseURL):]
	return rest == "" || strings.HasPrefix(rest, "/")
}

// LocalUserIDFromURI extracts the user id from a canonical local actor URI
// (`<baseURL>/users/<id>`), returning "" when uri is not one.
//
// ローカルユーザーは `user.uri` 列を持たない (NULL) ので URI では引けない。
// upstream ApPersonService.fetchPerson が `uri.split('/').pop()` で id を取り
// 出して DB を引くのと同じ経路。
func (b *URLBuilder) LocalUserIDFromURI(uri string) string {
	prefix := b.baseURL + "/users/"
	if b.baseURL == "" || !strings.HasPrefix(uri, prefix) {
		return ""
	}
	rest := uri[len(prefix):]
	// `/users/<id>` ちょうどの形だけを受ける (配下のサブパスは actor ではない)。
	if rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

// UserProfileURL returns the human-facing profile page URL (/@username),
// used as the actor's `url` field distinct from its `id` (/users/<id>)。
// upstream renderPerson の url: `${config.url}/@${user.username}` に揃える (#1869)。
func (b *URLBuilder) UserProfileURL(username string) string {
	return b.baseURL + "/@" + username
}

// UserInbox returns the inbox URL for a user.
func (b *URLBuilder) UserInbox(userID string) string {
	return b.UserURI(userID) + "/inbox"
}

// UserOutbox returns the outbox URL for a user.
func (b *URLBuilder) UserOutbox(userID string) string {
	return b.UserURI(userID) + "/outbox"
}

// UserFollowers returns the followers collection URL.
func (b *URLBuilder) UserFollowers(userID string) string {
	return b.UserURI(userID) + "/followers"
}

// UserFollowing returns the following collection URL.
func (b *URLBuilder) UserFollowing(userID string) string {
	return b.UserURI(userID) + "/following"
}

// UserFeatured returns the pinned-notes (featured) collection URL
// (upstream: `${id}/collections/featured`, #1876)。
func (b *URLBuilder) UserFeatured(userID string) string {
	return b.UserURI(userID) + "/collections/featured"
}

// UserKeyURI returns the public key fragment URI used in HTTP signatures.
func (b *URLBuilder) UserKeyURI(userID string) string {
	return b.UserURI(userID) + "#main-key"
}

// SharedInbox returns the shared inbox URL.
func (b *URLBuilder) SharedInbox() string {
	return b.baseURL + "/inbox"
}

// NoteURI returns the canonical note URI.
func (b *URLBuilder) NoteURI(noteID string) string {
	return b.baseURL + "/notes/" + noteID
}

// QuoteAuthorizationURI returns the URI of a FEP-044f approval stamp for a
// local note (#3234). 承認の URI は承認した作者のホストでなければならない
// (Mastodon は Accept の actor と承認の URI のホストを突き合わせる) ので、自分の
// 投稿の下に置く。
func (b *URLBuilder) QuoteAuthorizationURI(noteID, authorizationID string) string {
	return b.NoteURI(noteID) + "/quote-authorizations/" + authorizationID
}

// QuoteRequestURI returns the id of the QuoteRequest a local quoting note sends
// (#3234)。引用する投稿 1 つにつき 1 つなので、投稿の URI から決める。
func (b *URLBuilder) QuoteRequestURI(noteID string) string {
	return b.NoteURI(noteID) + "#quote-request"
}

// CreateActivityURI returns the URI of the Create activity wrapping a note.
func (b *URLBuilder) CreateActivityURI(noteID string) string {
	return b.NoteURI(noteID) + "/activity"
}

// VoteActivityURI returns a stable URI for the AP `Note` representing a
// poll vote. URI must be unique per (voter, target) pair to avoid
// collisions when the same user votes on multiple polls. Misskey TS の
// upstream wire format に合わせて fragment 形式 (`#votes/<targetNoteID>`)
// を採用する (#690)。fragment 形式なら remote 側が既存 voter URL の
// 同一性で actor を識別できる。
func (b *URLBuilder) VoteActivityURI(voterID, targetNoteID string) string {
	return b.UserURI(voterID) + "#votes/" + targetNoteID
}

// FollowURI returns the URI for a Follow activity.
// followeeIDが完全なURIの場合はハッシュ化してパスセグメントに埋め込む。
func (b *URLBuilder) FollowURI(followerID, followeeID string) string {
	id := followeeID
	if strings.Contains(followeeID, "/") {
		h := sha256.Sum256([]byte(followeeID))
		id = hex.EncodeToString(h[:16])
	}
	return b.baseURL + "/follows/" + followerID + "/" + id
}

// BlockURI returns a stable URI for a Block activity from blockerID to
// blockeeURI. block 行 id ではなく (blocker, blockee) から決定的に生成するため、
// 後で unblock した際の Undo(Block) も同じ id を参照できる (#1560)。
func (b *URLBuilder) BlockURI(blockerID, blockeeURI string) string {
	h := sha256.Sum256([]byte(blockeeURI))
	return b.baseURL + "/blocks/" + blockerID + "/" + hex.EncodeToString(h[:16])
}

// FollowRelayURI returns the URI for a Follow-Relay activity.
// Path 形式は upstream と一致させる (`/activities/follow-relay/{relayID}`):
// Accept / Reject inbox ハンドラがこの正規表現で relay を特定するため、
// 変更するときは processor 側の regex も一緒に更新すること。
func (b *URLBuilder) FollowRelayURI(relayID string) string {
	return b.baseURL + "/activities/follow-relay/" + relayID
}

// MoveURI returns the URI for a Move activity from srcID to dstURI.
// 本家は `${baseURL}/moves/${src.id}/${dst.id}` を使うが、dstURI はリモートで
// 内部 ID を持たないため、URI を SHA-256 で 16 bytes にハッシュ化して path
// に使う。一意性とショート URL 表現を両立する。
func (b *URLBuilder) MoveURI(srcID, dstURI string) string {
	h := sha256.Sum256([]byte(dstURI))
	return b.baseURL + "/moves/" + srcID + "/" + hex.EncodeToString(h[:16])
}

// ChatMessageURI returns the canonical URI for a chat message.
func (b *URLBuilder) ChatMessageURI(messageID string) string {
	return b.baseURL + "/chat-messages/" + messageID
}

// ChatRoomURI returns the canonical AP URI for a chat room. CherryPick group
// chat federation embeds this in the Group object id, in Add/Remove targets,
// and in the group chat message note's `@context`. The path shape
// (`/chat/rooms/{id}`) is load-bearing: receivers extract the room id with
// `/\/chat\/rooms\/([a-zA-Z0-9]+)$/`.
func (b *URLBuilder) ChatRoomURI(roomID string) string {
	return b.baseURL + "/chat/rooms/" + roomID
}

// chatRoomURIRe is the parser half of ChatRoomURI's format contract. The id is
// alphanumeric (aidx / ULID).
//
// **組み立てる側の隣に置く。** 受信側はこの正規表現で room id を取り出すので、
// `ChatRoomURI` のパス形状を変えると連合が黙って壊れる。別パッケージに写すと
// 片方だけ直る。
//
// **接頭辞は縛らない。** これは**相手の URI** も通す正規表現で、`/chat/rooms/{id}`
// をどこに生やすかは相手の実装次第 (サブパスの下に出す構成もありうる)。自分の側は
// `config.resolve()` が `scheme://host` だけを `cfg.URL` にするのでサブパスは
// 出ないが、そこに合わせて縛ると相手の正当な URI を落とす。
//
// 「自分の room か」は host 文字列ではなく `URLBuilder.ChatRoomURI` で組み立て直して
// 突き合わせる (core/chat)。
var chatRoomURIRe = regexp.MustCompile(`/chat/rooms/([a-zA-Z0-9]+)$`)

// ChatRoomIDFromURI returns the room id embedded in a chat room URI, or "" when
// uri is not one.
func ChatRoomIDFromURI(uri string) string {
	m := chatRoomURIRe.FindStringSubmatch(uri)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// IsChatRoomURI reports whether uri has the shape of a chat room URI.
func IsChatRoomURI(uri string) bool { return ChatRoomIDFromURI(uri) != "" }

// ErrMentionUserNotFound is returned by MentionResolver implementations
// when the user does not exist (typically deleted or the ID is stale).
// Caller (renderer) は本 sentinel を skip 扱いにし、その他 error は DB
// 障害として log 出力する (#572 item 2)。
var ErrMentionUserNotFound = errors.New("mention user not found")

// MentionResolver resolves a note.Mentions entry (user ID) into the data
// required to build an AS Mention tag. 実装は server/router.go 側で
// UserRepository を wrap する形で提供される。
//
// 戻り値:
//   - 成功: name, uri, nil
//   - ユーザー未存在: "", "", ErrMentionUserNotFound (skip)
//   - DB 障害等: "", "", <DB error> (skip + log; 将来 retry 可能)
//
// uri はローカルユーザーなら urls.UserURI(user.ID)、リモートユーザーなら
// user.URI を返す。name は Misskey 互換で "@username" / "@username@host"。
type MentionResolver interface {
	ResolveMention(userID string) (name, uri string, err error)
}

// FileResolver loads drive files by ID for rendering Note attachments.
type FileResolver interface {
	FindByIDs(ids []string) ([]*model.DriveFile, error)
}

// EmojiResolver resolves custom emoji names to their model data.
type EmojiResolver interface {
	FindByNameAndHost(name string, host *string) (*model.Emoji, error)
}

// NoteResolver resolves a note by ID (for quote renote URI lookup).
type NoteResolver interface {
	FindByID(id string) (*model.Note, error)
}

// PollResolver loads a poll by its parent note ID. Used by RenderNote to
// populate Question fields (oneOf/anyOf/endTime/closed).
type PollResolver interface {
	FindByNoteID(noteID string) (*model.Poll, error)
}

// actorTypeForUser returns the AP actor `type` to emit for a local user,
// mirroring upstream ApRendererService.renderPerson: a system account
// (username が '.' を含む、例 relay.actor / instance.actor / proxy.actor) は
// 'Application' を最優先で返す。それ以外は IsBot なら 'Service'、通常は 'Person'
// (#1956)。Mastodon/Misskey の relay ロジックや actor-type ベースの UI は
// Application を特別扱いするため、wire 上一致させる必要がある。
func actorTypeForUser(u *model.User) string {
	if strings.Contains(u.Username, ".") {
		return "Application"
	}
	if u.IsBot {
		return "Service"
	}
	return "Person"
}

// Renderer converts model entities into AS objects.
type Renderer struct {
	urls            *URLBuilder
	mentionResolver MentionResolver
	fileResolver    FileResolver
	emojiResolver   EmojiResolver
	noteResolver    NoteResolver
	pollResolver    PollResolver
	host            string // ローカルホスト名 (MFM変換用)
	// instanceImages は system actor の icon/banner fallback に使う meta.iconUrl /
	// meta.bannerUrl を実行時に解決する lookup (#1969)。meta は admin が変更しうる
	// ため static でなく lookup で都度取得する。nil なら system fallback 無し。
	instanceImages func() (iconURL, bannerURL string)
	// driveFileMeta は actor の avatar/banner drive file の isSensitive / comment を
	// 解決する lookup (#2031)。upstream renderImage は actor icon/image に
	// sensitive: file.isSensitive / name: file.comment を付ける。nil なら付けない。
	driveFileMeta func(fileID string) (isSensitive bool, comment *string, ok bool)
	// quoteApproval は引用する投稿の承認 URI (FEP-044f の quoteAuthorization) を
	// 引く lookup (#3234)。承認が無ければ空文字列。nil なら付けない。
	quoteApproval func(n *model.Note) (string, error)
}

// NewRenderer constructs a Renderer.
func NewRenderer(urls *URLBuilder) *Renderer {
	return &Renderer{urls: urls}
}

// URLs exposes the URLBuilder so handlers can construct canonical collection
// URLs (outbox/followers/following/featured) without holding a separate
// builder (#1876)。
func (r *Renderer) URLs() *URLBuilder {
	return r.urls
}

// SetQuoteApprovalResolver wires the lookup of a quoting note's approval URI
// (FEP-044f `quoteAuthorization`、#3234)。
func (r *Renderer) SetQuoteApprovalResolver(fn func(n *model.Note) (string, error)) {
	r.quoteApproval = fn
}

// IsQuote reports whether the note is rendered as a quote (a renote with text).
// 引用として配る (`quote` / `_misskey_quote` を付ける) かどうかの唯一の判定で、
// QuoteRequest を送るかどうかもこれに揃える。
func IsQuote(n *model.Note) bool {
	return n.RenoteID != nil && stringValue(n.Text) != ""
}

// SetInstanceImageLookup wires a lookup for meta.iconUrl / meta.bannerUrl used
// as the icon/image fallback for system actors in RenderPerson (#1969).
func (r *Renderer) SetInstanceImageLookup(fn func() (iconURL, bannerURL string)) {
	r.instanceImages = fn
}

// SetDriveFileMetaLookup wires a lookup that resolves an avatar/banner drive
// file's isSensitive / comment, used to populate actor icon/image sensitive/name
// in RenderPerson (#2031). nil disables the enrichment (falls back to {type,url}).
func (r *Renderer) SetDriveFileMetaLookup(fn func(fileID string) (isSensitive bool, comment *string, ok bool)) {
	r.driveFileMeta = fn
}

// applyDriveFileMeta sets img.Sensitive / img.Name from the drive file fileID via
// the driveFileMeta lookup (upstream renderImage: sensitive=file.isSensitive,
// name=file.comment)。fileID 空 / lookup 未配線 / 見つからない時は no-op (#2031)。
func (r *Renderer) applyDriveFileMeta(img *Image, fileID *string) {
	if img == nil || fileID == nil || *fileID == "" || r.driveFileMeta == nil {
		return
	}
	sensitive, comment, ok := r.driveFileMeta(*fileID)
	if !ok {
		return
	}
	img.Sensitive = &sensitive
	if comment != nil {
		img.Name = *comment
	}
}

// RenderOrderedCollection builds an AS OrderedCollection (#1876)。upstream
// ApRendererService.renderOrderedCollection と同じく first/last/orderedItems は
// 空なら省略する。@context は呼び出し側 (handler) が AddContext で付与する。
func (r *Renderer) RenderOrderedCollection(id string, totalItems int, first, last string, orderedItems []any) *OrderedCollection {
	return &OrderedCollection{
		ID:           id,
		Type:         "OrderedCollection",
		TotalItems:   totalItems,
		First:        first,
		Last:         last,
		OrderedItems: orderedItems,
	}
}

// RenderOrderedCollectionPage builds a single OrderedCollectionPage (#1877)。
// upstream renderOrderedCollectionPage と同じく orderedItems は常に出力 (空でも []),
// prev/next は空なら省略する。@context は呼び出し側が AddContext で付与する。
func (r *Renderer) RenderOrderedCollectionPage(id string, totalItems int, orderedItems []any, partOf, prev, next string) *OrderedCollectionPage {
	if orderedItems == nil {
		orderedItems = []any{}
	}
	return &OrderedCollectionPage{
		ID:           id,
		Type:         "OrderedCollectionPage",
		PartOf:       partOf,
		TotalItems:   totalItems,
		OrderedItems: orderedItems,
		Prev:         prev,
		Next:         next,
	}
}

// SetMentionResolver attaches a MentionResolver used by RenderNote to populate
// the `tag` field and additional `to` audience entries. nil で無効化できる。
func (r *Renderer) SetMentionResolver(mr MentionResolver) {
	r.mentionResolver = mr
}

// SetFileResolver attaches a FileResolver for Note attachment rendering.
func (r *Renderer) SetFileResolver(fr FileResolver) {
	r.fileResolver = fr
}

// SetPollResolver attaches a PollResolver for Question (poll) rendering.
func (r *Renderer) SetPollResolver(pr PollResolver) {
	r.pollResolver = pr
}

// SetEmojiResolver attaches an EmojiResolver for custom emoji tags.
func (r *Renderer) SetEmojiResolver(er EmojiResolver) {
	r.emojiResolver = er
}

// SetNoteResolver attaches a NoteResolver for quote renote URI lookup.
func (r *Renderer) SetNoteResolver(nr NoteResolver) {
	r.noteResolver = nr
}

// SetHost sets the local hostname for MFM→HTML conversion.
func (r *Renderer) SetHost(host string) {
	r.host = host
}

// RenderPerson packs a local user into a Person actor object.
// profile は nil でもよい (その場合 summary 等のフィールドは省略される)。
// publicKeyPEM はリポジトリから取得した RSA 公開鍵 PEM 文字列。
// ed25519PublicKey が non-nil の場合は FEP-521a Multikey 形式で
// `assertionMethod[]` に追加 expose する (#1067 / #1069)。Encode 失敗時は
// silently skip して RSA のみ出力する fail-soft 動作。
func (r *Renderer) RenderPerson(u *model.User, profile *model.UserProfile, publicKeyPEM string, ed25519PublicKey ed25519.PublicKey) *Person {
	uri := r.urls.UserURI(u.ID)

	actorType := actorTypeForUser(u)

	p := &Person{
		Object: Object{
			ID:   uri,
			Type: APType(actorType),
			Name: APLenientString(stringValue(u.Name)),
		},
		Inbox:             r.urls.UserInbox(u.ID),
		Outbox:            APLenientID(r.urls.UserOutbox(u.ID)),
		Followers:         APLenientID(r.urls.UserFollowers(u.ID)),
		Following:         APLenientID(r.urls.UserFollowing(u.ID)),
		PreferredUsername: u.Username,
		// url は id (/users/<id>) と区別し、人間向け profile ページ /@<username> を
		// 指す (#1869、upstream renderPerson)。連合向け caller は local user (Host==nil)
		// のみ RenderPerson に渡す前提なので自ドメインの /@username で正しい
		// (admin debug 経路 resolveLocal の host guard 欠落は #1873 で別途対応)。
		URL:         APLenientHref(r.urls.UserProfileURL(u.Username)),
		SharedInbox: APLenientID(r.urls.SharedInbox()), // top-level (#1560)
		Endpoints:   Endpoints{SharedInbox: APLenientID(r.urls.SharedInbox())},
		PublicKey: PublicKey{
			ID:           r.urls.UserKeyURI(u.ID),
			Owner:        APLenientString(uri),
			PublicKeyPEM: APLenientString(publicKeyPEM),
		},
		ManuallyApproves: APTruthyBool(u.IsLocked),
		Discoverable:     APLenientBool(u.IsExplorable),
		IsCat:            APLenientBool(u.IsCat),
	}
	// Ed25519 鍵を持つ user に対しては assertionMethod に Multikey として
	// 追加 expose する。Fedibird など FEP-521a 対応サーバーが Ed25519 鍵で
	// HTTP Signature を verify できるようにするための拡張 (#1067 / #1069)。
	// keyId fragment は `#ed25519-key` を採用 (Fedibird / Mastodon glitch-soc
	// と同じ慣習)。encode error (= key size != 32 byte) は production では
	// 発火しない理論上の defensive path だが、診断のため warn log を出して
	// silent omission を回避する。
	if ed25519PublicKey != nil {
		if mb, err := EncodeEd25519Multikey(ed25519PublicKey); err == nil {
			p.AssertionMethod = MultikeyList{Keys: []Multikey{{
				ID:                 uri + "#ed25519-key",
				Type:               MultikeyType,
				Controller:         uri,
				PublicKeyMultibase: mb,
			}}}
		} else {
			slog.Warn("ed25519 multikey encode failed",
				"userID", u.ID, "error", err)
		}
	}
	// upstream renderPerson の icon/image fallback (#1969):
	//   icon  = avatar ? renderImage(avatar) : isSystem ? renderSystemAvatar : renderIdenticon
	//   image = banner ? renderImage(banner) : isSystem ? renderSystemBanner  : null
	// avatar/banner 未設定の全ローカル actor に icon (identicon) を必ず付ける。
	// system actor (username に '.') は meta.iconUrl / bannerUrl を優先する。
	var instanceIcon, instanceBanner string
	if r.instanceImages != nil {
		instanceIcon, instanceBanner = r.instanceImages()
	}
	isSystem := strings.Contains(u.Username, ".")
	switch {
	case u.AvatarURL != nil && *u.AvatarURL != "":
		p.Icon = &Image{Type: "Image", URL: APLenientHref(*u.AvatarURL)}
		// drive file 由来の avatar には sensitive/name を付ける (#2031)。
		r.applyDriveFileMeta(p.Icon, u.AvatarID)
	case isSystem && instanceIcon != "":
		p.Icon = &Image{Type: "Image", URL: APLenientHref(instanceIcon)}
	default:
		// identicon URL は mk-go の /identicon/<username> route 規則に合わせる
		// (entity.IdenticonURL と同一 seed なので REST avatarUrl と同じ画像)。
		p.Icon = &Image{Type: "Image", URL: APLenientHref(r.urls.baseURL + "/identicon/" + u.Username)}
	}
	switch {
	case u.BannerURL != nil && *u.BannerURL != "":
		p.Image = &Image{Type: "Image", URL: APLenientHref(*u.BannerURL)}
		r.applyDriveFileMeta(p.Image, u.BannerID)
	case isSystem && instanceBanner != "":
		p.Image = &Image{Type: "Image", URL: APLenientHref(instanceBanner)}
	}
	if u.MovedToURI != nil && *u.MovedToURI != "" {
		p.MovedTo = APLenientID(*u.MovedToURI)
	}
	if u.AlsoKnownAs != nil && *u.AlsoKnownAs != "" {
		p.AlsoKnownAs = APIDList(strings.Split(*u.AlsoKnownAs, ","))
	}
	// upstream renderPerson は local actor に必ず featured: ${id}/collections/featured を
	// 出力する。RenderPerson は local user 専用なので無条件に自ドメインの featured
	// collection URI を入れる (pinned note を連合先へ公開する、#1876)。
	p.Featured = APLenientID(r.urls.UserFeatured(u.ID))
	p.MisskeyRequireSigninToViewContents = APLenientBool(u.RequireSigninToViewContents)
	p.MisskeyMakeNotesFollowersOnlyBefore = lenientIntPtr(u.MakeNotesFollowersOnlyBefore)
	p.MisskeyMakeNotesHiddenBefore = lenientIntPtr(u.MakeNotesHiddenBefore)
	// `_misskey_canChat` は CherryPick 互換の chat 連合 capability flag。
	// chatScope == "none" 以外なら true (everyone/followers/following/mutual)
	// と公開する。granular な scope は受信側で local 強制されるので AP には
	// boolean だけ流す (#692)。
	canChat := APLenientBool(u.ChatScope != "none")
	p.MisskeyCanChat = &canChat

	// profile から追加フィールドを埋める
	if profile != nil {
		r.enrichPersonFromProfile(p, profile)
	}

	// ユーザーのカスタム絵文字をEmoji tagとして追加
	r.addEmojiTags(&p.Tag, u.Emojis, nil)
	// user.tags を Hashtag として追加 (#1560、upstream renderHashtag)。
	// href/name 形式は RenderNote の Hashtag と揃える。
	for _, tag := range u.Tags {
		p.Tag = append(p.Tag, Hashtag{
			Type: "Hashtag",
			Href: "https://" + r.host + "/tags/" + tag,
			Name: "#" + tag,
		})
	}

	AddContext(p)
	return p
}

// rewriteFieldURLValue wraps an http(s) PropertyValue field value in an
// `<a rel="me nofollow noopener">` link so non-Misskey clients render it as a
// verifiable profile link (#1560、upstream tryRewriteUrl)。http(s) で始まらない
// 値はそのまま返す。href / text とも HTML escape する。
func rewriteFieldURLValue(value string) string {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return value
	}
	esc := html.EscapeString(value)
	return `<a href="` + esc + `" rel="me nofollow noopener" target="_blank">` + esc + `</a>`
}

// enrichPersonFromProfile fills profile-derived fields into Person.
func (r *Renderer) enrichPersonFromProfile(p *Person, profile *model.UserProfile) {
	// #2106 L50: _misskey_summary / _misskey_followedMessage は raw 値を常時出力する (null/値)。
	// pointer をそのまま渡し、空時も null として出す。Summary (AS2 HTML) は非空時のみ MFM 変換。
	p.MisskeySummary = lenientStringPtr(profile.Description)
	if profile.Description != nil && *profile.Description != "" {
		nodes := mfm.Parse(*profile.Description)
		p.Summary = APLenientString(mfm.ToHTML(nodes, r.host))
	}
	if profile.Birthday != nil && *profile.Birthday != "" {
		p.VcardBday = APLenientString(*profile.Birthday)
	}
	if profile.Location != nil && *profile.Location != "" {
		p.VcardAddress = APLenientString(*profile.Location)
	}
	p.MisskeyFollowedMessage = lenientStringPtr(profile.FollowedMessage)

	// profile.Fields → PropertyValue attachment
	if len(profile.Fields) > 0 {
		var fields []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(profile.Fields, &fields); err == nil && len(fields) > 0 {
			for _, f := range fields {
				p.Attachment = append(p.Attachment, PropertyValue{
					Type:  "PropertyValue",
					Name:  f.Name,
					Value: rewriteFieldURLValue(f.Value),
				})
			}
		}
	}
}

// RenderNote packs a local note into a Note object.
//
// 引用の承認を引けなかったときは、承認の無い引用として描画する (取得や Create を
// 止めない)。承認済みの引用を配り直す Update は RenderNoteUpdate を使うこと —
// 承認の抜けた Update は、相手側で承認済みの引用を未承認に戻す。
func (r *Renderer) RenderNote(n *model.Note, idGen id.Generator) *Note {
	return r.renderNote(n, idGen, r.lenientQuoteApproval)
}

func (r *Renderer) lenientQuoteApproval(n *model.Note) string {
	if r.quoteApproval == nil {
		return ""
	}
	approval, err := r.quoteApproval(n)
	if err != nil {
		slog.Warn("activitypub: quote approval lookup failed; rendering without it", "noteId", n.ID, "err", err)
		return ""
	}
	return approval
}

// strictQuoteApproval resolves the approval up front and fails on lookup errors.
func (r *Renderer) strictQuoteApproval(n *model.Note) (func(*model.Note) string, error) {
	approval := ""
	if IsQuote(n) && r.quoteApproval != nil {
		a, err := r.quoteApproval(n)
		if err != nil {
			return nil, fmt.Errorf("quote approval: %w", err)
		}
		approval = a
	}
	return func(*model.Note) string { return approval }, nil
}

func (r *Renderer) renderNote(n *model.Note, idGen id.Generator, approvalOf func(*model.Note) string) *Note {
	text := stringValue(n.Text)

	// MFM → HTML変換
	var htmlContent string
	var nodes []*mfm.Node
	if text != "" {
		nodes = mfm.Parse(text)
		htmlContent = mfm.ToHTML(nodes, r.host)
	}

	out := &Note{
		Object: Object{
			ID:   r.urls.NoteURI(n.ID),
			Type: "Note",
		},
		AttributedTo: APLenientID(r.urls.UserURI(n.UserID)),
		Content:      htmlContent,
		Published:    APLenientTimestamp(parseNoteTime(n.ID, idGen)),
		// upstream は sensitive = (note.cw != null) || files.some(isSensitive)
		// (#1560、ApRendererService.ts:489)。空文字 CW でも cw!=null なら
		// sensitive=true。file 由来の flip は下の attachment ループで行う。
		Sensitive: n.CW != nil,
	}

	// quote renote の URI を先に解決する (gate と quote-inline span の両方で使う)。
	var quoteURI string
	if IsQuote(n) {
		quoteURI = r.resolveNoteURI(*n.RenoteID)
	}

	// _misskey_content / source: 標準ノードのみなら省略 (TS版 noMisskeyContent)。
	// ただし quote renote は upstream getNoteHtml が extraHtml(!=null) を渡して
	// noMisskeyContent=false を強制するため、simple text でも必ず source/
	// _misskey_content を出す (受信 Misskey が raw markdown を失わないように、#1948-11)。
	hasExtraHTML := quoteURI != ""
	if text != "" && (hasExtraHTML || !mfm.IsSimple(nodes)) {
		out.MisskeyContent = APLenientString(text)
		out.Source = &Source{
			Content:   text,
			MediaType: "text/x.misskeymarkdown",
		}
	}

	if n.CW != nil {
		// upstream は summary = cw==='' ? ZWSP(U+200B) : cw (#1560、
		// ApRendererService.ts:443)。空文字 CW を ZWSP にすることで summary
		// omitempty に落とされず、受信側が「CW あり (本文は空)」と解釈できる。
		if *n.CW == "" {
			out.Summary = "\u200b" // ZWSP (upstream String.fromCharCode(0x200B))
		} else {
			out.Summary = APLenientString(*n.CW)
		}
	}
	if n.ReplyID != nil {
		// リモート note への reply の場合、InReplyTo は相手側 (note.URI) を
		// 指すべき。mk-go の local URL (`https://<self>/notes/<id>`) に
		// してしまうと連合先が `inReplyTo` を解決できず reply chain が切れる
		// (drop-in #369 で発覚)。
		out.InReplyTo = APLenientID(r.resolveNoteURI(*n.ReplyID))
	}

	// Quote renote: text付きrenoteは_misskey_quote + quoteUrlを付ける
	if quoteURI != "" {
		out.MisskeyQuote = APLenientID(quoteURI)
		out.QuoteURL = APLenientID(quoteURI)
		// FEP-044f の `quote` と `quoteAuthorization` は、承認があるときだけ付ける
		// (#3234)。**承認の無いまま `quote` を付けてはいけない** — Mastodon は
		// `quote` のある引用を「承認待ち」として表示し、本文の quote-inline span
		// (RE: リンク) を消す。承認を返さない相手 (本家 Misskey など) への引用は
		// 永久にその表示になる。`quote` が無ければ legacy として RE: リンクが残り、
		// 承認が届いた後の Update で承認済みに切り替わる。
		if approval := approvalOf(n); approval != "" {
			out.Quote = APLenientID(quoteURI)
			out.QuoteAuthorization = APLenientID(approval)
		}
		// 非 Misskey クライアント向けに content 末尾へ quote-inline span を
		// 付ける (#1560、upstream ApRendererService.ts:436-441)。class名
		// `quote-inline` は非 Misskey クライアントの quote 表示に使われる。
		esc := html.EscapeString(quoteURI)
		out.Content += `<br><br><span class="quote-inline">RE: <a href="` + esc + `">` + esc + `</a></span>`
	}

	out.InteractionPolicy = r.quotePolicy(n)

	// 添付ファイル
	r.addAttachments(out, n)

	// Hashtag tags
	for _, tag := range n.Tags {
		out.Tag = append(out.Tag, Hashtag{
			Type: "Hashtag",
			Href: "https://" + r.host + "/tags/" + tag,
			Name: "#" + tag,
		})
	}

	// Custom emoji tags
	r.addEmojiTags(&out.Tag, n.Emojis, nil)

	to, cc := r.addressing(n)

	// Resolve mentions into Tag entries and add mentioned user URIs to the
	// audience so receiving instances (特に Misskey) が mention 通知を作成できる。
	// upstream は mention URI を public/home/followers では cc に、specified では
	// to に置く (#1560、ApRendererService.ts:405-416)。mentionResolver 未設定
	// なら tag は付けない (テスト互換)。
	if r.mentionResolver != nil && len(n.Mentions) > 0 {
		// specified は to、それ以外は cc に追記する。
		mentionToCC := n.Visibility != model.NoteVisibilitySpecified
		seen := make(map[string]struct{}, len(to)+len(cc))
		for _, v := range to {
			seen[v] = struct{}{}
		}
		for _, v := range cc {
			seen[v] = struct{}{}
		}
		for _, uid := range n.Mentions {
			name, uri, err := r.mentionResolver.ResolveMention(uid)
			switch {
			case err == nil:
				// 成功
			case errors.Is(err, ErrMentionUserNotFound):
				// ユーザー削除済 / ID 不整合 — silent skip
				continue
			default:
				// DB 障害等 — log を残して skip。retry 戦略は caller (deliver
				// queue) 側に委ねる。
				slog.Warn("renderer: mention resolution failed",
					"userId", uid, "noteId", n.ID, "err", err)
				continue
			}
			if uri == "" {
				continue
			}
			out.Tag = append(out.Tag, NewMention(uri, name))
			if _, dup := seen[uri]; dup {
				continue
			}
			seen[uri] = struct{}{}
			if mentionToCC {
				cc = append(cc, uri)
			} else {
				to = append(to, uri)
			}
		}
	}

	out.To = to
	out.CC = cc

	// Poll (Question) の asPoll レンダリング。HasPoll が true かつ pollResolver
	// が設定されている場合、Note type を "Question" に変更し、選択肢を oneOf/anyOf
	// として出力する。upstream renderNote の asPoll 部分と同等。
	if n.HasPoll && r.pollResolver != nil {
		if poll, err := r.pollResolver.FindByNoteID(n.ID); err == nil {
			r.applyPoll(out, poll)
		}
	}

	AddContext(out)
	return out
}

// applyPoll converts a Note into a Question by changing the type and populating
// oneOf (single choice) or anyOf (multiple choice) + endTime/closed fields.
func (r *Renderer) applyPoll(out *Note, poll *model.Poll) {
	out.Type = "Question"
	choices := make([]QuestionChoice, len(poll.Choices))
	for i, c := range poll.Choices {
		var count int
		if i < len(poll.Votes) {
			count = int(poll.Votes[i])
		}
		choices[i] = QuestionChoice{
			Type: "Note",
			Name: c,
			Replies: &QuestionChoiceReplies{
				Type:       "Collection",
				TotalItems: APLenientInt(count),
			},
		}
	}
	if poll.Multiple {
		out.AnyOf = choices
	} else {
		out.OneOf = choices
	}
	if poll.ExpiresAt != nil {
		ts := poll.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
		// upstream asPoll は単一キー [expiresAt < now ? 'closed' : 'endTime']: expiresAt
		// で endTime / closed を排他的に出す。期限切れは closed のみ、未期限は endTime
		// のみ (両方は出さない、#1779)。
		if poll.ExpiresAt.Before(time.Now()) {
			out.Closed = APLenientString(ts)
		} else {
			out.EndTime = APLenientString(ts)
		}
	}
}

// addAttachments loads drive files and adds Document entries to the note.
func (r *Renderer) addAttachments(out *Note, n *model.Note) {
	if r.fileResolver == nil || len(n.FileIDs) == 0 {
		return
	}
	files, err := r.fileResolver.FindByIDs(n.FileIDs)
	if err != nil {
		slog.Warn("renderer: failed to load drive files", "noteId", n.ID, "err", err)
		return
	}
	for _, f := range files {
		// upstream renderDocument は mediaType: file.webpublicType ?? file.type、
		// url: getPublicUrl(file) (= file.webpublicUrl ?? file.url) で web 最適化
		// variant を優先する。連合先に転送する帯域/形式を抑えるため mk-go も揃える
		// (original URL も有効なので最適化目的、#1779)。
		mediaType := f.Type
		if f.WebpublicType != nil && *f.WebpublicType != "" {
			mediaType = *f.WebpublicType
		}
		fileURL := f.URL
		if f.WebpublicURL != nil && *f.WebpublicURL != "" {
			fileURL = *f.WebpublicURL
		}
		doc := Document{
			Type:      "Document",
			MediaType: mediaType,
			URL:       fileURL,
			Name:      stringValue(f.Comment),
			Sensitive: f.IsSensitive,
		}
		// upstream renderDocument は file.properties?.width / height を Document に
		// 載せる (画像の寸法メタデータ、upstream #17563)。drive_file.properties は
		// `{"width":..,"height":..}` jsonb。非画像は width/height を持たず omitempty で
		// 出ない。
		if w, h := driveImageDimensions([]byte(f.Properties)); w > 0 || h > 0 {
			doc.Width = w
			doc.Height = h
		}
		out.Attachment = append(out.Attachment, doc)
		if f.IsSensitive {
			out.Sensitive = true
		}
	}
}

// driveImageDimensions extracts width/height from a drive_file.properties jsonb
// (`{"width":..,"height":..}`)。値が無い / parse 失敗 / 非画像なら 0,0 を返す (#2069)。
func driveImageDimensions(props []byte) (int, int) {
	if len(props) == 0 {
		return 0, 0
	}
	var p struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return 0, 0
	}
	return p.Width, p.Height
}

// resolveNoteURI returns the canonical URI for a note (remote URI or local).
func (r *Renderer) resolveNoteURI(noteID string) string {
	if r.noteResolver != nil {
		if note, err := r.noteResolver.FindByID(noteID); err == nil {
			if note.URI != nil && *note.URI != "" {
				return *note.URI
			}
		}
	}
	return r.urls.NoteURI(noteID)
}

// addEmojiTags resolves custom emoji names and appends EmojiTag entries.
func (r *Renderer) addEmojiTags(tags *APObjectList, emojiNames []string, host *string) {
	if r.emojiResolver == nil || len(emojiNames) == 0 {
		return
	}
	for _, name := range emojiNames {
		emoji, err := r.emojiResolver.FindByNameAndHost(name, host)
		if err != nil {
			continue
		}
		// localOnly 絵文字は『この instance 限定』の明示意図なので AP tag から
		// 除外する (upstream renderNote/renderPerson の emojis.filter(e => !e.localOnly)
		// / renderLike の !emoji.localOnly と同じ、#1868)。FindByNameAndHost は
		// 表示/リアクション経路とも共有するので gate は render 側に置く。
		if emoji.LocalOnly {
			continue
		}
		// upstream renderEmoji の icon: mediaType = emoji.type ?? 'image/png'、
		// url = publicUrl || originalUrl (publicUrl 空時の後方互換 fallback) (#1948-11)。
		iconURL := emoji.PublicURL
		if iconURL == "" {
			iconURL = emoji.OriginalURL
		}
		mediaType := "image/png"
		if emoji.Type != nil && *emoji.Type != "" {
			mediaType = *emoji.Type
		}
		tag := EmojiTag{
			Type: "Emoji",
			Name: ":" + emoji.Name + ":",
			Icon: Image{Type: "Image", MediaType: mediaType, URL: APLenientHref(iconURL)},
		}
		// id: remote emoji は元 URI を保持、local emoji は自ドメインの
		// /emojis/<name> (upstream renderEmoji は常に local URL を出すが、remote
		// emoji の参照同一性を壊さないため URI を優先する保守的方針、#1948-11)。
		if emoji.URI != nil {
			tag.ID = *emoji.URI
		} else {
			tag.ID = r.urls.baseURL + "/emojis/" + emoji.Name
		}
		// updated: upstream renderEmoji は updatedAt?.toISOString() ?? now を常に
		// 出力する。受信側 instance が cache 済 emoji を再取得するか判定するため、
		// local emoji でも必ず付与する (#1948-11)。
		if emoji.UpdatedAt != nil {
			tag.Updated = emoji.UpdatedAt.UTC().Format(publishedLayout)
		} else {
			tag.Updated = time.Now().UTC().Format(publishedLayout)
		}
		// #731: upstream Misskey TS の renderEmoji と同じく `_misskey_license`
		// を出力する。受信側 mk-go / Misskey TS は wrapper を見て license を
		// 取り込む (extractEmojiTags)。emoji.License が nil でも、wrapper を
		// 出力して FreeText: null を明示することで「license 未設定」を
		// 連合先に伝える (upstream renderer の挙動と一致)。
		tag.License = &MisskeyLicense{FreeText: emoji.License}
		*tags = append(*tags, tag)
	}
}

// RenderQuestionUpdate builds the AP `Update(Question)` activity broadcast to
// followers when a local poll receives a vote (#690)。Misskey TS の
// PollService.deliverQuestionUpdate と同等で、配信先 instance が remote
// follower 視点で count を最新化できる。RenderNote が既に Question 互換の
// asPoll 出力をするので、それを wrap するだけ。
//
// Activity ID は `<noteURI>#updates/<RFC3339Nano>` で構成し、同一秒内に
// 連続投票が発生した場合の Activity ID 衝突を防ぐ。受信側 instance は
// Activity ID で idempotency dedup するため、衝突するとうしろ側 Update が
// drop されて count が古いまま固定される (#690 review)。
func (r *Renderer) RenderQuestionUpdate(n *model.Note, idGen id.Generator) (*Update, error) {
	return r.RenderNoteUpdate(n, idGen)
}

// RenderNoteUpdate builds an `Update` wrapping the current rendering of a local
// note. `updated` を付けないので、Mastodon は編集ではなく付随情報 (引用の承認・
// 引用してよい範囲・投票数) の更新として扱う (#3234)。
//
// 引用の承認を引けなければ error を返す。承認の抜けた Update を受けた Mastodon は、
// 承認済みの引用を未承認に戻す (承認 URI が変わったとみなす)。
func (r *Renderer) RenderNoteUpdate(n *model.Note, idGen id.Generator) (*Update, error) {
	approvalOf, err := r.strictQuoteApproval(n)
	if err != nil {
		return nil, err
	}
	obj := r.renderNote(n, idGen, approvalOf)
	// inner Note / Question の @context は outer Update に集約する (RenderCreate /
	// RenderUpdate と同じパターン、#2510)。
	obj.Context = nil
	// Activity ID は同一秒内の連続更新でも衝突しないよう nano 精度を保つが、
	// published は upstream toISOString() の .000Z 形式に揃える (#1948-11)。
	t := time.Now().UTC()
	u := &Update{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.NoteURI(n.ID) + "#updates/" + t.Format(time.RFC3339Nano),
				Type: "Update",
			},
			Actor:     r.urls.UserURI(n.UserID),
			Published: t.Format(publishedLayout),
			To:        obj.To,
			CC:        obj.CC,
		},
		Object: obj,
	}
	AddContext(u)
	return u, nil
}

// RenderVote builds the AP `Create(Note)` activity used to deliver a poll
// vote to the remote poll author's inbox (#690)。Misskey TS の vote wire
// format に合わせて Note は `name = <choice>` + `inReplyTo = <poll URI>` +
// 空 `content` で構成する。target は voted-on poll note (remote)。targetURI
// はリモート側の note.URI を使う (local URL で render すると相手が解決でき
// ない)。authorURI は target.UserID から URLBuilder 経由で組み立てる。
func (r *Renderer) RenderVote(voter *model.User, target *model.Note, targetURI, authorURI, choiceName string) *Create {
	now := time.Now().UTC().Format(publishedLayout)
	noteID := r.urls.VoteActivityURI(voter.ID, target.ID)
	// upstream renderVote の object Note は {id, type, attributedTo, to, inReplyTo, name}
	// のみ。汎用 Note 構造体は content/published(omitempty 無し) を必ず出力してしまい
	// upstream が送らない余剰フィールドが乗るため、最小 struct で組む (#1948-11)。
	note := &voteNote{
		ID:           noteID,
		Type:         "Note",
		AttributedTo: r.urls.UserURI(voter.ID),
		To:           []string{authorURI},
		InReplyTo:    targetURI,
		Name:         choiceName,
	}
	c := &Create{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.CreateActivityURI(noteID),
				Type: "Create",
			},
			Actor:     r.urls.UserURI(voter.ID),
			Published: now,
			To:        []string{authorURI},
		},
		Object: note,
	}
	AddContext(c)
	return c
}

// voteNote is the minimal AP Note shape upstream renderVote emits for a poll
// vote: exactly {id, type, attributedTo, to, inReplyTo, name} with no content /
// published / sensitive (#1948-11).
type voteNote struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	AttributedTo string   `json:"attributedTo"`
	To           []string `json:"to"`
	InReplyTo    string   `json:"inReplyTo"`
	Name         string   `json:"name"`
}

// RenderCreate wraps a Note into a Create activity addressed to the same audience.
func (r *Renderer) RenderCreate(n *model.Note, idGen id.Generator) *Create {
	note := r.RenderNote(n, idGen)
	// inner Note の @context は outer Create に集約するため除去する (upstream
	// renderNote は bare object を返し、@context は top-level のみ。
	// RenderUpdate と同じパターン、#2510)。
	note.Context = nil
	c := &Create{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.CreateActivityURI(n.ID),
				Type: "Create",
			},
			Actor:     r.urls.UserURI(n.UserID),
			Published: note.Published.String(),
			To:        note.To,
			CC:        note.CC,
		},
		Object: note,
	}
	AddContext(c)
	return c
}

// RenderFollow returns a Follow activity from follower → followeeURI.
func (r *Renderer) RenderFollow(followerID, followeeURI string) *Follow {
	f := &Follow{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.FollowURI(followerID, followeeURI),
				Type: "Follow",
			},
			Actor: r.urls.UserURI(followerID),
		},
		Object: followeeURI,
	}
	AddContext(f)
	return f
}

// RenderUpdate wraps a rendered Person in an Update activity, delivered to
// followers when a local user edits their profile so remote instances refresh
// the actor (#1560、upstream AccountUpdateService renderUpdate(renderPerson))。
// inner Person の @context は outer Update に集約するため除去する。
func (r *Renderer) RenderUpdate(person *Person) *Update {
	actor := person.ID
	person.Context = nil
	u := &Update{
		Activity: Activity{
			Object: Object{
				ID:   actor + "#updates/" + uuid.NewString(),
				Type: "Update",
			},
			Actor:     actor,
			To:        []string{Public},
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: person,
	}
	AddContext(u)
	return u
}

// RenderBlock returns a Block activity (actor=blocker, object=blockee URI),
// delivered when a local user blocks a remote user (#1560、upstream
// renderBlock + UserBlockingService.block delivery)。
func (r *Renderer) RenderBlock(blockerID, blockeeURI string) *Block {
	bl := &Block{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.BlockURI(blockerID, blockeeURI),
				Type: "Block",
			},
			Actor: r.urls.UserURI(blockerID),
		},
		Object: blockeeURI,
	}
	AddContext(bl)
	return bl
}

// RenderUndoBlock wraps a Block in an Undo, delivered when a local user
// unblocks a remote user (#1560、upstream renderUndo(renderBlock(...)))。
func (r *Renderer) RenderUndoBlock(blockerID, blockeeURI string) *Undo {
	inner := r.RenderBlock(blockerID, blockeeURI)
	inner.Context = nil // inner activity は @context を持たない
	u := &Undo{
		Activity: Activity{
			Object: Object{
				ID:   inner.ID + "/undo",
				Type: "Undo",
			},
			Actor:     r.urls.UserURI(blockerID),
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: inner,
	}
	AddContext(u)
	return u
}

// RenderUndoFollow wraps a Follow activity in an Undo for a local→remote
// unfollow. upstream renderUndo は全ての Undo に published を付けるため、ここでも
// .000Z published を入れる (#1993)。inner Follow の @context は upstream 同様
// 落とす (outer Undo のみ @context を持つ)。
func (r *Renderer) RenderUndoFollow(followerID, followeeURI string) *Undo {
	inner := r.RenderFollow(followerID, followeeURI)
	inner.Context = nil
	u := &Undo{
		Activity: Activity{
			Object: Object{
				ID:   inner.ID + "/undo",
				Type: "Undo",
			},
			Actor:     r.urls.UserURI(followerID),
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: inner,
	}
	AddContext(u)
	return u
}

// RenderFollowRelay returns the special-purpose Follow activity used to
// subscribe the instance's relay system actor to a relay endpoint.
//
// Mirrors upstream ApRendererService.renderFollowRelay:
//   - id: `${baseURL}/activities/follow-relay/${relayID}`
//   - actor: relay actor's local URI
//   - object: the ActivityStreams Public IRI (= subscribe to everything)
//
// The id format is load-bearing — inbox Accept/Reject activities are
// matched against `/activities/follow-relay/([\w-]+)$` to look the
// relay up when marking its status.
func (r *Renderer) RenderFollowRelay(relayID string, relayActorID string) *Follow {
	f := &Follow{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.FollowRelayURI(relayID),
				Type: "Follow",
			},
			Actor: r.urls.UserURI(relayActorID),
		},
		Object: Public,
	}
	AddContext(f)
	return f
}

// RenderAccept wraps an inner activity in an Accept.
//
// id は Misskey TS `addContext` 互換で `{baseURL}/{UUID}` 形式で必ず入れる。
// id なしの Accept は受信側 (Misskey / CherryPick) の InboxProcessorService
// で "skip: activity id is not a string" として弾かれるため、フォロー承認
// フローが成立しない。
func (r *Renderer) RenderAccept(actorID string, inner any) *Accept {
	a := &Accept{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.baseURL + "/" + uuid.NewString(),
				Type: "Accept",
			},
			Actor: r.urls.UserURI(actorID),
		},
		Object: inner,
	}
	AddContext(a)
	return a
}

// quotePolicy advertises who may quote the note (FEP-044f、#3234)。
//
// 公開範囲より広く引用させない (FEP の推奨)。home (未収載) は公開と同じく誰でも
// 引用できる (Mastodon の未収載と同じ)。**誰も引用できないときは作者だけを
// 入れる** — 空配列は JSON-LD で「項目が無い」と同じになり、無いときの解釈は
// 相手次第になる (FEP の指示)。
func (r *Renderer) quotePolicy(n *model.Note) *InteractionPolicy {
	var allowed []string
	switch n.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome:
		allowed = []string{Public}
	case model.NoteVisibilityFollowers:
		allowed = []string{r.urls.UserFollowers(n.UserID)}
	default:
		allowed = []string{r.urls.UserURI(n.UserID)}
	}
	return &InteractionPolicy{CanQuote: &InteractionRule{AutomaticApproval: allowed}}
}

// RenderQuoteAuthorization returns the approval stamp for a quote of a local
// note (FEP-044f、#3234)。引用する投稿も引用される投稿も URI だけを入れ、埋め
// 込まない (FEP の MUST NOT。見る権限の無い相手へ中身を渡さないため)。
func (r *Renderer) RenderQuoteAuthorization(note *model.Note, a *model.NoteQuoteAuthorization) *QuoteAuthorization {
	out := &QuoteAuthorization{
		Object: Object{
			ID:   r.urls.QuoteAuthorizationURI(note.ID, a.ID),
			Type: "QuoteAuthorization",
		},
		AttributedTo:      r.urls.UserURI(note.UserID),
		InteractingObject: a.QuotingURI,
		InteractionTarget: r.urls.NoteURI(note.ID),
	}
	AddContext(out)
	return out
}

// RenderQuoteRequestAccept returns the Accept for a QuoteRequest (FEP-044f、
// #3234)。object は受け取った QuoteRequest を id で指せる形に組み直し、result に
// 承認の URI を入れる。Mastodon は object の id で自分の引用を引き当てる。
func (r *Renderer) RenderQuoteRequestAccept(note *model.Note, a *model.NoteQuoteAuthorization, quoterURI string) *Accept {
	acc := r.RenderAccept(note.UserID, r.quoteRequestObject(note, a, quoterURI))
	acc.Result = r.urls.QuoteAuthorizationURI(note.ID, a.ID)
	return acc
}

// RenderQuoteRequestReject returns the Reject for a QuoteRequest (FEP-044f)。
func (r *Renderer) RenderQuoteRequestReject(note *model.Note, requestID, quotingURI, quoterURI string) *Reject {
	return r.RenderReject(note.UserID, map[string]any{
		"id":         requestID,
		"type":       "QuoteRequest",
		"actor":      quoterURI,
		"object":     r.urls.NoteURI(note.ID),
		"instrument": quotingURI,
	})
}

// RenderQuoteRequest returns the QuoteRequest a local quoting note sends to the
// author of the remote note it quotes (FEP-044f、#3234)。instrument に引用する
// 投稿を埋め込む — Mastodon は届いていない投稿を取りに来ずにそれを取り込める
// (Create より先に届いても答えられる)。
func (r *Renderer) RenderQuoteRequest(n *model.Note, quotedURI string, idGen id.Generator) *QuoteRequest {
	instrument := r.RenderNote(n, idGen)
	instrument.Context = nil
	q := &QuoteRequest{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.QuoteRequestURI(n.ID),
				Type: "QuoteRequest",
			},
			Actor: r.urls.UserURI(n.UserID),
		},
		Object:     quotedURI,
		Instrument: instrument,
	}
	AddContext(q)
	return q
}

func (r *Renderer) quoteRequestObject(note *model.Note, a *model.NoteQuoteAuthorization, quoterURI string) map[string]any {
	obj := map[string]any{
		"type":       "QuoteRequest",
		"actor":      quoterURI,
		"object":     r.urls.NoteURI(note.ID),
		"instrument": a.QuotingURI,
	}
	if a.RequestID != nil {
		obj["id"] = *a.RequestID
	}
	return obj
}

// RenderReject returns a Reject activity wrapping the given inner object
// (typically a Follow). Used for:
//   - local user rejecting an inbound Follow request
//   - local user removing an existing remote follower (invalidate)
//
// 本家 UserFollowingService と同じく actor=rejecting user、object=original Follow。
func (r *Renderer) RenderReject(actorID string, inner any) *Reject {
	a := &Reject{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.baseURL + "/" + uuid.NewString(),
				Type: "Reject",
			},
			Actor: r.urls.UserURI(actorID),
		},
		Object: inner,
	}
	AddContext(a)
	return a
}

// RenderChatRoom returns the ActivityStreams `Group` object representing a
// chat room, used as the object of Invite / Accept / Reject activities in
// CherryPick group chat federation. Mirrors ApRendererService.renderChatRoom.
func (r *Renderer) RenderChatRoom(room *model.ChatRoom) *ChatRoomGroup {
	g := &ChatRoomGroup{
		Object: Object{
			ID:   r.urls.ChatRoomURI(room.ID),
			Type: "Group",
			Name: APLenientString(room.Name),
		},
		AttributedTo: r.urls.UserURI(room.OwnerID),
	}
	if room.Description != "" {
		g.Summary = room.Description
	}
	return g
}

// RenderInvite wraps a chat room Group object in an Invite activity targeted
// at the invitee's actor URI. Mirrors ApRendererService.renderInvite.
//
// id は RenderAccept と同じく addContext 互換で `{baseURL}/{UUID}` を必ず入れる
// (id 無し activity は受信側 InboxProcessor で弾かれる)。actor は room owner。
func (r *Renderer) RenderInvite(ownerID string, inner any, targetURI string) *Invite {
	inv := &Invite{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.baseURL + "/" + uuid.NewString(),
				Type: "Invite",
			},
			Actor: r.urls.UserURI(ownerID),
		},
		Object: inner,
		Target: targetURI,
	}
	AddContext(inv)
	return inv
}

// RenderChatRoomRemove builds a Remove activity announcing that the user at
// leavingUserURI left the chat room at roomURI. Mirrors cherrypick
// ChatService.leaveRoom's renderRemove: actor = object = the leaving user,
// target = the room. id は addContext 互換で `{baseURL}/{UUID}` を必ず入れる
// (id 無し activity は受信側 InboxProcessor で弾かれる)。
// RenderAddToFeatured builds an Add activity announcing that a local user pinned
// a note into their featured collection (upstream NotePiningService.deliverPinnedChange
// + renderAdd、#2024)。target=featured collection、object=note URI。
func (r *Renderer) RenderAddToFeatured(userID, noteID string) *Add {
	a := &Add{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.baseURL + "/" + uuid.NewString(),
				Type: "Add",
			},
			Actor: r.urls.UserURI(userID),
		},
		Object: r.urls.NoteURI(noteID),
		Target: r.urls.UserFeatured(userID),
	}
	AddContext(a)
	return a
}

// RenderRemoveFromFeatured builds a Remove activity announcing that a local user
// unpinned a note from their featured collection (upstream renderRemove、#2024)。
func (r *Renderer) RenderRemoveFromFeatured(userID, noteID string) *Remove {
	rm := &Remove{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.baseURL + "/" + uuid.NewString(),
				Type: "Remove",
			},
			Actor: r.urls.UserURI(userID),
		},
		Object: r.urls.NoteURI(noteID),
		Target: r.urls.UserFeatured(userID),
	}
	AddContext(rm)
	return rm
}

func (r *Renderer) RenderChatRoomRemove(leavingUserURI, roomURI string) *Remove {
	rm := &Remove{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.baseURL + "/" + uuid.NewString(),
				Type: "Remove",
			},
			Actor: leavingUserURI,
		},
		Object: leavingUserURI,
		Target: roomURI,
	}
	AddContext(rm)
	return rm
}

// RenderChatRoomInviteRef reconstructs a room owner's original Invite as a
// nested object, used as the `object` of an invitee's Accept / Reject when the
// room (and its owner) are remote. All URIs are passed explicitly because they
// are the remote canonical URIs, not local ones; no id/@context is set since
// the result is nested inside an Accept/Reject that carries its own context.
func (r *Renderer) RenderChatRoomInviteRef(ownerURI, roomURI, roomName, targetURI string) *Invite {
	return &Invite{
		Activity: Activity{
			Object: Object{Type: "Invite"},
			Actor:  ownerURI,
		},
		Object: &ChatRoomGroup{
			Object:       Object{ID: roomURI, Type: "Group", Name: APLenientString(roomName)},
			AttributedTo: ownerURI,
		},
		Target: targetURI,
	}
}

// RenderLike returns a Like activity for the given reaction.
// targetURI は対象ノートの canonical URI (リモートなら note.URI、ローカルなら
// urls.NoteURI(note.ID))。
//
// reaction が custom emoji 形式 (`:name:` / `:name@.:` / `:name@host:`) の
// 場合は addEmojiTags で Like.Tag に Emoji オブジェクトを 1 件詰める。
// 連合相手側 (mk-go 受信側 / Misskey TS / CherryPick) は extractEmojiTags
// で Tag を拾って icon URL を local emoji table に upsert する設計なので、
// ここで Tag を出さないと remote 側で custom emoji 解決できず、heart
// fallback / 表示なしになる (#634)。Unicode 絵文字や fallback heart は
// `:` で囲まれていないので Tag 不要。
func (r *Renderer) RenderLike(reactor *model.User, targetURI string, reaction string, likeID string) *Like {
	l := &Like{
		Activity: Activity{
			Object: Object{
				ID:   likeID,
				Type: "Like",
			},
			Actor: r.urls.UserURI(reactor.ID),
		},
		Object:          targetURI,
		Content:         reaction,
		MisskeyReaction: reaction,
	}
	if name, host, ok := parseCustomEmojiReactionForTag(reaction); ok {
		var hostPtr *string
		if host != "" {
			h := host
			hostPtr = &h
		}
		r.addEmojiTags(&l.Tag, []string{name}, hostPtr)
	}
	AddContext(l)
	return l
}

// parseCustomEmojiReactionForTag extracts (name, host) from a canonical
// reaction string for emoji Tag emission. Returns ok=false for unicode
// (no surrounding colons) and fallback reactions. Local canonical "@."
// suffix is normalised to host="".
//
// activitypub 層に entity.parseCustomEmojiReaction と等価のミニ実装を持
// たせる: layer 規則上 activitypub → entity の依存は禁止 (entity →
// activitypub.Renderer の逆向き依存は許容) なので、両側で別々に持つ。
func parseCustomEmojiReactionForTag(s string) (name, host string, ok bool) {
	if len(s) < 3 || s[0] != ':' || s[len(s)-1] != ':' {
		return "", "", false
	}
	inner := s[1 : len(s)-1]
	if at := strings.Index(inner, "@"); at >= 0 {
		host = inner[at+1:]
		if host == "." {
			host = ""
		}
		return inner[:at], host, true
	}
	return inner, "", true
}

// RenderUndoLike wraps a previously emitted Like in an Undo activity.
func (r *Renderer) RenderUndoLike(reactor *model.User, like *Like) *Undo {
	u := &Undo{
		Activity: Activity{
			Object: Object{
				ID:   like.ID + "/undo",
				Type: "Undo",
			},
			Actor: r.urls.UserURI(reactor.ID),
			// upstream renderUndo は published を常に付与する (#1948-11)。
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: like,
	}
	AddContext(u)
	return u
}

// RenderAnnounce builds an Announce (boost) activity for a pure renote.
// targetURI は元ノートの URI (リモート / ローカル)、renoteID は renote 自身の ID。
// to/cc は renote 自身の visibility 由来で決める (upstream renderAnnounce、#1882):
//   - public:    to=[Public],    cc=[followers]
//   - home:      to=[followers], cc=[Public]
//   - followers: to=[followers], cc=[]
//
// それ以外 (specified 等) は public 扱いにフォールバックする。なお specified な
// pure renote を federate しない upstream の throw 相当の suppression は別スコープ
// (#1886 参照)。
func (r *Renderer) RenderAnnounce(renoter *model.User, renoteID, targetURI string, visibility model.NoteVisibility, idGen id.Generator) *Announce {
	followers := r.urls.UserFollowers(renoter.ID)
	to, cc := []string{Public}, []string{followers}
	switch visibility {
	case model.NoteVisibilityHome:
		to, cc = []string{followers}, []string{Public}
	case model.NoteVisibilityFollowers:
		to, cc = []string{followers}, nil
	}
	// published は upstream renderAnnounce と同じく renote 自身の id から導出
	// する (idService.parse(note.id).date.toISOString() 相当、#2510)。id を
	// 解析できない場合は omitempty で省く (upstream は throw だが、配送全体を
	// 落とすより published 無しの valid な shape の方がよい)。
	published := ""
	if idGen != nil {
		if t, err := idGen.ParseTime(renoteID); err == nil {
			published = t.UTC().Format(publishedLayout)
		}
	}
	a := &Announce{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.NoteURI(renoteID) + "/activity",
				Type: "Announce",
			},
			Actor:     r.urls.UserURI(renoter.ID),
			Published: published,
			To:        to,
			CC:        cc,
		},
		Object: targetURI,
	}
	AddContext(a)
	return a
}

// RenderUndoAnnounce wraps a previously emitted Announce in an Undo activity.
func (r *Renderer) RenderUndoAnnounce(renoter *model.User, announce *Announce) *Undo {
	u := &Undo{
		Activity: Activity{
			Object: Object{
				ID:   announce.ID + "/undo",
				Type: "Undo",
			},
			Actor: r.urls.UserURI(renoter.ID),
			// upstream renderUndo は published を常に付与する (#1948-11)。
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: announce,
	}
	AddContext(u)
	return u
}

// RenderUndoAnnounceForNote builds the Undo(Announce) emitted when a local
// pure renote (boost) is deleted. Mirrors upstream NoteDeleteService
// (`renderUndo(renderAnnounce(renote.uri ?? local, note), user)`).
//
// **Delete(Tombstone) では取り消せない。** Announce の id は
// `<noteURI>/activity` で、受信側はその id を renote の uri として保存する
// (mk-go 自身も processor.go が `renote.URI = act.ID` とする)。Tombstone の
// id は `/activity` の付かない note URI なので一致せず、ブーストが相手側に
// 残り続ける。
//
// renote 先の URI は noteResolver で解決する (リモート note のブーストでは
// 相手側の URI でないと mk-go 受信側の handleUndoAnnounce が対象を引けない)。
// idGen が nil のときは inner Announce の published を省く (omitempty)。
func (r *Renderer) RenderUndoAnnounceForNote(renoter *model.User, renote *model.Note, idGen id.Generator) *Undo {
	if renoter == nil || renote == nil || renote.RenoteID == nil || *renote.RenoteID == "" {
		return nil
	}
	announce := r.RenderAnnounce(renoter, renote.ID, r.resolveNoteURI(*renote.RenoteID), renote.Visibility, idGen)
	// inner Announce の @context は outer Undo に集約する (RenderQuestionUpdate
	// と同じパターン、#2510)。
	announce.Context = nil
	return r.RenderUndoAnnounce(renoter, announce)
}

// RenderDelete returns a Delete activity targeting the note URI. Object is a
// Tombstone so receivers can match it to a previously known note.
func (r *Renderer) RenderDelete(author *model.User, noteURI string) *Delete {
	d := &Delete{
		Activity: Activity{
			Object: Object{
				ID:   noteURI + "#Delete",
				Type: "Delete",
			},
			Actor: r.urls.UserURI(author.ID),
			// **to を付けない (upstream ApRendererService.renderDelete と同じ)。**
			// 実配送先は inbox の一覧で決まるので受信者集合は変わらないが、
			// to:[Public] のままだと followers/specified なノートの URI を
			// 参照する activity が公開宛てとして出てしまう。
			// upstream renderDelete は published を常に付与する (#1948-11)。
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: Tombstone{
			ID:   noteURI,
			Type: "Tombstone",
		},
	}
	AddContext(d)
	return d
}

// RenderDeleteActor renders a Delete activity whose object is a local user's own
// actor URI, used when an account is suspended or deleted (#1759). Mirrors
// upstream ApRendererService.renderDelete(genLocalUserUri(user.id), user): both
// actor と object が当該ユーザーの URI 文字列で、Tombstone ではなく bare IRI を
// object に置く (note の Delete と違い actor delete は Tombstone を使わない)。
func (r *Renderer) RenderDeleteActor(user *model.User) *Delete {
	uri := r.urls.UserURI(user.ID)
	d := &Delete{
		Activity: Activity{
			Object: Object{
				ID:   uri + "#Delete",
				Type: "Delete",
			},
			Actor: uri,
			To:    []string{Public},
			// upstream renderDelete は published を常に付与する (#1948-11)。
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: uri,
	}
	AddContext(d)
	return d
}

// RenderUndoDeleteActor wraps RenderDeleteActor in an Undo, used when a suspended
// account is unsuspended (#1759). Mirrors upstream renderUndo(renderDelete(...))。
// 内側 Delete は @context を持たない (Undo 側のみ context を付ける)。
func (r *Renderer) RenderUndoDeleteActor(user *model.User) *Undo {
	uri := r.urls.UserURI(user.ID)
	inner := r.RenderDeleteActor(user)
	inner.Context = nil // inner activity は @context を持たない
	u := &Undo{
		Activity: Activity{
			Object: Object{
				ID:   uri + "#Undo-Delete",
				Type: "Undo",
			},
			Actor: uri,
			To:    []string{Public},
			// upstream renderUndo は published を常に付与する (#1948-11)。
			Published: time.Now().UTC().Format(publishedLayout),
		},
		Object: inner,
	}
	AddContext(u)
	return u
}

// RenderFlag returns a Flag activity that forwards an abuse report to the
// origin instance of a remote user. Mirrors upstream ApRendererService
// renderFlag — the actor is the local instance's system account (to
// anonymize the reporter), object is the target user's canonical URI, and
// content is the moderator-visible comment.
func (r *Renderer) RenderFlag(actor *model.User, targetURI, content string) *Flag {
	f := &Flag{
		Activity: Activity{
			// upstream AbuseReportService は renderFlag を addContext で包み、
			// addContext が id 不在時に `{config.url}/{uuid}` を必ず注入する。
			// id を持たない activity は受信側 InboxProcessor に
			// "activity id is not a string" で silent drop されるため、
			// RenderAccept/Reject/Invite と同様にここで id を埋める (#1951)。
			Object: Object{
				ID:   r.urls.baseURL + "/" + uuid.NewString(),
				Type: "Flag",
			},
			Actor: r.urls.UserURI(actor.ID),
		},
		Object:  targetURI,
		Content: content,
	}
	AddContext(f)
	return f
}

// RenderMove returns a Move activity announcing that src has migrated to
// dstURI. Matches upstream ApRendererService.renderMove: actor == object ==
// srcURI, target == dstURI. ID is "{baseURL}/moves/{srcID}/{dstID}" where
// dstID is derived from dstURI (final path segment) so that receiving
// servers see a deterministic activity id per (src, dst) pair.
//
// To は follower collection URI を指す。DeliverToFollowers で各 follower の
// inbox へ直接配送するため To だけでは配送経路は決まらないが、受信側の一部
// AP 実装は to/cc を見て visibility を判定するため (Announce / Delete と同様)
// 明示的に addressing を付ける。
func (r *Renderer) RenderMove(src *model.User, dstURI string) *Move {
	srcURI := r.urls.UserURI(src.ID)
	m := &Move{
		Activity: Activity{
			Object: Object{
				ID:   r.urls.MoveURI(src.ID, dstURI),
				Type: "Move",
			},
			Actor: srcURI,
			To:    []string{r.urls.UserFollowers(src.ID)},
		},
		Object: srcURI,
		Target: dstURI,
	}
	AddContext(m)
	return m
}

// setChatMessageContent fills the AP `content` of a chat message note with the
// same MFM -> HTML conversion RenderNote performs, and mirrors its
// `source` / `_misskey_content` handling.
//
// `content` は mediaType を出していないので AS2 既定の text/html として解釈
// される。生の本文をそのまま入れると `<script>` や `onerror=` が受信側の
// content に HTML として載る (chat には入力時のサニタイズも無い)。RenderNote
// と同じ mfm.Parse -> mfm.ToHTML を通すことで、text ノードは
// html.EscapeString され、MFM は upstream と同じ HTML になる。
//
// **source は常に出す (レビュー M1)。** RenderNote は「標準ノードだけなら
// 省略」するが、`IsSimple` はカスタム絵文字 (`:name:`) を simple 扱いする一方、
// `ToHTML` は絵文字の前後に `\u200b` を書く。省略すると受信側は HTML から
// 戻すしかなく、**絵文字 1 個につきゼロ幅スペースが 2 文字混入した本文が
// 保存される**。この PR より前は `content` が MFM 原文だったので混入しなかった
// ので、省略したままだと chat がノートの既知の欠陥を新しく引き継ぐことになる。
func (r *Renderer) setChatMessageContent(note *Note, text *string) {
	if text == nil || *text == "" {
		return
	}
	nodes := mfm.Parse(*text)
	note.Content = mfm.ToHTML(nodes, r.host)
	note.MisskeyContent = APLenientString(*text)
	note.Source = &Source{
		Content:   *text,
		MediaType: "text/x.misskeymarkdown",
	}
}

// RenderChatMessage returns a CherryPick-compatible Create activity wrapping
// a Note flagged with `_misskey_talk: true`. ApRendererService.renderChatMessage
// と同じ wire format を出すことで CherryPick / レガシー Misskey との 1-on-1
// DM 連合互換性を確保する (#692)。
//
// 受け手側 (cherrypick の ApInboxService) は Note.`_misskey_talk` flag を見て
// chat_messages テーブルに保存する。chat 専用 type ではなく標準 Note を
// 使う設計なので、未対応の受信実装でも単なる note の Create として黙殺
// (致命的なエラーは起きない) のが利点。
func (r *Renderer) RenderChatMessage(msg *model.ChatMessage, senderURI, recipientURI string, published string) *Create {
	noteURI := r.urls.ChatMessageURI(msg.ID)
	note := &Note{
		Object: Object{
			ID:   noteURI,
			Type: "Note",
		},
		AttributedTo: APLenientID(senderURI),
		Published:    APLenientTimestamp(published),
		To:           []string{recipientURI},
		MisskeyTalk:  true,
	}
	r.setChatMessageContent(note, msg.Text)
	c := &Create{
		Activity: Activity{
			Object: Object{
				// Create activity の id は Note URI に "/activity" を付与した
				// 派生 URI とする (cherrypick も同様に renderActivity 系で
				// Create id を Note id から派生させる慣習)。
				ID:   noteURI + "/activity",
				Type: "Create",
			},
			Actor:     senderURI,
			Published: published,
			To:        []string{recipientURI},
		},
		Object: note,
	}
	AddContext(c)
	return c
}

// RenderChatRoomMessage renders a group chat (room) message as a Create
// wrapping a Note flagged with _misskey_talk, addressed to every room member.
// The note's `@context` is set to the room URI: CherryPick receivers extract
// the room id from it to route the message into the room. AddContext only sets
// the top-level Create context, so the note-level room `@context` survives.
func (r *Renderer) RenderChatRoomMessage(msg *model.ChatMessage, senderURI string, memberURIs []string, roomURI, published string) *Create {
	noteURI := r.urls.ChatMessageURI(msg.ID)
	note := &Note{
		Object: Object{
			ID:      noteURI,
			Type:    "Note",
			Context: roomURI,
		},
		AttributedTo: APLenientID(senderURI),
		Published:    APLenientTimestamp(published),
		To:           memberURIs,
		MisskeyTalk:  true,
	}
	r.setChatMessageContent(note, msg.Text)
	c := &Create{
		Activity: Activity{
			Object: Object{
				ID:   noteURI + "/activity",
				Type: "Create",
			},
			Actor:     senderURI,
			Published: published,
			To:        memberURIs,
		},
		Object: note,
	}
	AddContext(c)
	return c
}

// addressing computes to/cc lists for a note based on visibility.
func (r *Renderer) addressing(n *model.Note) (to []string, cc []string) {
	switch n.Visibility {
	case model.NoteVisibilityPublic:
		to = []string{Public}
		cc = []string{r.urls.UserFollowers(n.UserID)}
	case model.NoteVisibilityHome:
		to = []string{r.urls.UserFollowers(n.UserID)}
		cc = []string{Public}
	case model.NoteVisibilityFollowers:
		to = []string{r.urls.UserFollowers(n.UserID)}
	case model.NoteVisibilitySpecified:
		to = make([]string, 0, len(n.VisibleUserIDs))
		for _, uid := range n.VisibleUserIDs {
			// #2106 H5: visibleUser の AP URI を解決する。remote 受信者には remote
			// actor の実 URI (例 https://remote/users/xxx) を入れないと、受信側 Misskey が
			// resolvePerson に失敗して visibleUsers が空になり DM が silent-drop する
			// (自ドメイン /users/<remoteId> は送信元で 404、内部 ID 漏洩にもなる)。
			// mentionResolver は local→UserURI / remote→user.URI を返す (mention と同経路)。
			//
			// **解決できなかった宛先は落とす。** upstream の specified は
			// `to = mentionedRemoteUsers の uri` で、名前を引けないものは
			// そもそも並ばない (空の to も upstream では普通に起きる)。自ドメインの
			// `/users/<内部ID>` を代わりに置くと、(a) 自分の DB に居ない user を
			// 指す解決不能な URI になり、(b) 他の DM 宛先サーバへ内部 DB ID が
			// 出る。mention 側 (RenderNote の Mentions ループ) も resolve 失敗は
			// skip するので経路を揃える。
			//
			// resolver 未配線 (nil) のときだけ従来どおり local URI を置く。
			// local か remote かを判別する手段が無く、全宛先を落とすと specified
			// ノートの to が必ず空になるため。production は router.go が必ず
			// 配線する。
			if r.mentionResolver != nil {
				_, resolved, err := r.mentionResolver.ResolveMention(uid)
				if err != nil || resolved == "" {
					continue
				}
				to = append(to, resolved)
				continue
			}
			to = append(to, r.urls.UserURI(uid))
		}
	}
	return to, cc
}

func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func parseNoteTime(noteID string, idGen id.Generator) string {
	t, err := idGen.ParseTime(noteID)
	if err != nil {
		slog.Warn("renderer: failed to parse note ID for timestamp, using time.Now()", "noteId", noteID, "err", err)
		return time.Now().UTC().Format(publishedLayout)
	}
	return t.UTC().Format(publishedLayout)
}
