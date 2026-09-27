package server

import (
	"encoding/json"
	stdhtml "html"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/meta"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/frontendutil"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/notesummary"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// ssrMetaHandler serves the SPA shell with entity-specific <meta> tags for the
// public permalink routes (`/@user`, `/notes/:id`, ...).
//
// upstream は ClientServerService がこれらのパスを JSX で SSR し、OGP と
// `misskey:*` の meta を埋める。クローラや Discord / Slack のリンク展開は
// JavaScript を実行しないので、SPA shell をそのまま返すとどのページも
// インスタンス名しか読めない。
//
// 対象が見つからないときは meta 無しの素の shell を 200 で返す。upstream も
// 同じで、404 にするのは frontend の役目 (ページ内で「見つかりません」を出す)。
type ssrMetaHandler struct {
	cfg           *config.Config
	metaRepo      repository.MetaRepository
	proxyResolver meta.ProxyAccountResolver
	chunkedUpload meta.ChunkedUploadCapability
	clientEntry   frontendutil.ClientEntryInfo

	userRepo      repository.UserRepository
	noteRepo      repository.NoteRepository
	pageRepo      repository.PageRepository
	clipRepo      repository.ClipRepository
	flashRepo     repository.FlashRepository
	galleryRepo   repository.GalleryRepository
	driveFileRepo repository.DriveFileRepository
	channelRepo   repository.ChannelRepository
	reversiRepo   repository.ReversiRepository
	announceRepo  repository.AnnouncementRepository
	// idGen は drive file の pack (createdAt の復元) に要る。
	idGen id.Generator
}

// ssrMetaDeps carries the repositories the permalink pages read from.
// 引数で並べると 10 個を超えて取り違えても型が通ってしまうので、名前で渡す。
type ssrMetaDeps struct {
	User         repository.UserRepository
	Note         repository.NoteRepository
	Page         repository.PageRepository
	Clip         repository.ClipRepository
	Flash        repository.FlashRepository
	Gallery      repository.GalleryRepository
	Drive        repository.DriveFileRepository
	Channel      repository.ChannelRepository
	Reversi      repository.ReversiRepository
	Announcement repository.AnnouncementRepository
	// IDGen は drive file の pack (createdAt の復元) に要る。
	IDGen id.Generator
}

func newSSRMetaHandler(
	cfg *config.Config,
	metaRepo repository.MetaRepository,
	proxyResolver meta.ProxyAccountResolver,
	chunkedUpload meta.ChunkedUploadCapability,
	deps ssrMetaDeps,
) *ssrMetaHandler {
	return &ssrMetaHandler{
		cfg:           cfg,
		metaRepo:      metaRepo,
		proxyResolver: proxyResolver,
		chunkedUpload: chunkedUpload,
		clientEntry:   clientEntryFor(cfg),
		userRepo:      deps.User,
		noteRepo:      deps.Note,
		pageRepo:      deps.Page,
		clipRepo:      deps.Clip,
		flashRepo:     deps.Flash,
		galleryRepo:   deps.Gallery,
		driveFileRepo: deps.Drive,
		channelRepo:   deps.Channel,
		reversiRepo:   deps.Reversi,
		announceRepo:  deps.Announcement,
		idGen:         deps.IDGen,
	}
}

// render serves the shell with the given per-page overrides.
func (h *ssrMetaHandler) render(c echo.Context, ov shellOverrides) error {
	return renderFrontendShell(c, h.cfg, h.metaRepo, h.proxyResolver, h.chunkedUpload, h.clientEntry, ov)
}

// renderPlain serves the shell without any page-specific override. 対象が
// 見つからないときに使う (upstream も 404 にせず素の shell を返す)。
func (h *ssrMetaHandler) renderPlain(c echo.Context) error {
	return h.render(c, shellOverrides{})
}

// renderBareShell serves the shell for content we decided not to describe
// (未ログインには見せない設定、凍結、非公開 visibility など)。**リモート由来なら
// noindex は残す** (#3030)。
//
// `userHead` は「リモートのプロフィールを自インスタンスの URL で検索結果に
// 出さない」ために noindex を出すが、**中身を出さない分岐では `userHead` ごと
// 落ちていた**ので、その意図が効いていなかった。`ugcVisibilityForVisitor` の
// DB 既定は `local` なので、**既定の構成ではリモートの permalink が 1 つも
// noindex になっていなかった** (実測)。
//
// ローカル側は対象外 — 未ログインに見せない構成 (`none`) では upstream 同様
// robots.txt の `Disallow: /@` / `/notes` が受け持つ。
func (h *ssrMetaHandler) renderBareShell(c echo.Context, remote bool) error {
	return h.render(c, shellOverrides{NoIndex: remote})
}

// noteIsRemote reports whether the note came from another instance.
//
// **2 つのフィールドを見る (#3030)。** `note.User` は Preload 依存なので nil に
// なりうる。`note.userHost` は作成時に `user.host` を写した値で原則欠けないが、
// 片方だけを見る形にすると「中身を出さない分岐」と「完全に描く経路」で判定が
// 変わる余地が残るので、両方見て経路差を作らない。
func noteIsRemote(note *model.Note) bool {
	if note == nil {
		return false
	}
	return remoteHost(note.UserHost) || (note.User != nil && remoteHost(note.User.Host))
}

// metaTag builds a `<meta name=... content=...>` line. content は必ず escape する
// (username / note text は攻撃者が持ち込める)。
func metaTag(name, content string) string {
	return `<meta name="` + name + `" content="` + stdhtml.EscapeString(content) + `">` + "\n"
}

func propertyTag(property, content string) string {
	return `<meta property="` + property + `" content="` + stdhtml.EscapeString(content) + `">` + "\n"
}

func linkTag(rel, typ, href string) string {
	s := `<link rel="` + rel + `"`
	if typ != "" {
		s += ` type="` + typ + `"`
	}
	return s + ` href="` + stdhtml.EscapeString(href) + `">` + "\n"
}

// permalink ごとの Cache-Control。upstream ClientServerService が
// ハンドラ単位で出している値をそのまま持つ。
const (
	ssrPermalinkCacheControl = "public, max-age=15"
	ssrArchiveCacheControl   = "public, max-age=3600"
	ssrPrivateCacheControl   = "private, max-age=0, must-revalidate"
)

// remoteHost reports whether host identifies another instance.
// nil / 空文字はローカル (`user.host` / `note.userHost` の DB 表現)。
func remoteHost(host *string) bool {
	return host != nil && *host != ""
}

// robotsTagsFor returns the X-Robots-Tag values upstream emits for a profile.
// meta 版 (userHead) と両方出すのが upstream の挙動で、ヘッダー版は HTML を
// 解析しないクローラにも効く。
func robotsTagsFor(p *model.UserProfile) []string {
	if p == nil || !p.PreventAiLearning {
		return nil
	}
	return []string{"noimageai", "noai"}
}

// profileOf loads the profile row that carries the crawler preferences.
// 見つからなければ nil (upstream は profile 必須だが、mk-go では
// user だけ存在する経路があり得るので落とさない)。
func (h *ssrMetaHandler) profileOf(userID string) *model.UserProfile {
	if h.userRepo == nil || userID == "" {
		return nil
	}
	p, err := h.userRepo.FindProfileByUserID(userID)
	if err != nil {
		return nil
	}
	return p
}

// userHead builds the meta block shared by every page that belongs to a user.
// upstream の各 view (user.tsx / note.tsx / page.tsx ...) が共通で出すもの。
//
// forceNoindex はページ側の追加条件 (note.tsx の isRenote) を渡す。
func userHead(u *model.User, p *model.UserProfile, forceNoindex bool) string {
	if u == nil {
		// user が埋まっていなくても、呼び出し側が noindex を要求しているなら
		// それだけは出す (#3030)。`note.User` は Preload 依存なので、nil の
		// ときだけ remote の noindex が落ちる、という経路差を作らない。
		if forceNoindex {
			return metaTag("robots", "noindex")
		}
		return ""
	}
	var sb strings.Builder
	sb.WriteString(metaTag("misskey:user-username", u.Username))
	sb.WriteString(metaTag("misskey:user-id", u.ID))
	// remote user は自インスタンスの URL を正規とみなされないよう noindex。
	// upstream user.tsx の `props.user.host != null` 分岐と同じ。
	if forceNoindex || remoteHost(u.Host) || (p != nil && p.NoCrawle) {
		sb.WriteString(metaTag("robots", "noindex"))
	}
	// 学習利用の拒否。upstream と同じく noimageai と noai を並べる。
	// **モデルの既定は true** なので、明示的に許可した利用者以外は出る。
	if p != nil && p.PreventAiLearning {
		sb.WriteString(metaTag("robots", "noimageai"))
		sb.WriteString(metaTag("robots", "noai"))
	}
	return sb.String()
}

// visibleToVisitor reports whether an anonymous visitor may be shown metadata
// for content owned by a user on `host` (nil / "" = local).
//
// upstream ClientServerService は user / note の SSR で
// `ugcVisibilityForVisitor === 'all' || (=== 'local' && host == null)` を条件に
// している。permalink はログイン不要で誰でも叩けるので、ここを見ないと
// 「未ログインには見せない」設定にしてもリンク展開やクローラには本文の要約や
// プロフィール文が渡ってしまう。
//
// DB の既定値は 'local' なので、未設定・未知の値は 'local' として扱う。
func (h *ssrMetaHandler) visibleToVisitor(host *string) bool {
	m, err := h.metaRepo.Fetch()
	if err != nil || m == nil {
		return false
	}
	switch m.UgcVisibilityForVisitor {
	case "all":
		return true
	case "none":
		return false
	default:
		return host == nil || *host == ""
	}
}

// federationEnabled mirrors upstream の `props.federationEnabled`.
// 連合していないインスタンスで AP の URI を広告しても意味が無いので、
// rel="alternate" の出し分けに使う。
func (h *ssrMetaHandler) federationEnabled() bool {
	m, err := h.metaRepo.Fetch()
	if err != nil || m == nil {
		return false
	}
	return m.Federation != "none"
}

// userAlternateLinks emits the AP / canonical links upstream user.tsx puts in
// its metaSlot. sub パス (`/@alice/following` 等) では出さない。
func (h *ssrMetaHandler) userAlternateLinks(u *model.User, p *model.UserProfile, isSub bool) string {
	if u == nil || isSub || !h.federationEnabled() {
		return ""
	}
	var sb strings.Builder
	if u.Host == nil || *u.Host == "" {
		sb.WriteString(linkTag("alternate", "application/activity+json", h.cfg.URL+"/users/"+u.ID))
	}
	if u.URI != nil && *u.URI != "" {
		sb.WriteString(linkTag("alternate", "application/activity+json", *u.URI))
	}
	if p != nil && p.URL != nil && *p.URL != "" {
		sb.WriteString(linkTag("alternate", "text/html", *p.URL))
	}
	return sb.String()
}

// noteAlternateLinks emits the AP links upstream note.tsx puts in its metaSlot.
// ローカルノートは自分の AP URI を、リモートノートは originating server の URI を
// 指す (どちらも「この HTML の AP 版はこれ」を示す)。
func (h *ssrMetaHandler) noteAlternateLinks(note *model.Note) string {
	if note == nil || !h.federationEnabled() {
		return ""
	}
	var sb strings.Builder
	if note.UserHost == nil || *note.UserHost == "" {
		sb.WriteString(linkTag("alternate", "application/activity+json", h.cfg.URL+"/notes/"+note.ID))
	}
	if note.URI != nil && *note.URI != "" {
		sb.WriteString(linkTag("alternate", "application/activity+json", *note.URI))
	}
	return sb.String()
}

// meLinks emits `<link rel="me">` for the profile fields that hold a URL.
// upstream user.tsx と同じく http(s) で始まる値だけを対象にする
// (rel=me は所有証明に使われるので、任意文字列を混ぜない)。
func meLinks(p *model.UserProfile) string {
	if p == nil || len(p.Fields) == 0 {
		return ""
	}
	var fields []struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(p.Fields, &fields); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, f := range fields {
		if strings.HasPrefix(f.Value, "http://") || strings.HasPrefix(f.Value, "https://") {
			sb.WriteString(linkTag("me", "", f.Value))
		}
	}
	return sb.String()
}

// instanceName returns the name used in the `<title>` suffix.
func (h *ssrMetaHandler) instanceName() string {
	m, err := h.metaRepo.Fetch()
	if err == nil && m != nil && m.Name != nil && *m.Name != "" {
		return *m.Name
	}
	return "Misskey"
}

// pageTitle mirrors upstream の `title={`${x} | ${instanceName}`}`.
func (h *ssrMetaHandler) pageTitle(head string) string {
	return head + " | " + h.instanceName()
}

// absoluteURL makes a site-relative URL absolute.
//
// OGP を読むのは別 origin のクローラなので、相対 URL では解決できない
// (identicon fallback や local drive の URL が相対になる、#2527 と同じ罠)。
func (h *ssrMetaHandler) absoluteURL(u string) string {
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return strings.TrimSuffix(h.cfg.URL, "/") + "/" + strings.TrimPrefix(u, "/")
}

// avatarURL resolves the OGP image for a user. entity.IdenticonURL が
// proxy 化と identicon fallback をまとめて面倒を見る (#1529 / #710)。
func (h *ssrMetaHandler) avatarURL(u *model.User) string {
	if u == nil {
		return ""
	}
	return h.absoluteURL(entity.IdenticonURL(u))
}

// filesOf packs the drive files attached to a note / gallery post.
// 見つからない ID は黙って落とす (削除済みファイルで OGP 全体を落とさない)。
func (h *ssrMetaHandler) filesOf(ids []string) []entity.DriveFileEntity {
	if h.driveFileRepo == nil || len(ids) == 0 {
		return nil
	}
	rows, err := h.driveFileRepo.FindByIDs(ids)
	if err != nil {
		return nil
	}
	byID := make(map[string]*model.DriveFile, len(rows))
	for _, f := range rows {
		byID[f.ID] = f
	}
	// fileIds の順序が添付順なので、その順で並べ直す (og:image の 1 枚目が
	// リンク展開のサムネイルになる)。
	out := make([]entity.DriveFileEntity, 0, len(ids))
	for _, fid := range ids {
		if f, ok := byID[fid]; ok {
			out = append(out, entity.PackDriveFile(f, h.idGen))
		}
	}
	return out
}

// noteSummary reproduces upstream の getNoteSummary(packed note)。
//
// reply / renote は upstream では pack 済み (viewer=null) なので、公開でない
// ものは `isHidden` として `(⛔)` に落ちる。ここでも同じ扱いにして、本文が
// permalink 経由で漏れないようにする ([[note_embed_depth_visibility_gate_invariant]])。
func (h *ssrMetaHandler) noteSummary(note *model.Note) string {
	return notesummary.Get(h.summaryMap(note, true))
}

func (h *ssrMetaHandler) summaryMap(note *model.Note, hydrate bool) map[string]any {
	if note == nil {
		return nil
	}
	m := map[string]any{}
	if note.Visibility != model.NoteVisibilityPublic {
		// viewer なしでは読めない note。upstream の packed note と同じく
		// isHidden として扱い、本文を出さない。
		m["isHidden"] = true
		return m
	}
	if note.CW != nil {
		m["cw"] = *note.CW
	}
	if note.Text != nil {
		m["text"] = *note.Text
	}
	if n := len(note.FileIDs); n > 0 {
		files := make([]any, n)
		m["files"] = files
	}
	if note.HasPoll {
		m["poll"] = map[string]any{}
	}
	if note.ReplyID != nil {
		m["replyId"] = *note.ReplyID
		if hydrate {
			if reply := h.noteByID(*note.ReplyID); reply != nil {
				m["reply"] = h.summaryMap(reply, false)
			}
		}
	}
	if note.RenoteID != nil {
		m["renoteId"] = *note.RenoteID
		if hydrate {
			if renote := h.noteByID(*note.RenoteID); renote != nil {
				m["renote"] = h.summaryMap(renote, false)
			}
		}
	}
	return m
}

func (h *ssrMetaHandler) noteByID(id string) *model.Note {
	if h.noteRepo == nil || id == "" {
		return nil
	}
	n, err := h.noteRepo.FindByID(id)
	if err != nil {
		return nil
	}
	return n
}

// noteMediaOG builds the media half of upstream note.tsx's ogBlock: video meta
// for every attached video, then either the images (summary_large_image) or the
// author's avatar (summary).
//
// sensitive なファイルを除外しないのは upstream と同じ (note.tsx は
// isSensitive を見ない。gallery-post.tsx だけが見る)。
func noteMediaOG(files []entity.DriveFileEntity, avatarURL string) string {
	var sb strings.Builder
	var images []entity.DriveFileEntity
	for _, f := range files {
		switch {
		case strings.HasPrefix(f.Type, "video/"):
			sb.WriteString(propertyTag("og:video:url", f.URL))
			sb.WriteString(propertyTag("og:video:secure_url", f.URL))
			sb.WriteString(propertyTag("og:video:type", f.Type))
			if f.ThumbnailURL != nil && *f.ThumbnailURL != "" {
				sb.WriteString(propertyTag("og:video:image", *f.ThumbnailURL))
			}
			if f.Properties.Width != nil {
				sb.WriteString(propertyTag("og:video:width", strconv.Itoa(*f.Properties.Width)))
			}
			if f.Properties.Height != nil {
				sb.WriteString(propertyTag("og:video:height", strconv.Itoa(*f.Properties.Height)))
			}
		case strings.HasPrefix(f.Type, "image/"):
			images = append(images, f)
		}
	}
	if len(images) > 0 {
		sb.WriteString(metaTag("twitter:card", "summary_large_image"))
		for _, f := range images {
			sb.WriteString(propertyTag("og:image", f.URL))
			if f.Properties.Width != nil {
				sb.WriteString(propertyTag("og:image:width", strconv.Itoa(*f.Properties.Width)))
			}
			if f.Properties.Height != nil {
				sb.WriteString(propertyTag("og:image:height", strconv.Itoa(*f.Properties.Height)))
			}
		}
		return sb.String()
	}
	sb.WriteString(metaTag("twitter:card", "summary"))
	if avatarURL != "" {
		sb.WriteString(propertyTag("og:image", avatarURL))
	}
	return sb.String()
}

// authorImageOG is the avatar-backed og:image + twitter:card pair that upstream
// clip / flash / page / gallery emit when they have no better image.
// avatarUrl が無いときは card ごと出さない (upstream の三項分岐と同じ)。
func authorImageOG(avatarURL string) string {
	if avatarURL == "" {
		return ""
	}
	return propertyTag("og:image", avatarURL) + metaTag("twitter:card", "summary")
}

// largeImageOG is the summary_large_image variant used when the page has a
// representative image of its own (gallery の 1 枚目、page の eyeCatchingImage)。
func largeImageOG(f *entity.DriveFileEntity) string {
	if f == nil {
		return ""
	}
	url := f.URL
	if f.ThumbnailURL != nil && *f.ThumbnailURL != "" {
		url = *f.ThumbnailURL
	}
	if url == "" {
		return ""
	}
	return propertyTag("og:image", url) + metaTag("twitter:card", "summary_large_image")
}

// strOrEmpty dereferences an optional string into the non-nil form upstream
// passes as `desc` (`?? ”`). description タグ自体は必ず出る。
func strOrEmpty(p *string) *string {
	if p == nil {
		empty := ""
		return &empty
	}
	v := *p
	return &v
}

// fileByID packs a single drive file (page の eyeCatchingImage 用)。
func (h *ssrMetaHandler) fileByID(id *string) *entity.DriveFileEntity {
	if id == nil || *id == "" {
		return nil
	}
	files := h.filesOf([]string{*id})
	if len(files) == 0 {
		return nil
	}
	return &files[0]
}

// displayName returns the OGP title fragment for a user (`Name (@handle)`).
func displayName(u *model.User) string {
	handle := "@" + u.Username
	if u.Host != nil && *u.Host != "" {
		handle += "@" + *u.Host
	}
	if u.Name != nil && *u.Name != "" {
		return *u.Name + " (" + handle + ")"
	}
	return handle
}

// lookupUserByAcct resolves `alice` / `alice@remote.example` to a user row.
func (h *ssrMetaHandler) lookupUserByAcct(acct string) *model.User {
	if h.userRepo == nil || acct == "" {
		return nil
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(acct, "@"), "@")
	if name == "" {
		return nil
	}
	u, err := h.userRepo.FindByUsernameLower(strings.ToLower(name), h.acctHost(acct))
	if err != nil {
		return nil
	}
	return u
}

// acctHost returns the host part of an acct, or nil when it names a local user.
// 自ホスト宛ての acct は local 扱いにする (upstream の Acct.parse と同じ)。
//
// **DB に行が無くても判定できるのが要点 (#3030)。** acct 文字列だけで
// 「よそのインスタンスの誰か」と分かるので、まだ解決していないリモート
// ユーザーでも noindex を落とさずに済む。`ugcVisibilityForVisitor` が `all` の
// 構成では、SPA が初回アクセスでリモートを解決してプロフィールを描くため、
// ここを素通しにすると**初回のクロールだけ index される**窓ができる。
func (h *ssrMetaHandler) acctHost(acct string) *string {
	_, host, _ := strings.Cut(strings.TrimPrefix(acct, "@"), "@")
	if host == "" || strings.EqualFold(host, h.cfg.Host) {
		return nil
	}
	return &host
}

// UserPage serves `/@:acct` and its sub paths (`/@:acct/notes` 等)。
func (h *ssrMetaHandler) UserPage(c echo.Context) error {
	u := h.lookupUserByAcct(c.Param("acct"))
	if u == nil {
		return h.renderBareShell(c, remoteHost(h.acctHost(c.Param("acct"))))
	}
	// 凍結ユーザーは upstream の検索条件 (isSuspended: false) で対象外。
	// 未ログイン訪問者への UGC 露出も meta の生成前に判定する (#2533)。
	if u.IsSuspended || !h.visibleToVisitor(u.Host) {
		return h.renderBareShell(c, remoteHost(u.Host))
	}
	p := h.profileOf(u.ID)
	og := propertyTag("og:type", "blog") +
		propertyTag("og:title", displayName(u))
	// upstream は description が null ならタグごと出さない (空文字は出す)。
	if p != nil && p.Description != nil {
		og += propertyTag("og:description", *p.Description)
	}
	og += propertyTag("og:url", h.cfg.URL+"/@"+u.Username)
	og += authorImageOG(h.avatarURL(u))
	head := userHead(u, p, false) +
		h.userAlternateLinks(u, p, c.Param("sub") != "") +
		meLinks(p)
	// upstream の Layout title は og:title と違って host を付けない。
	name := u.Username
	if u.Name != nil && *u.Name != "" {
		name = *u.Name
	}
	var desc *string
	if p != nil {
		desc = strOrEmpty(p.Description)
	} else {
		desc = strOrEmpty(nil)
	}
	return h.render(c, shellOverrides{
		Head:         head,
		OG:           og,
		Title:        h.pageTitle(name + " (@" + u.Username + ")"),
		Description:  desc,
		CacheControl: ssrPermalinkCacheControl,
		RobotsTag:    robotsTagsFor(p),
	})
}

// UserPagePage serves `/@:acct/pages/:page`.
func (h *ssrMetaHandler) UserPagePage(c echo.Context) error {
	u := h.lookupUserByAcct(c.Param("acct"))
	if u == nil {
		return h.renderBareShell(c, remoteHost(h.acctHost(c.Param("acct"))))
	}
	if u.IsSuspended || !h.visibleToVisitor(u.Host) || h.pageRepo == nil {
		return h.renderBareShell(c, remoteHost(u.Host))
	}
	p := h.profileOf(u.ID)
	page, err := h.pageRepo.FindByUserAndName(u.ID, c.Param("page"))
	if err != nil || page == nil {
		return h.render(c, shellOverrides{Head: userHead(u, p, false)})
	}
	og := propertyTag("og:type", "article") +
		propertyTag("og:title", page.Title)
	if page.Summary != nil {
		og += propertyTag("og:description", *page.Summary)
	}
	// upstream page.tsx は permalink を /pages/<id> で広告する (SPA が解決する)。
	og += propertyTag("og:url", h.cfg.URL+"/pages/"+page.ID)
	if img := largeImageOG(h.fileByID(page.EyeCatchingImageID)); img != "" {
		og += img
	} else {
		og += authorImageOG(h.avatarURL(u))
	}
	// upstream は公開ページだけ共有キャッシュに載せ、それ以外は revalidate を
	// 強制する (limited / specified なページが CDN に残らないようにする)。
	cache := ssrPermalinkCacheControl
	if page.Visibility != model.PageVisibilityPublic {
		cache = ssrPrivateCacheControl
	}
	return h.render(c, shellOverrides{
		Head:         userHead(u, p, false) + metaTag("misskey:page-id", page.ID),
		OG:           og,
		Title:        h.pageTitle(page.Title),
		Description:  strOrEmpty(page.Summary),
		CacheControl: cache,
		RobotsTag:    robotsTagsFor(p),
	})
}

// NotePage serves `/notes/:id` for HTML clients. AP クライアント (Accept:
// application/activity+json) は先に AP handler が処理するので、ここには
// 来ない。
func (h *ssrMetaHandler) NotePage(c echo.Context) error {
	if h.noteRepo == nil {
		return h.renderPlain(c)
	}
	note, err := h.noteRepo.FindByIDWithUser(c.Param("id"))
	if err != nil || note == nil {
		return h.renderPlain(c)
	}
	// visibility が public 以外の note は meta を出さない。クローラや
	// リンク展開に非公開投稿の本文・著者を渡さないため (upstream も
	// public 以外は SSR しない)。
	if note.Visibility != model.NoteVisibilityPublic {
		return h.renderBareShell(c, noteIsRemote(note))
	}
	// 投稿者が「ログインしないと見せない」設定なら meta も出さない。
	// upstream note ハンドラの `!note.user.requireSigninToViewContents` (#2533)。
	if note.User != nil && note.User.RequireSigninToViewContents {
		return h.renderBareShell(c, noteIsRemote(note))
	}
	if !h.visibleToVisitor(note.UserHost) {
		return h.renderBareShell(c, noteIsRemote(note))
	}
	title := ""
	if note.User != nil {
		title = displayName(note.User)
	}
	summary := h.noteSummary(note)
	og := propertyTag("og:type", "article")
	if title != "" {
		og += propertyTag("og:title", title)
	}
	// upstream は本文そのままではなく getNoteSummary を使う。添付数・投票・
	// 返信元が要約に含まれるので、リンク展開だけ見ても中身が分かる。
	og += propertyTag("og:description", summary)
	og += propertyTag("og:url", h.cfg.URL+"/notes/"+note.ID)
	og += noteMediaOG(h.filesOf(note.FileIDs), h.avatarURL(note.User))
	var profile *model.UserProfile
	if note.User != nil {
		profile = h.profileOf(note.User.ID)
	}
	// upstream note.tsx は isRenotePacked (= renoteId != null) で noindex。
	// 引用リノートも対象で、他人の投稿を写した URL を検索対象にしない。
	// リモート判定は bail-out 側と同じ `noteIsRemote` に揃える (#3030)。
	// `userHead` が見る `note.User.Host` だけだと、`User` が nil のときに
	// remote の noindex が落ちる。
	head := userHead(note.User, profile, note.RenoteID != nil || noteIsRemote(note)) +
		metaTag("misskey:note-id", note.ID) +
		h.noteAlternateLinks(note)
	return h.render(c, shellOverrides{
		Head:         head,
		OG:           og,
		Title:        h.pageTitle(title),
		Description:  &summary,
		CacheControl: ssrPermalinkCacheControl,
		RobotsTag:    robotsTagsFor(profile),
	})
}

// ClipPage serves `/clips/:clip`.
func (h *ssrMetaHandler) ClipPage(c echo.Context) error {
	if h.clipRepo == nil {
		return h.renderPlain(c)
	}
	clip, err := h.clipRepo.FindByID(c.Param("clip"))
	if err != nil || clip == nil || !clip.IsPublic {
		return h.renderPlain(c)
	}
	author := h.userByID(clip.UserID)
	og := propertyTag("og:type", "article") +
		propertyTag("og:title", clip.Name)
	if clip.Description != nil {
		og += propertyTag("og:description", *clip.Description)
	}
	og += propertyTag("og:url", h.cfg.URL+"/clips/"+clip.ID)
	og += authorImageOG(h.avatarURL(author))
	profile := h.profileOf(clip.UserID)
	return h.render(c, shellOverrides{
		Head:         userHead(author, profile, false) + metaTag("misskey:clip-id", clip.ID),
		OG:           og,
		Title:        h.pageTitle(clip.Name),
		Description:  strOrEmpty(clip.Description),
		CacheControl: ssrPermalinkCacheControl,
		RobotsTag:    robotsTagsFor(profile),
	})
}

// FlashPage serves `/play/:id`.
func (h *ssrMetaHandler) FlashPage(c echo.Context) error {
	if h.flashRepo == nil {
		return h.renderPlain(c)
	}
	flash, err := h.flashRepo.FindByID(c.Param("id"))
	if err != nil || flash == nil {
		return h.renderPlain(c)
	}
	// 非公開の Flash は API (flash/show) では所有者以外に存在しない扱いにして
	// いるので、ここでも title / summary / 作者を出さない。SSR はセッションを
	// 見ない (Cookie 認証を持たない) ので所有者かどうかを判定できず、非公開は
	// 一律に素の shell を返す。upstream はここで可視性を見ない (divergence.md)。
	if flash.Visibility != "" && flash.Visibility != "public" {
		return h.renderPlain(c)
	}
	author := h.userByID(flash.UserID)
	og := propertyTag("og:type", "article") +
		propertyTag("og:title", flash.Title) +
		propertyTag("og:description", flash.Summary) +
		propertyTag("og:url", h.cfg.URL+"/play/"+flash.ID) +
		authorImageOG(h.avatarURL(author))
	profile := h.profileOf(flash.UserID)
	return h.render(c, shellOverrides{
		Head:         userHead(author, profile, false) + metaTag("misskey:flash-id", flash.ID),
		OG:           og,
		Title:        h.pageTitle(flash.Title),
		Description:  &flash.Summary,
		CacheControl: ssrPermalinkCacheControl,
		RobotsTag:    robotsTagsFor(profile),
	})
}

// GalleryPage serves `/gallery/:post`.
func (h *ssrMetaHandler) GalleryPage(c echo.Context) error {
	if h.galleryRepo == nil {
		return h.renderPlain(c)
	}
	posts, err := h.galleryRepo.FindPostsByIDs([]string{c.Param("post")})
	if err != nil || len(posts) == 0 {
		return h.renderPlain(c)
	}
	post := posts[0]
	author := post.User
	if author == nil {
		author = h.userByID(post.UserID)
	}
	og := propertyTag("og:type", "article") +
		propertyTag("og:title", post.Title)
	if post.Description != nil {
		og += propertyTag("og:description", *post.Description)
	}
	og += propertyTag("og:url", h.cfg.URL+"/gallery/"+post.ID)
	// sensitive な投稿は作品そのものを展開先に出さず、著者の avatar に落とす
	// (upstream gallery-post.tsx の分岐)。
	if post.IsSensitive {
		og += authorImageOG(h.avatarURL(author))
	} else {
		files := h.filesOf(post.FileIDs)
		if len(files) > 0 {
			og += largeImageOG(&files[0])
		}
	}
	profile := h.profileOf(post.UserID)
	head := userHead(author, profile, false) +
		metaTag("misskey:gallery-post-id", post.ID)
	return h.render(c, shellOverrides{
		Head:         head,
		OG:           og,
		Title:        h.pageTitle(post.Title),
		Description:  strOrEmpty(post.Description),
		CacheControl: ssrPermalinkCacheControl,
		RobotsTag:    robotsTagsFor(profile),
	})
}

// NoIndexPage serves the shell with `noindex`, for pages that have no business
// being in search results. 中身は SPA が描くので shell のままでよく、出したい
// のは `<meta name="robots" content="noindex">` だけ。
//
// 現在の利用者:
//   - `/tags/:tag` / `/user-tags/:tag` — upstream も `{ noindex: true }` で描く。
//     タグの組み合わせは無限に増える
//   - `/instance-info/:host` — **upstream は SSR していない** (#3030)。自鯖の
//     情報ではないものを検索結果に出す意味が無く、連合先の数だけ URL が増える
func (h *ssrMetaHandler) NoIndexPage(c echo.Context) error {
	return h.render(c, shellOverrides{NoIndex: true})
}

// ChannelPage serves `/channels/:channel` (upstream views/channel.tsx)。
func (h *ssrMetaHandler) ChannelPage(c echo.Context) error {
	if h.channelRepo == nil {
		return h.renderPlain(c)
	}
	ch, err := h.channelRepo.FindByID(c.Param("channel"))
	if err != nil || ch == nil {
		return h.renderPlain(c)
	}
	og := propertyTag("og:type", "website") +
		propertyTag("og:title", ch.Name)
	if ch.Description != nil {
		og += propertyTag("og:description", *ch.Description)
	}
	og += propertyTag("og:url", h.cfg.URL+"/channels/"+ch.ID)
	// banner はチャンネルの顔なので、あればそれを展開先に出す。
	if banner := h.fileByID(ch.BannerID); banner != nil && banner.URL != "" {
		og += propertyTag("og:image", banner.URL) + metaTag("twitter:card", "summary")
	}
	return h.render(c, shellOverrides{
		OG:           og,
		Title:        h.pageTitle(ch.Name),
		Description:  strOrEmpty(ch.Description),
		CacheControl: ssrPermalinkCacheControl,
	})
}

// ReversiGamePage serves `/reversi/g/:game` (upstream views/reversi-game.tsx)。
func (h *ssrMetaHandler) ReversiGamePage(c echo.Context) error {
	if h.reversiRepo == nil {
		return h.renderPlain(c)
	}
	game, err := h.reversiRepo.FindByID(c.Param("game"))
	if err != nil || game == nil {
		return h.renderPlain(c)
	}
	title := h.reversiTitle(game)
	// upstream は固定文言。対局内容は出さない。
	const description = "⚫⚪Misskey Reversi⚪⚫"
	og := propertyTag("og:type", "article") +
		propertyTag("og:title", title) +
		propertyTag("og:description", description) +
		propertyTag("og:url", h.cfg.URL+"/reversi/g/"+game.ID) +
		metaTag("twitter:card", "summary")
	desc := description
	return h.render(c, shellOverrides{
		OG:           og,
		Title:        h.pageTitle(title),
		Description:  &desc,
		CacheControl: ssrArchiveCacheControl,
	})
}

// reversiTitle mirrors upstream の `${user1.username} vs ${user2.username}`.
// 対局者が引けなくても落とさず、空の側は空文字のままにする。
func (h *ssrMetaHandler) reversiTitle(game *model.ReversiGame) string {
	name := func(embedded *model.User, id string) string {
		if embedded != nil {
			return embedded.Username
		}
		if u := h.userByID(id); u != nil {
			return u.Username
		}
		return ""
	}
	return name(game.User1, game.User1ID) + " vs " + name(game.User2, game.User2ID)
}

// AnnouncementPage serves `/announcements/:id` (upstream views/announcement.tsx)。
func (h *ssrMetaHandler) AnnouncementPage(c echo.Context) error {
	if h.announceRepo == nil {
		return h.renderPlain(c)
	}
	a, err := h.announceRepo.FindByID(c.Param("id"))
	if err != nil || a == nil {
		return h.renderPlain(c)
	}
	// 個人宛てのお知らせ (userId が入っているもの) は permalink で配らない。
	// upstream も `userId: IsNull()` で絞っている。ここが緩むと、URL を
	// 知っているだけで他人宛ての通知内容が読める。
	if a.UserID != nil && *a.UserID != "" {
		return h.renderPlain(c)
	}
	description := announcementSummary(a.Text)
	og := propertyTag("og:type", "article") +
		propertyTag("og:title", a.Title) +
		propertyTag("og:description", description) +
		propertyTag("og:url", h.cfg.URL+"/announcements/"+a.ID)
	if a.ImageURL != nil && *a.ImageURL != "" {
		og += propertyTag("og:image", h.absoluteURL(*a.ImageURL)) +
			metaTag("twitter:card", "summary_large_image")
	}
	return h.render(c, shellOverrides{
		OG:           og,
		Title:        h.pageTitle(a.Title),
		Description:  &description,
		CacheControl: ssrArchiveCacheControl,
	})
}

// announcementSummary truncates the body like upstream announcement.tsx
// (100 文字を超えたら "…" を付ける)。**rune 単位で数える**: byte で切ると
// 日本語の途中で切れて壊れた UTF-8 が meta に載る。
func announcementSummary(text string) string {
	const limit = 100
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func (h *ssrMetaHandler) userByID(id string) *model.User {
	if h.userRepo == nil || id == "" {
		return nil
	}
	u, err := h.userRepo.FindByID(id)
	if err != nil {
		return nil
	}
	return u
}

// prefersHTML reports whether the client asked for HTML rather than
// ActivityPub JSON. `/@:acct` と `/notes/:id` は AP actor / object の URL でも
// あるので、Accept を見て振り分ける。
func prefersHTML(c echo.Context) bool {
	accept := c.Request().Header.Get(echo.HeaderAccept)
	if accept == "" {
		return true
	}
	return !strings.Contains(accept, "application/activity+json") &&
		!strings.Contains(accept, "application/ld+json")
}
