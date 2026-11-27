package i

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/htmlstream"
	"github.com/elythia-network/elythia/internal/model"
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
	return htmlLinksBackTo(io.LimitReader(resp.Body, maxVerifyLinkBytes), myLink)
}

// htmlLinksBackTo reads the page for a back-link to myLink. Like before, a page
// that x/net/html's Parse could not parse (too deep, or a read error) has none.
func htmlLinksBackTo(r io.Reader, myLink string) bool {
	found := false
	sc := htmlstream.NewScanner(r)
	for {
		k := sc.Next()
		if k == htmlstream.EOF {
			return found && sc.Err() == nil
		}
		if found || k != htmlstream.StartElement {
			continue
		}
		n := sc.Element()
		if n.Name != "a" && n.Name != "link" {
			continue
		}
		var href, rel string
		for _, attr := range n.Attr {
			switch attr.Key {
			case "href":
				href = attr.Val
			case "rel":
				rel = attr.Val
			}
		}
		if href != myLink {
			continue
		}
		// upstream includesMyLink: 任意の <a href=myLink> (rel 不問)。
		if n.Name == "a" {
			found = true
			continue
		}
		// upstream includesRelMeLinks: <link> は rel に "me" を含むとき。
		for _, token := range strings.Fields(rel) {
			if token == "me" {
				found = true
			}
		}
	}
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
