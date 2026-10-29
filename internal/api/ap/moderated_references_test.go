package ap

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNote_AllowsSuspendedAndDeletedRelatedAuthors(t *testing.T) {
	for _, moderation := range []struct {
		name string
		user *model.User
	}{
		{name: "suspended", user: &model.User{IsSuspended: true}},
		{name: "deleted", user: &model.User{IsDeleted: true}},
	} {
		for _, relation := range []string{"reply", "quote", "mention"} {
			t.Run(moderation.name+"/"+relation, func(t *testing.T) {
				h, userRepo, noteRepo, _ := newHandler(t)
				rootAuthor := &model.User{ID: "root-author", Username: "alice", FederationStatus: model.FederationStatusOn}
				relatedAuthor := *moderation.user
				relatedAuthor.ID, relatedAuthor.Username, relatedAuthor.FederationStatus = "related-author", "bob", model.FederationStatusOn
				userRepo.Users[rootAuthor.ID] = rootAuthor
				userRepo.Users[relatedAuthor.ID] = &relatedAuthor
				h.SetNoteRepo(noteRepo)
				h.renderer.SetMentionResolver(federation.NewUserMentionResolver(userRepo, h.renderer.URLs()))
				targetText := "related note"
				targetID := "related-note"
				noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: relatedAuthor.ID, User: &relatedAuthor, Text: &targetText, Visibility: model.NoteVisibilityPublic}
				rootText := "reply"
				root := &model.Note{ID: "root-note", UserID: rootAuthor.ID, User: rootAuthor, Text: &rootText, Visibility: model.NoteVisibilityPublic}
				if relation == "reply" {
					root.ReplyID = &targetID
				} else if relation == "quote" {
					root.RenoteID = &targetID
				} else {
					root.Mentions = []string{relatedAuthor.ID}
				}
				noteRepo.Notes[root.ID] = root
				observer := &localFederationObserverStub{available: true}
				h.SetLocalFederationObserver(observer)

				c, rec := newReq(t, "id", root.ID)
				c.Request().Header.Set("Accept", "application/activity+json")
				require.NoError(t, h.Note(c))
				assert.Equal(t, http.StatusOK, rec.Code)
				if relation == "mention" {
					assert.Contains(t, rec.Body.String(), h.renderer.URLs().UserURI(relatedAuthor.ID))
					assert.Contains(t, observer.recorded, "related-author:actor:")
				} else {
					assert.Contains(t, rec.Body.String(), h.renderer.URLs().NoteURI(targetID))
					assert.Contains(t, observer.recorded, "related-author:note:")
				}
				assert.Contains(t, observer.recorded, "root-author:note:")
			})
		}
	}
}

func TestNoteActivity_AllowsSuspendedAndDeletedRelatedReferences(t *testing.T) {
	for _, moderation := range []struct {
		name string
		user *model.User
	}{
		{name: "suspended", user: &model.User{IsSuspended: true}},
		{name: "deleted", user: &model.User{IsDeleted: true}},
	} {
		for _, relation := range []string{"reply", "quote", "mention"} {
			t.Run(moderation.name+"/"+relation, func(t *testing.T) {
				h, userRepo, noteRepo, _ := newHandler(t)
				rootAuthor := &model.User{ID: "root-author", Username: "alice", FederationStatus: model.FederationStatusOn}
				relatedAuthor := *moderation.user
				relatedAuthor.ID, relatedAuthor.Username, relatedAuthor.FederationStatus = "related-author", "bob", model.FederationStatusOn
				userRepo.Users[rootAuthor.ID] = rootAuthor
				userRepo.Users[relatedAuthor.ID] = &relatedAuthor
				h.SetNoteRepo(noteRepo)
				h.renderer.SetMentionResolver(federation.NewUserMentionResolver(userRepo, h.renderer.URLs()))
				targetText := "related note"
				targetID := "related-note"
				noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: relatedAuthor.ID, User: &relatedAuthor, Text: &targetText, Visibility: model.NoteVisibilityPublic}
				rootText := "reply"
				root := &model.Note{ID: "root-note", UserID: rootAuthor.ID, User: rootAuthor, Text: &rootText, Visibility: model.NoteVisibilityPublic}
				if relation == "reply" {
					root.ReplyID = &targetID
				} else if relation == "quote" {
					root.RenoteID = &targetID
				} else {
					root.Mentions = []string{relatedAuthor.ID}
				}
				noteRepo.Notes[root.ID] = root
				observer := &localFederationObserverStub{available: true}
				h.SetLocalFederationObserver(observer)

				c, rec := newReq(t, "id", root.ID)
				require.NoError(t, h.NoteActivity(c))
				assert.Equal(t, http.StatusOK, rec.Code)
				if relation == "mention" {
					assert.Contains(t, rec.Body.String(), h.renderer.URLs().UserURI(relatedAuthor.ID))
					assert.Contains(t, observer.recorded, "related-author:actor:")
				} else {
					assert.Contains(t, rec.Body.String(), h.renderer.URLs().NoteURI(targetID))
					assert.Contains(t, observer.recorded, "related-author:note:")
				}
			})
		}
	}
}
func TestOutbox_AllowsSuspendedAndDeletedMentionedUsers(t *testing.T) {
	h, userRepo, noteRepo := newHandlerWithOutbox(t)
	author := &model.User{ID: "author", Username: "alice", FederationStatus: model.FederationStatusOn}
	suspended := &model.User{ID: "suspended", Username: "bob", FederationStatus: model.FederationStatusOn, IsSuspended: true}
	deleted := &model.User{ID: "deleted", Username: "carol", FederationStatus: model.FederationStatusOn, IsDeleted: true}
	for _, user := range []*model.User{author, suspended, deleted} {
		userRepo.Users[user.ID] = user
	}
	text := "hello"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: author.ID, User: author, Mentions: []string{suspended.ID, deleted.ID}, Text: &text, Visibility: model.NoteVisibilityPublic}
	observer := &localFederationObserverStub{available: true}
	h.SetLocalFederationObserver(observer)
	h.renderer.SetMentionResolver(federation.NewUserMentionResolver(userRepo, h.renderer.URLs()))

	c, rec := newReqQuery(t, "id", author.ID, "page=true")
	require.NoError(t, h.Outbox(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), h.renderer.URLs().UserURI(suspended.ID))
	assert.Contains(t, rec.Body.String(), h.renderer.URLs().UserURI(deleted.ID))
	assert.Contains(t, observer.recorded, "suspended:actor:")
	assert.Contains(t, observer.recorded, "deleted:actor:")
}

func TestFollowers_Page_AllowsSuspendedAndDeletedRelatedActors(t *testing.T) {
	h, users, following := newHandlerWithFollowing(t)
	users.Users["owner"] = &model.User{ID: "owner", Username: "alice", FollowersCount: 2, FederationStatus: model.FederationStatusOn}
	users.Profiles["owner"] = &model.UserProfile{UserID: "owner", FollowersVisibility: model.FollowingVisibilityPublic}
	users.Users["suspended"] = &model.User{ID: "suspended", Username: "bob", FederationStatus: model.FederationStatusOn, IsSuspended: true}
	users.Users["deleted"] = &model.User{ID: "deleted", Username: "carol", FederationStatus: model.FederationStatusOn, IsDeleted: true}
	require.NoError(t, following.Create(&model.Following{ID: "f1", FolloweeID: "owner", FollowerID: "suspended"}))
	require.NoError(t, following.Create(&model.Following{ID: "f2", FolloweeID: "owner", FollowerID: "deleted"}))
	observer := &localFederationObserverStub{available: true}
	h.SetLocalFederationObserver(observer)

	c, rec := newReqQuery(t, "id", "owner", "page=true")
	require.NoError(t, h.Followers(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), h.renderer.URLs().UserURI("suspended"))
	assert.Contains(t, rec.Body.String(), h.renderer.URLs().UserURI("deleted"))
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	items, ok := page["orderedItems"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{h.renderer.URLs().UserURI("suspended"), h.renderer.URLs().UserURI("deleted")}, items)
}

func TestOutbox_RejectsMentionedPendingAndOffUsers(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusPending, model.FederationStatusOff} {
		t.Run(string(status), func(t *testing.T) {
			h, userRepo, noteRepo := newHandlerWithOutbox(t)
			author := &model.User{ID: "author", Username: "alice", FederationStatus: model.FederationStatusOn}
			related := &model.User{ID: "related", Username: "bob", FederationStatus: status}
			userRepo.Users[author.ID] = author
			userRepo.Users[related.ID] = related
			text := "hello"
			noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: author.ID, User: author, Mentions: []string{related.ID}, Text: &text, Visibility: model.NoteVisibilityPublic}
			observer := &localFederationObserverStub{available: true, unavailable: map[string]bool{related.ID: true}}
			h.SetLocalFederationObserver(observer)
			h.renderer.SetMentionResolver(federation.NewUserMentionResolver(userRepo, h.renderer.URLs()))

			c, rec := newReqQuery(t, "id", author.ID, "page=true")
			require.NoError(t, h.Outbox(c))
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.NotContains(t, rec.Body.String(), h.renderer.URLs().UserURI(related.ID))
			assert.Equal(t, []string{"author:collection:"}, observer.recorded)
		})
	}
}
