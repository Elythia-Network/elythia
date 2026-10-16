package webpush_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

type recordingPush struct{ bodies []map[string]any }

// laggingReplicaRepo answers the replica reads with a stale copy taken before
// the deletion, while the primary reads see the current rows.
type laggingReplicaRepo struct {
	*testutil.MockNoteRepository
	stale          map[string]*model.Note
	primaryReads   int
	primaryIDReads int
}

func (r *laggingReplicaRepo) FindByIDWithRelations(id string) (*model.Note, error) {
	if n, ok := r.stale[id]; ok {
		c := *n
		return &c, nil
	}
	return r.MockNoteRepository.FindByIDWithRelations(id)
}

func (r *laggingReplicaRepo) FindByIDWithRelationsOnPrimary(id string) (*model.Note, error) {
	r.primaryReads++
	return r.MockNoteRepository.FindByIDWithRelations(id)
}

func (r *laggingReplicaRepo) ExistingNoteIDsOnPrimary(ids []string) ([]string, error) {
	r.primaryIDReads++
	return r.MockNoteRepository.ExistingNoteIDsOnPrimary(ids)
}

func (r *recordingPush) PushNotification(_ string, body map[string]any) {
	r.bodies = append(r.bodies, body)
}

// A note deleted after the notification was created is not pushed, and neither
// is a deleted note it embeds: the push summary of a reply whose target was
// deleted carries no "RE:" text from the target (#3611 review M-1').
// Upstream pushes the creation-time note as is; Elythia keeps the result it
// had when the push read the note again.
func TestHook_PushOmitsDeletedNotes(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		deleted func(reply, parent string) string
		want    func(t *testing.T, body map[string]any)
	}{
		{"nothing deleted keeps the reply target", func(string, string) string { return "" }, func(t *testing.T, body map[string]any) {
			assert.Contains(t, summaryOf(t, body), "parent secret", "control: the summary does carry the target")
		}},
		{"deleted reply target, lagging replica", func(_, parent string) string { return parent }, func(t *testing.T, body map[string]any) {
			s := summaryOf(t, body)
			assert.Contains(t, s, "the reply")
			assert.NotContains(t, s, "parent secret")
		}},
		{"deleted note", func(reply, _ string) string { return reply }, func(t *testing.T, body map[string]any) {
			_, hasNote := body["note"]
			assert.False(t, hasNote)
			assert.NotContains(t, mustJSON(t, body), "the reply")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr.FlushAll()
			svc := corenotification.NewService(rc, idGen, "")
			svc.SetUnreadPublishDelay(0)
			users := testutil.NewMockUserRepository()
			for _, u := range []string{"alice", "bob", "carol"} {
				users.Users[u] = &model.User{ID: u, Username: u, UsernameLower: u}
			}
			notes := &laggingReplicaRepo{MockNoteRepository: testutil.NewMockNoteRepository(), stale: map[string]*model.Note{}}
			parent := &model.Note{ID: idGen.Generate(time.Now()), UserID: "carol", Text: text("parent secret"), Visibility: model.NoteVisibilityPublic}
			notes.Notes[parent.ID] = parent
			reply := &model.Note{ID: idGen.Generate(time.Now()), UserID: "bob", Text: text("the reply"), Visibility: model.NoteVisibilityPublic, ReplyID: &parent.ID, Mentions: model.StringArray{"alice"}}
			notes.Notes[reply.ID] = reply

			hook := corenotification.NewHook(svc, users)
			pub := &recordingPush{}
			hook.SetWebPushPublisher(pub)
			hook.SetPackers(webpush.NewUserRepoPacker(users), webpush.NewNoteRepoPacker(notes, idGen, testutil.NewMockFollowingRepository(), nil))
			hook.SetNoteLoader(notes.FindByIDWithRelations)
			hook.SetNoteExistenceChecker(notes)

			// 作成時に読んだノート (返信先つき)。その後、push までの間に消す。
			created, err := notes.FindByIDWithRelations(reply.ID)
			require.NoError(t, err)
			created.User = users.Users["bob"]
			created.Reply.User = users.Users["carol"]
			if d := tc.deleted(reply.ID, parent.ID); d != "" {
				// レプリカは遅れていて、消す前の返信 (返信先つき) をまだ返す。
				stale := *created
				notes.stale[reply.ID] = &stale
				delete(notes.Notes, d)
			}

			hook.OnNoteCreated(created, users.Users["bob"], nil, nil)

			require.Len(t, pub.bodies, 1)
			tc.want(t, webpush.TruncateBody(webpush.TypeNotification, pub.bodies[0]))
			assert.Equal(t, 1, notes.primaryIDReads, "one existence check on the primary")
		})
	}
}

func summaryOf(t *testing.T, body map[string]any) string {
	t.Helper()
	note, ok := body["note"].(map[string]any)
	require.True(t, ok, "note present")
	s, _ := note["text"].(string)
	return s
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}
