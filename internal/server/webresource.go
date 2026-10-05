package server

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// webResourceHandler serves the non-API web resources upstream exposes from
// ClientServerService (robots.txt / opensearch.xml).
//
// フロントエンドの SPA catchall より先に登録する必要がある。未登録のままだと
// catchall が index.html を 200 で返してしまい、「実装されているように見えて
// 中身が HTML」という分かりにくい壊れ方をする (#2345)。
type webResourceHandler struct {
	config   *config.Config
	metaRepo repository.MetaRepository
}

func newWebResourceHandler(cfg *config.Config, metaRepo repository.MetaRepository) *webResourceHandler {
	return &webResourceHandler{config: cfg, metaRepo: metaRepo}
}

// robotsDisallowedPaths mirrors upstream ClientServerService の robots.txt。
// 順序も upstream と揃えておく (差分比較でノイズにしないため)。
var robotsDisallowedPaths = []string{
	"/settings",
	"/admin",
	"/custom-emojis-manager",
	"/avatar-decorations",
	"/share",
	"/my",
	"/api",
	"/inbox",
	"/oauth",
	"/proxy",
	"/url",
}

// RobotsTxt serves /robots.txt.
//
// ugcVisibilityForVisitor が 'none' のインスタンスは未ログイン閲覧を塞いで
// いる意思表示なので、/@ と /notes もクロール対象から外す (upstream 同様)。
func (h *webResourceHandler) RobotsTxt(c echo.Context) error {
	disallowed := robotsDisallowedPaths
	if m, err := h.metaRepo.Fetch(); err == nil && m != nil && m.UgcVisibilityForVisitor == "none" {
		disallowed = append(append([]string{}, disallowed...), "/@", "/notes")
	}

	var b strings.Builder
	b.WriteString("User-agent: *\n")
	for i, p := range disallowed {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Disallow: " + p)
	}
	b.WriteString("\nAllow: /\n")
	b.WriteString("\n# todo: sitemap\n")

	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(b.String()))
}

// openSearchDescription is the OpenSearch 1.1 document served at
// /opensearch.xml.
type openSearchDescription struct {
	XMLName     xml.Name `xml:"OpenSearchDescription"`
	Xmlns       string   `xml:"xmlns,attr"`
	XmlnsMoz    string   `xml:"xmlns:moz,attr"`
	ShortName   string   `xml:"ShortName"`
	Description string   `xml:"Description"`
	InputEnc    string   `xml:"InputEncoding"`
	Image       struct {
		Width  int    `xml:"width,attr"`
		Height int    `xml:"height,attr"`
		Type   string `xml:"type,attr"`
		URL    string `xml:",chardata"`
	} `xml:"Image"`
	URL struct {
		Type     string `xml:"type,attr"`
		Template string `xml:"template,attr"`
	} `xml:"Url"`
}

// OpenSearchXML serves /opensearch.xml so browsers can register the instance
// as a search engine.
func (h *webResourceHandler) OpenSearchXML(c echo.Context) error {
	name := "Misskey"
	if m, err := h.metaRepo.Fetch(); err == nil && m != nil && m.Name != nil && *m.Name != "" {
		name = *m.Name
	}

	doc := openSearchDescription{
		Xmlns:       "http://a9.com/-/spec/opensearch/1.1/",
		XmlnsMoz:    "http://www.mozilla.org/2006/browser/search/",
		ShortName:   name,
		Description: name + " Search",
		InputEnc:    "UTF-8",
	}
	doc.Image.Width = 16
	doc.Image.Height = 16
	doc.Image.Type = "image/x-icon"
	doc.Image.URL = h.config.URL + "/favicon.ico"
	doc.URL.Type = "text/html"
	// {searchTerms} は OpenSearch のプレースホルダなので、そのまま出す。
	doc.URL.Template = h.config.URL + "/search?q={searchTerms}"

	out, err := xml.Marshal(doc)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.Blob(http.StatusOK, "application/opensearchdescription+xml", out)
}
