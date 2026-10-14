package ap

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
)

// apShowSoftwareBlocker is a stub of SoftwareSuspensionChecker.
type apShowSoftwareBlocker struct{ suspended map[string]bool }

func (b apShowSoftwareBlocker) IsSuspended(host string) bool { return b.suspended[host] }

// **受信を拒否するソフトウェアのホストは、ドメインブロックと同じく
// FEDERATION_NOT_ALLOWED にする (#3563)。** 止めないと resolver は取り込みを拒むが、
// 取得した生の AP JSON をそのまま返してしまう。
func TestAPIShow_InboxSuspendedSoftwareIsNotAllowed(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.SetFederationGate(apShowHostBlocker{}, "local.example")
	h.SetRemote(&mockFetcher{data: []byte(`{"type":"Note","id":"https://other.example/notes/1","content":"x"}`)}, &mockResolver{noteErr: assert.AnError})
	h.SetSoftwareSuspensionChecker(apShowSoftwareBlocker{suspended: map[string]bool{"susp.example": true}})
	assert.True(t, h.HasSoftwareSuspensionChecker())

	// ホストは小文字にしてから判定する (instance の行は小文字で持つ)。
	for _, uri := range []string{"https://susp.example/notes/1", "https://SUSP.example/notes/1"} {
		rec := postJSON(h.APIShow, `{"uri":"`+uri+`"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, uri)
		assert.Contains(t, rec.Body.String(), "FEDERATION_NOT_ALLOWED", uri)
	}

	// instance の行と同じ形 (punycode の小文字、既定でないポートは残す) にしてから
	// 判定する。IDN 表記・全角の句点・既定でないポートでも素通りしない。
	idn := idnhost.Puny("例え.example")
	require.NotEqual(t, "例え.example", idn)
	h.SetSoftwareSuspensionChecker(apShowSoftwareBlocker{suspended: map[string]bool{
		"susp.example": true, idn: true, "port.example:8443": true,
	}})
	for _, uri := range []string{
		"https://例え.example/notes/1",
		"https://例え。example/notes/1",
		"https://PORT.example:8443/notes/1",
		"https://susp.example:443/notes/1",
	} {
		rec := postJSON(h.APIShow, `{"uri":"`+uri+`"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, uri)
		assert.Contains(t, rec.Body.String(), "FEDERATION_NOT_ALLOWED", uri)
	}
	// 既定でないポートは別のホストとして扱う (instance の行も分かれる)。
	rec := postJSON(h.APIShow, `{"uri":"https://port.example/notes/1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	// 当たらないホストは従来どおり取りに行く。
	rec = postJSON(h.APIShow, `{"uri":"https://other.example/notes/1"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	// 自ホストは判定しない。
	h.SetSoftwareSuspensionChecker(apShowSoftwareBlocker{suspended: map[string]bool{"local.example": true}})
	rec = postJSON(h.APIShow, `{"uri":"https://local.example/notes/ghost"}`)
	assert.NotContains(t, rec.Body.String(), "FEDERATION_NOT_ALLOWED", "自ホストは判定しない")
}

// **リダイレクトで別のホストの文書が返ったときも、生の AP JSON を返す前に、文書の
// id のホストで判定する (#3563)。** 入口では要求した URI のホストしか見ないので、
// 拒否していないホストから拒否中のホストへリダイレクトさせると、resolver が取り込みを
// 拒んだ後の分岐で中身をそのまま返していた。ドメインブロックも同じ形で漏れていた。
func TestAPIShow_RawJSONIsGatedByDocumentIDHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       string
		software map[string]bool
		blocked  map[string]bool
		wantDeny bool
	}{
		{"software", "https://susp.example/notes/1", map[string]bool{"susp.example": true}, nil, true},
		{"software_port_spelling", "https://SUSP.example:443/notes/1", map[string]bool{"susp.example": true}, nil, true},
		{"domain_block", "https://blocked.example/notes/1", nil, map[string]bool{"blocked.example": true}, true},
		{"allowed", "https://other.example/notes/1", map[string]bool{"susp.example": true}, map[string]bool{"blocked.example": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newHandler(t)
			h.SetFederationGate(apShowHostBlocker{blocked: tc.blocked}, "local.example")
			h.SetSoftwareSuspensionChecker(apShowSoftwareBlocker{suspended: tc.software})
			h.SetRemote(&mockFetcher{data: []byte(`{"type":"Note","id":"` + tc.id + `","content":"secret"}`)}, &mockResolver{noteErr: assert.AnError})

			rec := postJSON(h.APIShow, `{"uri":"https://redirector.example/notes/1"}`)
			if tc.wantDeny {
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), "FEDERATION_NOT_ALLOWED")
				assert.NotContains(t, rec.Body.String(), "secret")
				return
			}
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), "secret")
		})
	}
}
