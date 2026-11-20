package ap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

// An unknown requester is judged as an anonymous viewer of this server, the
// same as notes/show, the embed and the feed: a note whose conditions name
// this server's host reads the same through ActivityPub as on the web (#4203).
func TestNoteDeliveryRules_UnknownRequesterIsJudgedAsOwnHost(t *testing.T) {
	anonymous := map[string]string{
		"no User-Agent":          "",
		"unknown User-Agent":     "curl/8.0",
		"own host in User-Agent": "Misskey/2025.1.0 (https://local.example)",
	}
	for name, ua := range anonymous {
		t.Run(name, func(t *testing.T) {
			f := newSignedFetchFixture(t)
			f.h.SetNoteDeliveryRules(ownHostDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example"}}}))
			assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, ua, false), "exclude own host hides it from anonymous fetches too")

			f = newSignedFetchFixture(t)
			f.h.SetNoteDeliveryRules(ownHostDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example", "local.example"}}}))
			assert.Equal(t, http.StatusOK, f.fetchWithUA(t, ua, false), "include own host shows it to anonymous fetches")

			f = newSignedFetchFixture(t)
			f.h.SetNoteDeliveryRules(ownHostDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}}}))
			assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, ua, false))

			f = newSignedFetchFixture(t)
			f.h.SetNoteDeliveryRules(ownHostDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}}}))
			assert.Equal(t, http.StatusOK, f.fetchWithUA(t, ua, false))
		})
	}
	t.Run("signed requester is judged by its own host", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(ownHostDeliveryRules(t, map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example"}}}))
		assert.Equal(t, http.StatusOK, f.fetch(t, f.h.Note, "n-public", true), "remote.example is not excluded")
	})
}

// A signer this server refuses must not read an exclude note as an unknown
// host: the excluded host would only have to sign its fetch (#4199).
func TestNoteDeliveryRules_SignerRejectedExclude(t *testing.T) {
	targets := map[string]plugin.DeliveryTargets{"n-public": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"remote.example"}}}
	tests := []struct {
		name   string
		reject func(f *signedFetchFixture)
	}{
		{name: "suspended", reject: func(f *signedFetchFixture) { f.bob.IsSuspended = true }},
		{name: "deleted", reject: func(f *signedFetchFixture) { f.bob.IsDeleted = true }},
		{name: "host this server does not deliver to", reject: func(f *signedFetchFixture) {
			f.h.SetFetchSigner(f.signer, stubDeliveryGate{"remote.example": true}, apShowSoftwareBlocker{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSignedFetchFixture(t)
			f.h.SetNoteDeliveryRules(apDeliveryRules(t, targets))
			tt.reject(f)
			assert.Equal(t, http.StatusNotFound, f.fetch(t, f.h.Note, "n-public", true))
			assert.Equal(t, http.StatusNotFound, f.fetchWithUA(t, "Misskey/2025.1.0 (https://other.example)", true))
		})
	}
	t.Run("unsigned unknown host still reads an exclude note", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.h.SetNoteDeliveryRules(apDeliveryRules(t, targets))
		assert.Equal(t, http.StatusOK, f.fetch(t, f.h.Note, "n-public", false))
	})
}

func failingDeliveryRules(t *testing.T) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) {
			return nil, errors.New("db down")
		},
	}))
	return r
}

// While the plugin cannot report the targets, a fetch answers 503 rather than
// 404, for every note alike: a peer that takes 404 as "deleted" would not
// fetch the note again once the plugin recovers (#4202).
func TestNoteDeliveryRules_PluginFailureIsUnavailable(t *testing.T) {
	for name := range signedFetchEndpoints(newSignedFetchFixture(t).h) {
		t.Run(name, func(t *testing.T) {
			f := newSignedFetchFixture(t)
			f.h.SetNoteDeliveryRules(failingDeliveryRules(t))
			handler := signedFetchEndpoints(f.h)[name]
			assert.Equal(t, http.StatusServiceUnavailable, f.fetch(t, handler, "n-public", true), "signed")
			assert.Equal(t, http.StatusServiceUnavailable, f.fetch(t, handler, "n-public", false), "unsigned")
		})
	}
}

// An author whose notes do not federate answers 404 even while the plugin is
// down: 503 asks the peer to fetch again, which it should not (#4202).
func TestNoteDeliveryRules_PluginFailureKeepsUnavailableAuthorNotFound(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		for name := range signedFetchEndpoints(newSignedFetchFixture(t).h) {
			t.Run(string(status)+" "+name, func(t *testing.T) {
				f := newSignedFetchFixture(t)
				f.users.Users["alice"].FederationStatus = status
				f.h.SetLocalFederationObserver(&availabilityObserver{})
				f.h.SetNoteDeliveryRules(failingDeliveryRules(t))
				handler := signedFetchEndpoints(f.h)[name]
				assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-public", true))
			})
		}
	}
	t.Run("available author", func(t *testing.T) {
		f := newSignedFetchFixture(t)
		f.users.Users["alice"].FederationStatus = model.FederationStatusOn
		f.h.SetLocalFederationObserver(&availabilityObserver{})
		f.h.SetNoteDeliveryRules(failingDeliveryRules(t))
		assert.Equal(t, http.StatusServiceUnavailable, f.fetch(t, f.h.Note, "n-public", true))
		assert.Equal(t, http.StatusServiceUnavailable, f.fetch(t, f.h.NoteActivity, "n-public", true))
	})
}

// A value that is not a host denies only that note, which is not an outage:
// it stays 404 so it looks like any other note the requester may not see.
func TestNoteDeliveryRules_NonHostValueIsNotFound(t *testing.T) {
	for name := range signedFetchEndpoints(newSignedFetchFixture(t).h) {
		t.Run(name, func(t *testing.T) {
			f := newSignedFetchFixture(t)
			f.h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{
				"n-public": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"https://other.example"}},
			}))
			handler := signedFetchEndpoints(f.h)[name]
			assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-public", true))
		})
	}
}

// The signature is checked once per request even when several notes are
// restricted (outbox pages).
func TestNoteDeliveryRules_FetchHostMemoized(t *testing.T) {
	f := newSignedFetchFixture(t)
	c, _ := newReq(t, "id", "x")
	c.Request().Header.Set("Signature", `keyId="https://remote.example/users/bob#main-key"`)
	for range 2 {
		host, rejected := f.h.fetchHost(c)
		assert.Equal(t, "remote.example", host)
		assert.False(t, rejected)
	}
	assert.Equal(t, 1, f.signer.calls)
}

// A followers-only note needs the signer for visibility and again for the
// delivery rules; the signature is verified once for both (#4199).
func TestNoteDeliveryRules_SignatureVerifiedOnce(t *testing.T) {
	f := newSignedFetchFixture(t)
	f.follow(t)
	f.h.SetNoteDeliveryRules(apDeliveryRules(t, map[string]plugin.DeliveryTargets{
		"n-followers": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"remote.example"}},
	}))
	assert.Equal(t, http.StatusOK, f.fetch(t, f.h.Note, "n-followers", true))
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

// ownHostDeliveryRules restricts notes on a server at local.example, as the
// server builds the registry from its url.
func ownHostDeliveryRules(t *testing.T, targets map[string]plugin.DeliveryTargets) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.NewForServer("https://local.example")
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return targets, nil },
	}))
	return r
}

// The outbox and featured lists judge an unsigned fetch as this server's own
// host, the same as a single note (#4203).
var ownHostListTargets = map[string]plugin.DeliveryTargets{
	"n-exclude-own":  {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example"}},
	"n-include-own":  {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"remote.example", "local.example"}},
	"n-include-else": {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"remote.example"}},
}

func TestNoteDeliveryRules_OutboxJudgesUnsignedFetchAsOwnHost(t *testing.T) {
	h, users, notes := newHandlerWithOutbox(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", NotesCount: 4}
	for _, id := range []string{"n-exclude-own", "n-include-own", "n-include-else", "n-plain"} {
		notes.Notes[id] = &model.Note{ID: id, UserID: "owner", Visibility: model.NoteVisibilityPublic}
	}
	h.SetNoteDeliveryRules(ownHostDeliveryRules(t, ownHostListTargets))

	c, rec := newReqQuery(t, "id", "owner", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, _ := page["orderedItems"].([]any)
	var got []string
	for _, it := range items {
		activity, _ := it.(map[string]any)
		object, _ := activity["object"].(map[string]any)
		got = append(got, object["id"].(string))
	}
	assert.ElementsMatch(t, []string{"https://example.com/notes/n-include-own", "https://example.com/notes/n-plain"}, got)
}

func TestNoteDeliveryRules_FeaturedJudgesUnsignedFetchAsOwnHost(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	author := &model.User{ID: "u1", Username: "alice"}
	userRepo.Users["u1"] = author
	for i, id := range []string{"n-exclude-own", "n-include-own", "n-include-else", "n-plain"} {
		noteRepo.Notes[id] = &model.Note{ID: id, UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic}
		require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: fmt.Sprintf("p%d", i), UserID: "u1", NoteID: id}))
	}
	h.SetNoteDeliveryRules(ownHostDeliveryRules(t, ownHostListTargets))

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	items, _ := col["orderedItems"].([]any)
	var got []string
	for _, it := range items {
		item, _ := it.(map[string]any)
		got = append(got, item["id"].(string))
	}
	assert.ElementsMatch(t, []string{"https://example.com/notes/n-include-own", "https://example.com/notes/n-plain"}, got)
}

func TestNoteDeliveryRules_OutboxPluginFailureIsUnavailable(t *testing.T) {
	h, users, notes := newHandlerWithOutbox(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", NotesCount: 1}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "owner", Visibility: model.NoteVisibilityPublic}
	h.SetNoteDeliveryRules(failingDeliveryRules(t))

	c, rec := newReqQuery(t, "id", "owner", "page=true")
	require.NoError(t, h.Outbox(c))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "an empty page would read as no notes")
}

// While the plugin is down, featured asks only about the notes it would list:
// a user who pinned only local-only or non-public notes gets the same empty
// collection as in normal times, not 503 (#4202).
func TestNoteDeliveryRules_FeaturedPluginFailureSkipsUnlistedNotes(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	author := &model.User{ID: "u1", Username: "alice"}
	userRepo.Users["u1"] = author
	noteRepo.Notes["local-only"] = &model.Note{ID: "local-only", UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic, LocalOnly: true}
	noteRepo.Notes["followers"] = &model.Note{ID: "followers", UserID: "u1", User: author, Visibility: model.NoteVisibilityFollowers}
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: "u1", NoteID: "local-only"}))
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p2", UserID: "u1", NoteID: "followers"}))
	h.SetNoteDeliveryRules(failingDeliveryRules(t))

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var col map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &col))
	assert.Empty(t, col["orderedItems"])
}

func TestNoteDeliveryRules_FeaturedPluginFailureIsUnavailable(t *testing.T) {
	h, userRepo, noteRepo, piningRepo := newHandlerWithPining(t)
	author := &model.User{ID: "u1", Username: "alice"}
	userRepo.Users["u1"] = author
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", User: author, Visibility: model.NoteVisibilityPublic}
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: "u1", NoteID: "n1"}))
	h.SetNoteDeliveryRules(failingDeliveryRules(t))

	c, rec := newReq(t, "id", "u1")
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "an empty collection would read as unpinned")
}
