package ap

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUser_FiltersUnavailableLocalAliasesAndMovedTo(t *testing.T) {
	h, users, _, keys := newHandler(t)
	owner := &model.User{ID: "owner", Username: "alice", FederationStatus: model.FederationStatusOn}
	akaOn := &model.User{ID: "aka-on", Username: "old-alice", FederationStatus: model.FederationStatusOn}
	akaOff := &model.User{ID: "aka-off", Username: "disabled-alias", FederationStatus: model.FederationStatusOn}
	moveOn := &model.User{ID: "move-on", Username: "new-alice", FederationStatus: model.FederationStatusOn}
	moveOff := &model.User{ID: "move-off", Username: "disabled-move", FederationStatus: model.FederationStatusOn}
	for _, u := range []*model.User{owner, akaOn, akaOff, moveOn, moveOff} {
		users.Users[u.ID] = u
	}
	aka := h.renderer.URLs().UserURI(akaOn.ID) + ", " + h.renderer.URLs().UserURI(akaOff.ID) + ", https://remote.example/users/old-alice"
	move := h.renderer.URLs().UserURI(moveOn.ID)
	owner.AlsoKnownAs = &aka
	owner.MovedToURI = &move
	keys.items[owner.ID] = &model.UserKeypair{UserID: owner.ID, PublicKey: "PUBKEY"}
	observer := &localFederationObserverStub{available: true, unavailable: map[string]bool{akaOff.ID: true, moveOff.ID: true}}
	h.SetLocalFederationObserver(observer)

	c, rec := newReq(t, "id", owner.ID)
	require.NoError(t, h.User(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var person map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &person))
	aliases, ok := person["alsoKnownAs"].([]any)
	require.True(t, ok)
	assert.Equal(t, []any{h.renderer.URLs().UserURI(akaOn.ID), "https://remote.example/users/old-alice"}, aliases)
	assert.Equal(t, h.renderer.URLs().UserURI(moveOn.ID), person["movedTo"])
	assert.NotContains(t, rec.Body.String(), h.renderer.URLs().UserURI(akaOff.ID))
	assert.NotContains(t, rec.Body.String(), h.renderer.URLs().UserURI(moveOff.ID))
	assert.Equal(t, []string{"owner:actor:", "aka-on:actor:", "move-on:actor:"}, observer.recorded)
}

func TestUser_OmitsMissingLocalAlias(t *testing.T) {
	h, users, _, keys := newHandler(t)
	owner := &model.User{ID: "owner", Username: "alice", FederationStatus: model.FederationStatusOn}
	missingAlias := h.renderer.URLs().UserURI("deleted-alias")
	owner.AlsoKnownAs = &missingAlias
	users.Users[owner.ID] = owner
	keys.items[owner.ID] = &model.UserKeypair{UserID: owner.ID, PublicKey: "PUBKEY"}
	observer := &localFederationObserverStub{available: true}
	h.SetLocalFederationObserver(observer)

	c, rec := newReq(t, "id", owner.ID)
	require.NoError(t, h.User(c))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), missingAlias)
	assert.Equal(t, []string{"owner:actor:"}, observer.recorded)
}

type failAPUserLookupRepo struct {
	*testutil.MockUserRepository
	failingID string
}

func (r failAPUserLookupRepo) FindByID(id string) (*model.User, error) {
	if id == r.failingID {
		return nil, errors.New("alias lookup unavailable")
	}
	return r.MockUserRepository.FindByID(id)
}

func TestUser_AliasLookupFailureReturnsEmptyServerError(t *testing.T) {
	h, users, notes, keys := newHandler(t)
	owner := &model.User{ID: "owner", Username: "alice", FederationStatus: model.FederationStatusOn}
	validAlias := &model.User{ID: "valid-alias", Username: "old-alice", FederationStatus: model.FederationStatusOn}
	users.Users[owner.ID] = owner
	users.Users[validAlias.ID] = validAlias
	aka := h.renderer.URLs().UserURI(validAlias.ID) + ", " + h.renderer.URLs().UserURI("broken-alias")
	owner.AlsoKnownAs = &aka
	keys.items[owner.ID] = &model.UserKeypair{UserID: owner.ID, PublicKey: "PUBKEY"}
	repo := failAPUserLookupRepo{MockUserRepository: users, failingID: "broken-alias"}
	h.userService = user.NewService(repo, notes, testutil.NewMockUserNotePiningRepository(), h.idGen, nil)
	observer := &localFederationObserverStub{available: true}
	h.SetLocalFederationObserver(observer)

	c, rec := newReq(t, "id", owner.ID)
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, rec.Body.String(), "partial actor data must not be returned after an alias lookup error")
	assert.NotContains(t, observer.recorded, "valid-alias:actor:", "all local alias lookups finish before any alias is observed")
}

func TestUser_ObservationFailureReturnsEmptyServerError(t *testing.T) {
	h, users, _, keys := newHandler(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", FederationStatus: model.FederationStatusOn}
	keys.items["owner"] = &model.UserKeypair{UserID: "owner", PublicKey: "PUBKEY"}
	h.SetLocalFederationObserver(&localFederationObserverStub{available: true, observeErr: errors.New("reference store unavailable")})

	c, rec := newReq(t, "id", "owner")
	require.NoError(t, h.User(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, rec.Body.String(), "the Person document must not be served when reference recording fails")
}

func TestFollowers_Page_FiltersUnavailableLocalActors(t *testing.T) {
	h, users, following := newHandlerWithFollowing(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", FollowersCount: 2, FederationStatus: model.FederationStatusOn}
	users.Profiles["owner"] = &model.UserProfile{UserID: "owner", FollowersVisibility: model.FollowingVisibilityPublic}
	users.Users["available"] = &model.User{ID: "available", Username: "bob", FederationStatus: model.FederationStatusOn}
	users.Users["disabled"] = &model.User{ID: "disabled", Username: "carol", FederationStatus: model.FederationStatusOff}
	remoteHost, remoteURI := "remote.example", "https://remote.example/users/dave"
	users.Users["remote"] = &model.User{ID: "remote", Username: "dave", Host: &remoteHost, URI: &remoteURI}
	for i, counterpart := range []string{"available", "disabled", "remote"} {
		require.NoError(t, following.Create(&model.Following{ID: string(rune('a' + i)), FolloweeID: "owner", FollowerID: counterpart}))
	}
	observer := &localFederationObserverStub{available: true, unavailable: map[string]bool{"disabled": true}}
	h.SetLocalFederationObserver(observer)

	c, rec := newReqQuery(t, "id", "owner", "page=true")
	require.NoError(t, h.Followers(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, ok := page["orderedItems"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"https://example.com/users/available", remoteURI}, items)
	assert.Equal(t, float64(2), page["totalItems"], "the collection retains its current totalItems count")
	assert.NotContains(t, rec.Body.String(), "/users/disabled")
	assert.Equal(t, []string{"owner:collection:", "available:collection:"}, observer.recorded)
}

func TestFollowers_Page_UserBatchLookupErrorReturnsEmptyServerError(t *testing.T) {
	h, users, following := newHandlerWithFollowing(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", FollowersCount: 1}
	users.Profiles["owner"] = &model.UserProfile{UserID: "owner", FollowersVisibility: model.FollowingVisibilityPublic}
	users.Users["follower"] = &model.User{ID: "follower", Username: "bob"}
	require.NoError(t, following.Create(&model.Following{ID: "f1", FolloweeID: "owner", FollowerID: "follower"}))
	users.FindManyByIDsErr = errors.New("batch lookup unavailable")

	c, rec := newReqQuery(t, "id", "owner", "page=true")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, rec.Body.String(), "no partial page is returned when actor resolution fails")
}

func TestFollowers_Page_ObservationFailureReturnsEmptyServerError(t *testing.T) {
	h, users, following := newHandlerWithFollowing(t)
	owner := &model.User{ID: "owner", Username: "alice", FollowersCount: 1, FederationStatus: model.FederationStatusOn}
	owner.FirstExternalReferenceAt = ptrTimeForAPTest()
	users.Users[owner.ID] = owner
	users.Profiles[owner.ID] = &model.UserProfile{UserID: owner.ID, FollowersVisibility: model.FollowingVisibilityPublic}
	users.Users["follower"] = &model.User{ID: "follower", Username: "bob", FederationStatus: model.FederationStatusOn}
	require.NoError(t, following.Create(&model.Following{ID: "f1", FolloweeID: owner.ID, FollowerID: "follower"}))
	h.SetLocalFederationObserver(&localFederationObserverStub{available: true, observeErr: errors.New("reference store unavailable")})

	c, rec := newReqQuery(t, "id", owner.ID, "page=true")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, rec.Body.String(), "an unresolved actor reference must not produce a partial collection page")
}

func TestExtractLocalAPReferences_ReturnsSerializationError(t *testing.T) {
	h, _, _, _ := newHandler(t)
	_, err := extractLocalAPReferences(map[string]any{"unserializable": func() {}}, h.renderer.URLs())
	require.Error(t, err)
}

func TestWriteAPJSON_ReferencePoliciesReturnEmptyResponses(t *testing.T) {
	t.Run("missing related local actor does not reject response", func(t *testing.T) {
		h, _, _, _ := newHandler(t)
		h.SetLocalFederationObserver(&localFederationObserverStub{available: true})
		c, rec := newReq(t, "unused", "")

		require.NoError(t, h.writeAPJSON(c, map[string]any{"actor": h.renderer.URLs().UserURI("missing")}))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), h.renderer.URLs().UserURI("missing"))
	})

	t.Run("local note lookup error is internal server error", func(t *testing.T) {
		h, _, notes, _ := newHandler(t)
		h.SetNoteRepo(notes)
		notes.FindErr = errors.New("note lookup unavailable")
		h.SetLocalFederationObserver(&localFederationObserverStub{available: true})
		c, rec := newReq(t, "unused", "")

		require.NoError(t, h.writeAPJSON(c, map[string]any{"object": h.renderer.URLs().NoteURI("broken")}))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, rec.Body.String())
	})

	t.Run("local only note is not found", func(t *testing.T) {
		h, users, notes, _ := newHandler(t)
		h.SetNoteRepo(notes)
		users.Users["author"] = &model.User{ID: "author", Username: "alice", FederationStatus: model.FederationStatusOn}
		notes.Notes["private"] = &model.Note{ID: "private", UserID: "author", LocalOnly: true}
		observer := &localFederationObserverStub{available: true}
		h.SetLocalFederationObserver(observer)
		c, rec := newReq(t, "unused", "")

		require.NoError(t, h.writeAPJSON(c, map[string]any{"object": h.renderer.URLs().NoteURI("private")}))
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Empty(t, rec.Body.String())
		assert.Empty(t, observer.recorded, "invalid note references are rejected before author observation")
	})

	t.Run("reference user database error is internal server error", func(t *testing.T) {
		h, users, _, _ := newHandler(t)
		users.FindErr = errors.New("user lookup unavailable")
		h.SetLocalFederationObserver(&localFederationObserverStub{available: true})
		c, rec := newReq(t, "unused", "")

		require.NoError(t, h.writeAPJSON(c, map[string]any{"actor": h.renderer.URLs().UserURI("broken")}))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, rec.Body.String())
	})

	t.Run("observation error is internal server error", func(t *testing.T) {
		h, users, _, _ := newHandler(t)
		users.Users["actor"] = &model.User{ID: "actor", Username: "bob", FederationStatus: model.FederationStatusOn}
		observer := &localFederationObserverStub{available: true, observeErr: errors.New("reference store unavailable")}
		h.SetLocalFederationObserver(observer)
		c, rec := newReq(t, "unused", "")

		require.NoError(t, h.writeAPJSON(c, map[string]any{"actor": h.renderer.URLs().UserURI("actor")}))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Empty(t, rec.Body.String())
		assert.Equal(t, []string{"actor:actor:"}, observer.recorded)
	})
}

func TestQuoteAuthorization_MissingCanonicalLocalQuotedNoteReturnsEmptyNotFound(t *testing.T) {
	h, users, notes, _ := newHandler(t)
	users.Users["author"] = &model.User{ID: "author", Username: "alice", FederationStatus: model.FederationStatusOn}
	notes.Notes["target"] = &model.Note{ID: "target", UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.SetNoteRepo(notes)
	h.SetQuoteAuthorizationStore(qaStore{"approval": {ID: "approval", NoteID: "target", QuotingURI: h.renderer.URLs().NoteURI("missing-quoted")}})
	h.SetLocalFederationObserver(&quoteFederationObserver{})

	c, res := qaReq(t, "target", "approval")
	require.NoError(t, h.QuoteAuthorization(c))
	code, body, headers := res()
	assert.Equal(t, http.StatusNotFound, code)
	assert.Empty(t, body)
	assert.Equal(t, "no-store", headers.Get("Cache-Control"))
}

func TestQuoteAuthorization_LocalQuotedNoteLookupFailureReturnsEmptyServerError(t *testing.T) {
	h, users, notes, _ := newHandler(t)
	users.Users["author"] = &model.User{ID: "author", Username: "alice", FederationStatus: model.FederationStatusOn}
	notes.Notes["target"] = &model.Note{ID: "target", UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.SetNoteRepo(failingAPReferenceNoteRepo{MockNoteRepository: notes, failingID: "quoted"})
	h.SetQuoteAuthorizationStore(qaStore{"approval": {ID: "approval", NoteID: "target", QuotingURI: h.renderer.URLs().NoteURI("quoted")}})
	h.SetLocalFederationObserver(&quoteFederationObserver{})

	c, res := qaReq(t, "target", "approval")
	require.NoError(t, h.QuoteAuthorization(c))
	code, body, headers := res()
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Empty(t, body)
	assert.Equal(t, "no-store", headers.Get("Cache-Control"))
}

func TestQuoteAuthorization_ObservationFailureReturnsEmptyServerError(t *testing.T) {
	h, users, notes, _ := newHandler(t)
	users.Users["author"] = &model.User{ID: "author", Username: "alice", FederationStatus: model.FederationStatusOn}
	notes.Notes["target"] = &model.Note{ID: "target", UserID: "author", Visibility: model.NoteVisibilityPublic}
	h.SetNoteRepo(notes)
	h.SetQuoteAuthorizationStore(qaStore{"approval": {ID: "approval", NoteID: "target", QuotingURI: "https://remote.example/notes/quote"}})
	h.SetLocalFederationObserver(&localFederationObserverStub{available: true, observeErr: errors.New("reference store unavailable")})

	c, res := qaReq(t, "target", "approval")
	require.NoError(t, h.QuoteAuthorization(c))
	code, body, headers := res()
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Empty(t, body)
	assert.Equal(t, "no-store", headers.Get("Cache-Control"))
}

func TestQuoteAuthorization_RemoteQuotedNoteLookupFailureReturnsEmptyServerError(t *testing.T) {
	h, users, notes, _ := newHandler(t)
	users.Users["author"] = &model.User{ID: "author", Username: "alice"}
	notes.Notes["target"] = &model.Note{ID: "target", UserID: "author", Visibility: model.NoteVisibilityPublic}
	notes.FindByURIErr = errors.New("quoted URI lookup unavailable")
	h.SetNoteRepo(notes)
	h.SetQuoteAuthorizationStore(qaStore{"approval": {ID: "approval", NoteID: "target", QuotingURI: "https://remote.example/notes/quote"}})

	c, res := qaReq(t, "target", "approval")
	require.NoError(t, h.QuoteAuthorization(c))
	code, body, headers := res()
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Empty(t, body)
	assert.Equal(t, "no-store", headers.Get("Cache-Control"))
}

type failingAPPiningsRepo struct {
	*testutil.MockUserNotePiningRepository
	err error
}

func (r failingAPPiningsRepo) ListByUser(string) ([]*model.UserNotePining, error) {
	return nil, r.err
}

func TestFeatured_PinningLookupFailureReturnsEmptyServerError(t *testing.T) {
	h, users, notes, _ := newHandlerWithPining(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", FederationStatus: model.FederationStatusOn}
	pins := failingAPPiningsRepo{MockUserNotePiningRepository: testutil.NewMockUserNotePiningRepository(), err: errors.New("pin lookup unavailable")}
	h.userService = user.NewService(users, notes, pins, h.idGen, nil)

	c, rec := newReq(t, "id", "owner")
	require.NoError(t, h.Featured(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, rec.Body.String(), "a partial featured collection is not returned when pins cannot be read")
}

func TestOutbox_Page_RemoteRenoteTargetWithoutURIFallsBackToCreate(t *testing.T) {
	h, users, notes := newHandlerWithOutbox(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", NotesCount: 1}
	remoteHost := "remote.example"
	notes.Notes["target"] = &model.Note{ID: "target", UserID: "remote-user", UserHost: &remoteHost}
	rid := "target"
	notes.Notes["renote"] = &model.Note{ID: "renote", UserID: "owner", Visibility: model.NoteVisibilityPublic, RenoteID: &rid}

	c, rec := newReqQuery(t, "id", "owner", "page=true")
	require.NoError(t, h.Outbox(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, ok := page["orderedItems"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	activity, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Create", activity["type"], "a URI-less remote target cannot produce an invalid Announce object")
}

func TestWriteAPJSON_NoteWithMissingLocalAuthorAllowsRelatedReference(t *testing.T) {
	h, _, notes, _ := newHandler(t)
	h.SetNoteRepo(notes)
	userID := "missing-author"
	notes.Notes["public"] = &model.Note{ID: "public", UserID: userID, Visibility: model.NoteVisibilityPublic}
	h.SetLocalFederationObserver(&localFederationObserverStub{available: true})
	c, rec := newReq(t, "unused", "")

	require.NoError(t, h.writeAPJSON(c, map[string]any{"object": h.renderer.URLs().NoteURI("public")}))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), h.renderer.URLs().NoteURI("public"))
	assert.Empty(t, h.localFederationObserver.(*localFederationObserverStub).recorded, "a missing related author is not observed")
}

func TestNoteAndNoteActivity_MissingLocalAuthorReturnEmptyNotFound(t *testing.T) {
	for _, route := range []struct {
		name     string
		activity bool
	}{
		{name: "note"},
		{name: "note activity", activity: true},
	} {
		t.Run(route.name, func(t *testing.T) {
			h, _, notes, _ := newHandler(t)
			h.SetNoteRepo(notes)
			notes.Notes["orphan"] = &model.Note{ID: "orphan", UserID: "missing-author", Visibility: model.NoteVisibilityPublic}
			h.SetLocalFederationObserver(&localFederationObserverStub{available: true})
			c, rec := newReq(t, "id", "orphan")

			var err error
			if route.activity {
				err = h.NoteActivity(c)
			} else {
				err = h.Note(c)
			}
			require.NoError(t, err)
			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.Empty(t, rec.Body.String())
		})
	}
}
