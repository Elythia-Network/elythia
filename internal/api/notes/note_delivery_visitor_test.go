package notes

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

// visitorRules restricts the notes in targets on a server at local.example.
func visitorRules(t *testing.T, targets map[string]plugin.DeliveryTargets) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.NewForServer("https://local.example")
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) { return targets, nil },
	}))
	return r
}

func showAs(t *testing.T, h *Handler, viewer *model.User) map[string]any {
	t.Helper()
	c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
	if viewer != nil {
		setAuthUser(c, viewer)
	}
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// A viewer who is not signed in is judged as this server's own host: a note
// whose conditions keep it from local.example is blanked for them, and only
// for them (#4203).
func TestShow_HiddenFromVisitorsByDeliveryRules(t *testing.T) {
	tests := []struct {
		name       string
		targets    plugin.DeliveryTargets
		wantHidden bool
	}{
		{name: "exclude own host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example"}}, wantHidden: true},
		{name: "exclude other host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}}, wantHidden: false},
		{name: "include own host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example", "local.example"}}, wantHidden: false},
		{name: "include other host", targets: plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}}, wantHidden: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, noteRepo := newShowHandler(t)
			noteRepo.Notes["n1"] = &model.Note{
				ID: "n1", UserID: "author", Visibility: "public",
				Text: strPtr("body"),
				User: &model.User{ID: "author", Username: "author"},
			}
			h.SetNoteDeliveryRules(visitorRules(t, map[string]plugin.DeliveryTargets{"n1": tt.targets}))

			anon := showAs(t, h, nil)
			if tt.wantHidden {
				assert.Equal(t, true, anon["isHidden"])
				assert.Nil(t, anon["text"])
			} else {
				assert.NotEqual(t, true, anon["isHidden"])
				assert.Equal(t, "body", anon["text"])
			}

			signedIn := showAs(t, h, &model.User{ID: "viewer"})
			assert.Equal(t, "body", signedIn["text"], "signed-in local users are not narrowed")
		})
	}
}

// A remote note is never judged: the conditions are about local notes.
func TestShow_RemoteNoteNotJudgedForVisitors(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: "public", UserHost: &host,
		Text: strPtr("body"),
		User: &model.User{ID: "author", Username: "author", Host: &host},
	}
	h.SetNoteDeliveryRules(visitorRules(t, map[string]plugin.DeliveryTargets{
		"n1": {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"local.example"}},
	}))
	assert.Equal(t, "body", showAs(t, h, nil)["text"])
}

// While the plugin cannot report the conditions, the note is blanked for
// viewers who are not signed in (#4203).
func TestShow_PluginFailureHidesFromVisitors(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: "public",
		Text: strPtr("body"),
		User: &model.User{ID: "author", Username: "author"},
	}
	r := notedelivery.NewForServer("https://local.example")
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) {
			return nil, errors.New("db down")
		},
	}))
	h.SetNoteDeliveryRules(r)
	anon := showAs(t, h, nil)
	assert.Equal(t, true, anon["isHidden"])
	assert.Nil(t, anon["text"])
	assert.Equal(t, "body", showAs(t, h, &model.User{ID: "viewer"})["text"])
}
