package i

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/model"
	"golang.org/x/net/html"
)

// httpsFieldValues extracts the https:// field values from a user profile's
// `fields` JSON ([{name,value}]). Used to pick the candidate links to verify
// (upstream filters `fields.filter(x => x.value.startsWith('https://'))`、#1786)。
func httpsFieldValues(profile *model.UserProfile) []string {
	if profile == nil || len(profile.Fields) == 0 {
		return nil
	}
	var fields []struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(profile.Fields, &fields); err != nil {
		return nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.HasPrefix(f.Value, "https://") {
			out = append(out, f.Value)
		}
	}
	return out
}

// maxVerifyLinkBytes caps how much of a remote page we parse for rel=me
// verification, guarding against unbounded responses.
const maxVerifyLinkBytes = 1 << 20 // 1 MiB

// verifyRelMe fetches pageURL and reports whether it links back to myLink,
// mirroring upstream i/update.ts verifyLink: a match is any `<a href=myLink>`
// (rel ignored) or any `<a>`/`<link>` whose rel attribute contains the token
// "me" and whose href equals myLink. The supplied client must be SSRF-safe
// (the caller passes the server's guarded outbound client); redirects to
// private addresses are rejected at dial time by that transport. Any
// error / non-200 yields false (#1786).
func verifyRelMe(client *http.Client, pageURL, myLink string) bool {
	if client == nil || pageURL == "" || myLink == "" {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	doc, err := html.Parse(io.LimitReader(resp.Body, maxVerifyLinkBytes))
	if err != nil {
		return false
	}
	return htmlLinksBackTo(doc, myLink)
}

// htmlLinksBackTo walks the parsed document for a back-link to myLink.
func htmlLinksBackTo(doc *html.Node, myLink string) bool {
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode && (n.Data == "a" || n.Data == "link") {
			var href, rel string
			for _, attr := range n.Attr {
				switch attr.Key {
				case "href":
					href = attr.Val
				case "rel":
					rel = attr.Val
				}
			}
			if href == myLink {
				// upstream includesMyLink: 任意の <a href=myLink> (rel 不問)。
				if n.Data == "a" {
					found = true
					return
				}
				// upstream includesRelMeLinks: <link> は rel に "me" を含むとき。
				for _, token := range strings.Fields(rel) {
					if token == "me" {
						found = true
						return
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// verifyLinks returns the subset of urls that link back to myLink, checked
// sequentially. Used to recompute user_profile.verifiedLinks after a profile
// update (#1786).
func verifyLinks(client *http.Client, myLink string, urls []string) []string {
	verified := make([]string, 0, len(urls))
	for _, u := range urls {
		if verifyRelMe(client, u, myLink) {
			verified = append(verified, u)
		}
	}
	return verified
}
