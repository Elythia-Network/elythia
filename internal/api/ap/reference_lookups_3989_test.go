package ap

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apLookupCalls counts the repository reads one ActivityPub GET performs.
type apLookupCalls map[string]int

type countingLookupUserRepo struct {
	*testutil.MockUserRepository
	calls apLookupCalls
}

func (r *countingLookupUserRepo) FindByID(userID string) (*model.User, error) {
	r.calls["user.FindByID"]++
	return r.MockUserRepository.FindByID(userID)
}

func (r *countingLookupUserRepo) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	r.calls["user.FindProfileByUserID"]++
	return r.MockUserRepository.FindProfileByUserID(userID)
}

func (r *countingLookupUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	r.calls["user.FindManyByIDs"]++
	return r.MockUserRepository.FindManyByIDs(ids)
}

type countingLookupNoteRepo struct {
	*testutil.MockNoteRepository
	calls apLookupCalls
}

func (r *countingLookupNoteRepo) FindByID(noteID string) (*model.Note, error) {
	r.calls["note.FindByID"]++
	return r.MockNoteRepository.FindByID(noteID)
}

func (r *countingLookupNoteRepo) FindByIDWithRelations(noteID string) (*model.Note, error) {
	r.calls["note.FindByIDWithRelations"]++
	return r.MockNoteRepository.FindByIDWithRelations(noteID)
}

const (
	lookupNotePlain      = "at1cdxc09q1h0001"
	lookupNoteParent     = "at1cf7mo9q1h0002"
	lookupNoteReply      = "at1cghxc9q1h0003"
	lookupNoteOffParent  = "at1chs809q1h0004"
	lookupNoteOffReply   = "at1cj2io9q1h0005"
	lookupNoteOffMention = "at1ckctc9q1h0006"
)

// newLookupCountingNoteHandler wires the /notes/:id handler the way the server
// does (mention / note resolvers on the renderer, reference observer on) over
// repositories that count every read.
func newLookupCountingNoteHandler(tb testing.TB) (*Handler, apLookupCalls) {
	tb.Helper()
	calls := apLookupCalls{}
	userRepo := &countingLookupUserRepo{MockUserRepository: testutil.NewMockUserRepository(), calls: calls}
	noteRepo := &countingLookupNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository(), calls: calls}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(tb, err)
	urls := activitypub.NewURLBuilder("https://example.com")
	renderer := activitypub.NewRenderer(urls)
	observer := &availabilityObserver{}
	mentions := corefederation.NewUserMentionResolver(userRepo, urls)
	mentions.SetLocalFederationObserver(observer)
	renderer.SetMentionResolver(mentions)
	renderer.SetNoteResolver(noteRepo)
	userSvc := coreuser.NewService(userRepo, noteRepo, testutil.NewMockUserNotePiningRepository(), idGen, nil)
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(renderer, userSvc, querySvc, &memoryKeypairRepo{items: map[string]*model.UserKeypair{}}, idGen, nil, nil, nil, "", nil, nil)
	h.SetNoteRepo(noteRepo)
	h.SetLocalFederationObserver(observer)

	referencedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	user := func(id, username string, status model.FederationStatus) *model.User {
		u := &model.User{ID: id, Username: username, FederationStatus: status, FirstExternalReferenceAt: &referencedAt}
		userRepo.Users[id] = u
		userRepo.Profiles[id] = &model.UserProfile{UserID: id}
		return u
	}
	author := user("author", "alice", model.FederationStatusOn)
	bob := user("bob", "bob", model.FederationStatusOn)
	carol := user("carol", "carol", model.FederationStatusOn)
	parentAuthor := user("parent", "dave", model.FederationStatusOn)
	// ローカル専用 (#3852) に切り替えた利用者。参照すると 404 か、メンションから外れる。
	off := user("off", "erin", model.FederationStatusOff)

	note := func(noteID string, by *model.User, text string, mentions []string, replyID *string) {
		noteRepo.Notes[noteID] = &model.Note{
			ID: noteID, UserID: by.ID, User: by, Text: &text, Mentions: mentions,
			ReplyID: replyID, Visibility: model.NoteVisibilityPublic,
		}
	}
	parentID, offParentID := lookupNoteParent, lookupNoteOffParent
	note(lookupNotePlain, author, "hello world", nil, nil)
	note(lookupNoteParent, parentAuthor, "parent", nil, nil)
	note(lookupNoteReply, author, "@bob @carol hi", []string{bob.ID, carol.ID}, &parentID)
	note(lookupNoteOffParent, off, "local only now", nil, nil)
	note(lookupNoteOffReply, author, "reply", nil, &offParentID)
	note(lookupNoteOffMention, author, "@bob @erin hi", []string{bob.ID, off.ID}, nil)
	return h, calls
}

// TestNote_ReferenceLookupsPerRequest pins how many repository reads one
// GET /notes/:id performs while the local-only reference check (#3852) is on,
// and that the bytes served are the ones served before the reads were shared
// (#3989). The note itself is read once by the query (FindByIDWithRelations).
func TestNote_ReferenceLookupsPerRequest(t *testing.T) {
	tests := []struct {
		name       string
		noteID     string
		wantStatus int
		wantCalls  apLookupCalls
		// golden is the body under testdata/reference_lookups_3989, empty for no body.
		golden string
	}{
		{
			// 作者は投稿の読み取りで読んだ行を使う。以前は user 1 + profile 1。
			name:       "plain text",
			noteID:     lookupNotePlain,
			wantStatus: http.StatusOK,
			wantCalls:  apLookupCalls{"note.FindByIDWithRelations": 1},
			golden:     "plain.json",
		},
		{
			// メンションの2人は描画で1回ずつ引き、確認はそれを使う。残るのは返信先の
			// 投稿 (描画の URI と確認で2回) と、その作者。以前は user 6 + profile 4 + note 2。
			name:       "reply with two local mentions",
			noteID:     lookupNoteReply,
			wantStatus: http.StatusOK,
			wantCalls:  apLookupCalls{"note.FindByIDWithRelations": 1, "note.FindByID": 2, "user.FindByID": 3},
			golden:     "reply_mentions.json",
		},
		{
			// 返信先の作者がローカル専用なので 404。以前は user 2 + profile 2 + note 2。
			name:       "reply to a local-only account is not found",
			noteID:     lookupNoteOffReply,
			wantStatus: http.StatusNotFound,
			wantCalls:  apLookupCalls{"note.FindByIDWithRelations": 1, "note.FindByID": 2, "user.FindByID": 1},
		},
		{
			// ローカル専用の相手はメンションから外れ、応答に出ない。以前は user 4 + profile 2。
			name:       "mention of a local-only account is left out",
			noteID:     lookupNoteOffMention,
			wantStatus: http.StatusOK,
			wantCalls:  apLookupCalls{"note.FindByIDWithRelations": 1, "user.FindByID": 2},
			golden:     "local_only_mention.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, calls := newLookupCountingNoteHandler(t)
			c, rec := newReq(t, "id", tt.noteID)
			require.NoError(t, h.Note(c))
			require.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantCalls, calls)
			want := ""
			if tt.golden != "" {
				b, err := os.ReadFile(filepath.Join("testdata", "reference_lookups_3989", tt.golden))
				require.NoError(t, err)
				want = string(b)
			}
			assert.Equal(t, want, rec.Body.String(), "the served bytes must not change")
		})
	}
}

// TestNoteActivity_ReferenceLookupsPerRequest covers the Create wrapper, which
// renders the same note through the request-scoped renderer.
func TestNoteActivity_ReferenceLookupsPerRequest(t *testing.T) {
	h, calls := newLookupCountingNoteHandler(t)
	c, rec := newReq(t, "id", lookupNoteReply)
	require.NoError(t, h.NoteActivity(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, apLookupCalls{"note.FindByIDWithRelations": 1, "note.FindByID": 2, "user.FindByID": 3}, calls)
	assert.Contains(t, rec.Body.String(), `"type":"Create"`)
	assert.Contains(t, rec.Body.String(), `"href":"https://example.com/users/carol"`)
}

// TestNote_LocalOnlyAuthorFromQueryIsNotServed checks that reusing the author
// row read with the note keeps the local-only check (#3852): the row decides.
func TestNote_LocalOnlyAuthorFromQueryIsNotServed(t *testing.T) {
	h, calls := newLookupCountingNoteHandler(t)
	c, rec := newReq(t, "id", lookupNoteOffParent)
	require.NoError(t, h.Note(c))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, apLookupCalls{"note.FindByIDWithRelations": 1}, calls)
}

// BenchmarkNote_ReferenceCheck measures GET /notes/:id with the reference
// check on, over counting mock repositories (no database round trips).
func BenchmarkNote_ReferenceCheck(b *testing.B) {
	for _, bc := range []struct {
		name   string
		noteID string
	}{
		{name: "plain", noteID: lookupNotePlain},
		{name: "reply_mentions", noteID: lookupNoteReply},
	} {
		b.Run(bc.name, func(b *testing.B) {
			h, calls := newLookupCountingNoteHandler(b)
			e := echo.New()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set(echo.HeaderAccept, "application/activity+json")
				rec := httptest.NewRecorder()
				c := e.NewContext(req, rec)
				c.SetParamNames("id")
				c.SetParamValues(bc.noteID)
				if err := h.Note(c); err != nil || rec.Code != http.StatusOK {
					b.Fatalf("status %d err %v", rec.Code, err)
				}
			}
			b.StopTimer()
			total := 0
			for _, n := range calls {
				total += n
			}
			b.ReportMetric(float64(total)/float64(b.N), "repo/op")
		})
	}
}
