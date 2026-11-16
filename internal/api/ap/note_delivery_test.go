package ap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/plugin"
)

func apDeliveryRules(t *testing.T, targets map[string]plugin.DeliveryTargets) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return targets, nil },
	}))
	return r
}

// A restricted note is served by the requester's host (#4199).
// bob signs from remote.example. An unsigned request without a known
// User-Agent is an unknown host: include hides the note, exclude serves it.
func TestNoteDeliveryRules_NoteAndActivity(t *testing.T) {
	tests := []struct {
		name         string
		targets      plugin.DeliveryTargets
		wantSigned   int
		wantUnsigned int
	}{
		{name: "include signer host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"remote.example"}}, wantSigned: http.StatusOK, wantUnsigned: http.StatusNotFound},
		{name: "include other host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"other.example"}}, wantSigned: http.StatusNotFound, wantUnsigned: http.StatusNotFound},
		{name: "exclude signer host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"remote.example"}}, wantSigned: http.StatusNotFound, wantUnsigned: http.StatusOK},
		{name: "exclude other host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"other.example"}}, wantSigned: http.StatusOK, wantUnsigned: http.StatusOK},
	}
	for _, tt := range tests {
		for name := range signedFetchEndpoints(newSignedFetchFixture(t).h) {
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				f := newSignedFetchFixture(t)
				f.h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": tt.targets}))
				handler := signedFetchEndpoints(f.h)[name]
				assert.Equal(t, tt.wantSigned, f.fetch(t, handler, "n-public", true), "signed")
				assert.Equal(t, tt.wantUnsigned, f.fetch(t, handler, "n-public", false), "unsigned")
			})
		}
	}
}

// fetchWithUA runs an unsigned GET of n-public with a User-Agent.
func (f *signedFetchFixture) fetchWithUA(t *testing.T, userAgent string, signed bool) int {
	t.Helper()
	c, rec := newReq(t, "id", "n-public")
	c.Request().Header.Set("User-Agent", userAgent)
	if signed {
		c.Request().Header.Set("Signature", `keyId="https://remote.example/users/bob#main-key"`)
	}
	require.NoError(t, f.h.Note(c))
	return rec.Code
}

// Without a signature, the host named in the User-Agent is used, as CherryPick
// does: Misskey and Mastodon put their own URL there.
func TestNoteDeliveryRules_UserAgentHost(t *testing.T) {
	misskey := "Misskey/2025.1.0 (https://ua.example)"
	mastodon := "http.rb/5.1.1 (Mastodon/4.3.0; +https://ua.example/)"
	t.Run("include listed", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"ua.example"}}}))
		assert.Equal(t, http.StatusOK, f.fetchWithUA(t, misskey, false))
		assert.Equal(t, http.StatusOK, f.fetchWithUA(t, mastodon, false))
		assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, "curl/8.0", false))
	})
	t.Run("exclude listed", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"ua.example"}}}))
		assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, misskey, false))
		assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, mastodon, false))
		assert.Equal(t, http.StatusOK, f.fetchWithUA(t, "curl/8.0", false))
	})
	t.Run("a verified signature wins over the User-Agent", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"ua.example"}}}))
		assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, misskey, true), "bob signs from remote.example")
	})
}

func TestUserAgentHost(t *testing.T) {
	tests := map[string]string{
		"http.rb/5.1.1 (Mastodon/4.3.0; +https://Mastodon.Example/)":      "mastodon.example",
		"Pleroma 2.5.0; https://pleroma.example <admin@pleroma.example>":  "pleroma.example",
		"Misskey/2025.1.0 (https://misskey.example)":                      "misskey.example",
		"Elythia/2.2.0 (https://elythia.example:8443)":                    "elythia.example:8443",
		"Misskey/2025.1.0 (https://misskey.example:443)":                  "misskey.example",
		"Friendica 'Yellow Archangel' 2024.03; https://friendica.example": "friendica.example",
		"Misskey/2025.1.0 (https://xn--r8jz45g.xn--zckzah)":               "xn--r8jz45g.xn--zckzah",
		"curl/8.0":                        "",
		"":                                "",
		"Mozilla/5.0 (X11; Linux x86_64)": "",
	}
	for ua, want := range tests {
		assert.Equal(t, want, userAgentHost(ua), ua)
	}
}

func TestNoteDeliveryRules_SignerRejected(t *testing.T) {
	targets := map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"remote.example"}}}
	t.Run("signature does not verify falls back to the User-Agent", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, targets))
		f.signer.user, f.signer.err = nil, errors.New("bad signature")
		assert.Equal(t, http.StatusNotFound, f.fetch(t, f.h.Note, "n-public", true))
		assert.Equal(t, http.StatusOK, f.fetchWithUA(t, "Misskey/2025.1.0 (https://remote.example)", true))
	})
	t.Run("suspended signer cannot name another host", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, targets))
		f.bob.IsSuspended = true
		assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, "Misskey/2025.1.0 (https://remote.example)", true))
	})
	t.Run("host this server does not deliver to", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, targets))
		f.h.SetFetchSigner(f.signer, stubDeliveryGate{"remote.example": true}, apShowSoftwareBlocker{})
		assert.Equal(t, http.StatusNotFound, f.fetch(t, f.h.Note, "n-public", true))
	})
	t.Run("own host in the User-Agent is unknown", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"local.example"}}}))
		assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, "Misskey/2025.1.0 (https://local.example)", false))
	})
}

func TestNoteDeliveryRules_PluginFailureHidesNote(t *testing.T) {
	f := newSignedFetchFixture(t)
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) {
			return nil, errors.New("db down")
		},
	}))
	f.h.SetNoteDeliveryRules(r)
	assert.Equal(t, http.StatusNotFound, f.fetch(t, f.h.Note, "n-public", true))
}

// The signature is checked once per request even when several notes are
// restricted (outbox pages).
func TestNoteDeliveryRules_FetchHostMemoized(t *testing.T) {
	f := newSignedFetchFixture(t)
	c, _ := newReq(t, "id", "x")
	c.Request().Header.Set("Signature", `keyId="https://remote.example/users/bob#main-key"`)
	assert.Equal(t, "remote.example", f.h.fetchHost(c))
	assert.Equal(t, "remote.example", f.h.fetchHost(c))
	assert.Equal(t, 1, f.signer.calls)
}

func TestNoteDeliveryRules_Outbox(t *testing.T) {
	h, users, notes := newHandlerWithOutbox(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", NotesCount: 2}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "owner", Visibility: model.NoteVisibilityPublic}
	notes.Notes["n2"] = &model.Note{ID: "n2", UserID: "owner", Visibility: model.NoteVisibilityPublic}
	h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{
		"n2": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"remote.example"}},
	}))

	c, rec := newReqQuery(t, "id", "owner", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	require.Len(t, items, 1, "the restricted note must not appear to an unsigned fetch")
	activity, _ := items[0].(map[string]any)
	object, _ := activity["object"].(map[string]any)
	assert.Equal(t, "https://example.com/notes/n1", object["id"])
}

func TestNoteDeliveryRules_Featured(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	author := &model.User{ID: "u1", Username: "alice"}
	userRepo.Users["u1"] = author
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic}
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: "u1", NoteID: "n1"}))
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p2", UserID: "u1", NoteID: "n2"}))
	h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{
		"n2": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"remote.example"}},
	}))

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	items, _ := col["orderedItems"].([]any)
	require.Len(t, items, 1)
	item, _ := items[0].(map[string]any)
	assert.Equal(t, "https://example.com/notes/n1", item["id"])
}
