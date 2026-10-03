package server

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/activitypub/mfm"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/core/ugcvisibility"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// feedNoteLimit mirrors upstream FeedService (take: 20).
const feedNoteLimit = 20

// feedEntry is one note rendered into a feed-agnostic shape.
type feedEntry struct {
	Title    string
	Link     string
	Date     time.Time
	Summary  string // note.cw
	Content  string // MFM -> HTML
	ImageURL string
}

// feedData carries everything the three serialisations need.
type feedData struct {
	ID          string
	Title       string
	Description string
	Link        string
	Updated     time.Time
	ImageURL    string
	AuthorName  string
	AuthorLink  string
	Entries     []feedEntry
}

// --- RSS 2.0 ---

type rssRoot struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	LastBuild   string    `xml:"lastBuildDate,omitempty"`
	Generator   string    `xml:"generator"`
	Items       []rssItem `xml:"item"`
	Image       *rssImage `xml:"image,omitempty"`
}

type rssImage struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description,omitempty"`
}

func (f *feedData) rss2() ([]byte, error) {
	ch := rssChannel{
		Title:       f.Title,
		Link:        f.Link,
		Description: f.Description,
		Generator:   "Misskey",
	}
	if !f.Updated.IsZero() {
		ch.LastBuild = f.Updated.UTC().Format(time.RFC1123Z)
	}
	if f.ImageURL != "" {
		ch.Image = &rssImage{URL: f.ImageURL, Title: f.Title, Link: f.Link}
	}
	for _, e := range f.Entries {
		body := e.Content
		if e.Summary != "" {
			body = e.Summary
		}
		ch.Items = append(ch.Items, rssItem{
			Title:       e.Title,
			Link:        e.Link,
			GUID:        e.Link,
			PubDate:     e.Date.UTC().Format(time.RFC1123Z),
			Description: body,
		})
	}
	out, err := xml.Marshal(rssRoot{Version: "2.0", Channel: ch})
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

// --- Atom 1.0 ---

type atomFeed struct {
	XMLName  xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID       string      `xml:"id"`
	Title    string      `xml:"title"`
	Updated  string      `xml:"updated"`
	Subtitle string      `xml:"subtitle,omitempty"`
	Logo     string      `xml:"logo,omitempty"`
	Links    []atomLink  `xml:"link"`
	Author   *atomAuthor `xml:"author,omitempty"`
	Entries  []atomEntry `xml:"entry"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
	Type string `xml:"type,attr,omitempty"`
}

type atomAuthor struct {
	Name string `xml:"name"`
	URI  string `xml:"uri,omitempty"`
}

type atomEntry struct {
	ID      string       `xml:"id"`
	Title   string       `xml:"title"`
	Updated string       `xml:"updated"`
	Link    atomLink     `xml:"link"`
	Summary string       `xml:"summary,omitempty"`
	Content *atomContent `xml:"content,omitempty"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

func (f *feedData) atom1() ([]byte, error) {
	updated := f.Updated
	if updated.IsZero() {
		updated = time.Unix(0, 0).UTC()
	}
	af := atomFeed{
		ID:       f.ID,
		Title:    f.Title,
		Updated:  updated.UTC().Format(time.RFC3339),
		Subtitle: f.Description,
		Logo:     f.ImageURL,
		Links: []atomLink{
			{Href: f.Link, Rel: "alternate"},
			{Href: f.Link + ".atom", Rel: "self", Type: "application/atom+xml"},
		},
		Author: &atomAuthor{Name: f.AuthorName, URI: f.AuthorLink},
	}
	for _, e := range f.Entries {
		ae := atomEntry{
			ID:      e.Link,
			Title:   e.Title,
			Updated: e.Date.UTC().Format(time.RFC3339),
			Link:    atomLink{Href: e.Link, Rel: "alternate"},
			Summary: e.Summary,
		}
		if e.Content != "" {
			ae.Content = &atomContent{Type: "html", Body: e.Content}
		}
		af.Entries = append(af.Entries, ae)
	}
	out, err := xml.Marshal(af)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

// --- JSON Feed 1.1 ---

func (f *feedData) jsonFeed() ([]byte, error) {
	items := make([]map[string]any, 0, len(f.Entries))
	for _, e := range f.Entries {
		item := map[string]any{
			"id":             e.Link,
			"url":            e.Link,
			"title":          e.Title,
			"date_published": e.Date.UTC().Format(time.RFC3339),
		}
		if e.Content != "" {
			item["content_html"] = e.Content
		}
		if e.Summary != "" {
			item["summary"] = e.Summary
		}
		if e.ImageURL != "" {
			item["image"] = e.ImageURL
		}
		items = append(items, item)
	}
	doc := map[string]any{
		"version":       "https://jsonfeed.org/version/1.1",
		"title":         f.Title,
		"home_page_url": f.Link,
		"feed_url":      f.Link + ".json",
		"description":   f.Description,
		"authors":       []map[string]any{{"name": f.AuthorName, "url": f.AuthorLink}},
		"items":         items,
	}
	if f.ImageURL != "" {
		doc["icon"] = f.ImageURL
	}
	return json.Marshal(doc)
}

// --- handler ---

// FeedNoteLister returns the notes a user's feed should contain.
type FeedNoteLister interface {
	ListPublicNotesForFeed(userID string, limit int) ([]*model.Note, error)
}

// FeedUserResolver resolves a local user by its username.
type FeedUserResolver interface {
	FindLocalByUsername(username string) (*model.User, error)
}

// feedHandler serves /@:user.rss / .atom / .json.
type feedHandler struct {
	baseURL string
	host    string
	users   FeedUserResolver
	notes   FeedNoteLister
	// parseTime は note.id から投稿日時を得る。upstream も
	// idService.parse(note.id).date を使っており、note 行に createdAt は無い。
	parseTime func(id string) (time.Time, error)
	// ugcVisibility は meta.ugcVisibilityForVisitor の live lookup。nil なら
	// metaUGCVisibility の既定 ('local') として扱う。
	ugcVisibility func() string
	profiles      func(userID string) *model.UserProfile
	avatarURL     func(u *model.User) string
	// toHTML は note の本文と mentionedRemoteUsers 列から HTML を作る。本家
	// FeedService も toHtml に列を渡すので、リモートのメンションはその利用者の
	// url へのリンクになる。
	toHTML func(text, mentionedRemoteUsers string) string
	// now は時間窓の判定に使う現在時刻。nil なら time.Now (テストで固定する)。
	now func() time.Time
}

// serve builds the feed for the requested user and hands it to render.
//
// upstream は存在しないユーザーに 404 を返す。SPA catchall より前に登録して
// あるので、ここで 404 を返せばそのままクライアントに届く。
func (h *feedHandler) serve(c echo.Context, username string, render func(*feedData) ([]byte, error), contentType string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	u, err := h.users.FindLocalByUsername(username)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。このファイルは
		// apierr を使わないので echo の HTTPError で返す。
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	if err != nil || u == nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	// 凍結中 / 「ログインしていないユーザーにコンテンツを見せない」設定の
	// 利用者はフィードを配らない。upstream ClientServerService.getFeed は
	// `isSuspended: false, requireSigninToViewContents: false` を lookup の
	// 条件に入れており、外れると user が undefined になって 404 が返る
	// (ClientServerService.ts の getFeed)。mk-go は lookup 自体は
	// FindByUsernameLower を共有しているので、ここで同じ 2 条件を見る。
	//
	// **フィードは常に未認証で読める**ので viewer 判定は無い。他の 3 経路
	// (`ssr_meta.go` の NotePage / `internal/api/notehide` / `internal/stream/
	// channels/note_filter.go`) と同じく「requireSigninToViewContents な著者の
	// コンテンツは匿名に出さない」を、この経路でも成立させる。
	if u.IsSuspended || u.RequireSigninToViewContents {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	// upstream getFeed は ugcVisibilityForVisitor が 'none' なら feed を返さない
	// (404)。'local' で remote 利用者を外す条件は、この feed がローカル利用者
	// だけを引く (feedUserResolver) ので常に満たされない。
	if ugcvisibility.HidesAll(h.ugcVisibilityNow()) {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	data := h.build(u)
	out, err := render(data)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.Blob(http.StatusOK, contentType, out)
}

// HasUGCVisibility reports whether the meta.ugcVisibilityForVisitor lookup
// was wired. 未配線だと ugcVisibilityNow が既定の `local` に倒れ、`none` に
// しても feed が 404 にならないので、起動時検査で拾う。
func (h *feedHandler) HasUGCVisibility() bool { return h.ugcVisibility != nil }

// ugcVisibilityNow resolves the current meta.ugcVisibilityForVisitor.
func (h *feedHandler) ugcVisibilityNow() string {
	if h.ugcVisibility == nil {
		return defaultUGCVisibilityForVisitor
	}
	return h.ugcVisibility()
}

func (h *feedHandler) build(u *model.User) *feedData {
	name := u.Username
	if u.Name != nil && *u.Name != "" {
		name = *u.Name
	}
	authorLink := h.baseURL + "/@" + u.Username

	data := &feedData{
		ID:         authorLink,
		Title:      name + " (@" + u.Username + "@" + h.host + ")",
		Link:       authorLink,
		AuthorName: name,
		AuthorLink: authorLink,
	}
	if h.avatarURL != nil {
		data.ImageURL = h.avatarURL(u)
	}
	data.Description = h.describe(u)

	notes, err := h.notes.ListPublicNotesForFeed(u.ID, feedNoteLimit)
	if err != nil {
		notes = nil
	}
	nowMs := time.Now().UnixMilli()
	if h.now != nil {
		nowMs = h.now().UnixMilli()
	}
	for _, n := range notes {
		e := feedEntry{
			Title: "New note by " + name,
			Link:  h.baseURL + "/notes/" + n.ID,
		}
		// 作成時刻が分からないときは epoch 0 扱いにし、期間設定のゲートを
		// 「隠す」側に倒す (notehide.parseCreatedAtMs と同じ方針)。
		var createdAtMs int64
		if h.parseTime != nil {
			if t, err := h.parseTime(n.ID); err == nil {
				e.Date = t
				createdAtMs = t.UnixMilli()
			}
		}
		// upstream 2026.10.0 FeedService は 20 件取った後で
		// makeNotesHiddenBefore / makeNotesFollowersOnlyBefore に当たる note を
		// 落とす。フィードは常に匿名なので、followers へ降格した note も
		// 出してはいけない。
		//
		// LIMIT の後で落としても 1 ページは欠けない。どちらの設定も「ある時刻
		// より古い note」を隠すもので、一覧は id (= 作成時刻) の新しい順なので、
		// 隠れる note は必ず末尾に固まり、21 件目以降に見せてよい note は無い。
		if corenote.ShouldHideNoteByTime(u.MakeNotesHiddenBefore, createdAtMs, nowMs) ||
			corenote.ShouldHideNoteByTime(u.MakeNotesFollowersOnlyBefore, createdAtMs, nowMs) {
			continue
		}
		if n.CW != nil {
			e.Summary = *n.CW
		}
		if n.Text != nil && *n.Text != "" && h.toHTML != nil {
			e.Content = h.toHTML(*n.Text, n.MentionedRemoteUsers)
		}
		data.Entries = append(data.Entries, e)
	}
	if len(data.Entries) > 0 {
		data.Updated = data.Entries[0].Date
	}
	return data
}

// describe mirrors upstream の description 組み立て。
// followingVisibility / followersVisibility が public でなければ数を伏せる。
func (h *feedHandler) describe(u *model.User) string {
	followingVis, followersVis, bio := "public", "public", ""
	if h.profiles != nil {
		if p := h.profiles(u.ID); p != nil {
			followingVis, followersVis = string(p.FollowingVisibility), string(p.FollowersVisibility)
			if p.Description != nil {
				bio = *p.Description
			}
		}
	}
	hide := func(vis string, n int) string {
		if vis != "public" {
			return "?"
		}
		return strconv.Itoa(n)
	}
	desc := strconv.Itoa(u.NotesCount) + " Notes, " +
		hide(followingVis, u.FollowingCount) + " Following, " +
		hide(followersVis, u.FollowersCount) + " Followers"
	if bio != "" {
		desc += " · " + bio
	}
	return desc
}

func (h *feedHandler) RSS(c echo.Context, username string) error {
	return h.serve(c, username, (*feedData).rss2, "application/rss+xml; charset=utf-8")
}

func (h *feedHandler) Atom(c echo.Context, username string) error {
	return h.serve(c, username, (*feedData).atom1, "application/atom+xml; charset=utf-8")
}

func (h *feedHandler) JSON(c echo.Context, username string) error {
	return h.serve(c, username, (*feedData).jsonFeed, "application/json; charset=utf-8")
}

// TryServe dispatches /@<acct> to a feed when the acct carries a feed
// extension, reporting whether it handled the request.
//
// Echo のルータは Fastify と違い `/@:user.rss` のような「セグメント内に
// リテラルを含むパターン」を解釈できず、パラメータ名が `user.rss` になって
// しまう。そのため既存の /@:acct ハンドラで受けて、ここで拡張子を見て振り分ける。
func (h *feedHandler) TryServe(c echo.Context, acct string) (bool, error) {
	switch {
	case strings.HasSuffix(acct, ".rss"):
		return true, h.RSS(c, strings.TrimSuffix(acct, ".rss"))
	case strings.HasSuffix(acct, ".atom"):
		return true, h.Atom(c, strings.TrimSuffix(acct, ".atom"))
	case strings.HasSuffix(acct, ".json"):
		return true, h.JSON(c, strings.TrimSuffix(acct, ".json"))
	}
	return false, nil
}

// feedUserResolver adapts repository.UserRepository to FeedUserResolver.
//
// フィードはローカルユーザーのみ対象なので host=nil で引く。
type feedUserResolver struct {
	repo repository.UserRepository
}

func (r feedUserResolver) FindLocalByUsername(username string) (*model.User, error) {
	return r.repo.FindByUsernameLower(strings.ToLower(username), nil)
}

// feedNoteHTML returns the feedHandler.toHTML that renders a note's text with
// its mentionedRemoteUsers column, like upstream FeedService passes
// JSON.parse(note.mentionedRemoteUsers) to MfmService.toHtml.
func feedNoteHTML(host string) func(text, mentionedRemoteUsers string) string {
	return func(text, mentionedRemoteUsers string) string {
		return mfm.ToHTMLWithMentions(mfm.Parse(text), host, mfm.ParseMentionedRemoteUsers(mentionedRemoteUsers))
	}
}
