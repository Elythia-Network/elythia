package federation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
)

const (
	sharedFollowerInbox = "https://shared.example/inbox"
	soloFollowerInbox   = "https://solo.example/users/sam/inbox"
)

// directRecipientsFixture registers remote users for the 1:1 paths:
//   - bob (mentioned): shares shared.example's inbox with a follower
//   - sam (mentioned): has no sharedInbox and is himself a follower
//   - carol (reply target): on a host no follower is on
func directRecipientsFixture(t *testing.T, userRepo *testutil.MockUserRepository, noteRepo *testutil.MockNoteRepository, followingRepo *testutil.MockFollowingRepository, authorID string) (bobInbox, carolInbox string) {
	t.Helper()
	followingRepo.RemoteInboxes[authorID] = []string{sharedFollowerInbox, soloFollowerInbox}
	followingRepo.RemoteSharedInboxes[sharedFollowerInbox] = true

	str := func(s string) *string { return &s }
	bobInbox, carolInbox = "https://shared.example/users/bob/inbox", "https://other.example/users/carol/inbox"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: str("shared.example"), Inbox: &bobInbox, SharedInbox: str(sharedFollowerInbox)}
	userRepo.Users["sam"] = &model.User{ID: "sam", Username: "sam", Host: str("solo.example"), Inbox: str(soloFollowerInbox)}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol", Host: str("other.example"), Inbox: &carolInbox, SharedInbox: str("https://other.example/inbox")}
	noteRepo.Notes["carolnote"] = &model.Note{ID: "carolnote", UserID: "carol"}
	return bobInbox, carolInbox
}

// TestNoteDeliveryHook_SkipsDirectCoveredByFollowers pins #4223: a mentioned
// or replied-to user reached through an inbox the follower fan-out already
// enqueued to is not sent a second 1:1 copy, like upstream's DeliverManager.
func TestNoteDeliveryHook_SkipsDirectCoveredByFollowers(t *testing.T) {
	for _, vis := range []model.NoteVisibility{model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers} {
		t.Run(string(vis), func(t *testing.T) {
			hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
			author := makeLocalAuthor(t, userRepo, keypairRepo)
			_, carolInbox := directRecipientsFixture(t, userRepo, noteRepo, followingRepo, author.ID)

			note := makeNote(author.ID, vis)
			reply := "carolnote"
			note.ReplyID = &reply
			note.Mentions = model.StringArray{"bob", "sam"}
			hook.OnNoteCreated(note, author)

			// bob は shared inbox で、sam は自分の inbox がフォロワーの配送先に
			// あるので届く。carol のホストへはフォロワーの配送が無いので 1:1 で送る。
			assert.ElementsMatch(t, []string{sharedFollowerInbox, soloFollowerInbox, carolInbox}, enqueuedInboxes(enq))
		})
	}
}

// TestNoteDeliveryHook_DirectNoteStillDeliversToEachRecipient pins that a
// specified note, which has no follower fan-out, still goes to each
// recipient's own inbox.
func TestNoteDeliveryHook_DirectNoteStillDeliversToEachRecipient(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	bobInbox, carolInbox := directRecipientsFixture(t, userRepo, noteRepo, followingRepo, author.ID)

	note := makeNote(author.ID, model.NoteVisibilitySpecified)
	reply := "carolnote"
	note.ReplyID = &reply
	note.VisibleUserIDs = model.StringArray{"bob", "carol"}
	note.Mentions = model.StringArray{"bob", "sam"}
	hook.OnNoteCreated(note, author)

	assert.ElementsMatch(t, []string{bobInbox, carolInbox, soloFollowerInbox}, enqueuedInboxes(enq))
}

// TestNoteDeliveryHook_SendNoteUpdateSkipsDirectCoveredByFollowers pins that
// the re-delivered Update goes to the same recipients as the Create.
func TestNoteDeliveryHook_SendNoteUpdateSkipsDirectCoveredByFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	_, carolInbox := directRecipientsFixture(t, userRepo, noteRepo, followingRepo, author.ID)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text, reply := "hi", "carolnote"
	note.Text, note.ReplyID = &text, &reply
	note.Mentions = model.StringArray{"bob", "sam"}
	require.NoError(t, hook.SendNoteUpdate(note, author))

	assert.ElementsMatch(t, []string{sharedFollowerInbox, soloFollowerInbox, carolInbox}, enqueuedInboxes(enq))
}

// failInboxEnqueuer fails the deliveries to one inbox.
type failInboxEnqueuer struct {
	stubEnqueuer
	fail string
}

func (f *failInboxEnqueuer) EnqueueDeliver(payload queue.DeliverPayload, opts ...driver.EnqueueOption) error {
	if payload.Inbox == f.fail {
		return errors.New("enqueue failed")
	}
	return f.stubEnqueuer.EnqueueDeliver(payload, opts...)
}

// TestNoteDeliveryHook_FollowerFailureKeepsDirect pins that when the follower
// fan-out fails, the 1:1 deliveries are not skipped (the shared inbox may
// never have been enqueued).
func TestNoteDeliveryHook_FollowerFailureKeepsDirect(t *testing.T) {
	enq := &failInboxEnqueuer{fail: sharedFollowerInbox}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	hook := federation.NewNoteDeliveryHook(federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls, nil),
		activitypub.NewRenderer(urls), urls, idGen, userRepo, noteRepo, nil)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	bobInbox, _ := directRecipientsFixture(t, userRepo, noteRepo, followingRepo, author.ID)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.Mentions = model.StringArray{"bob"}
	hook.OnNoteCreated(note, author)

	assert.Contains(t, enqueuedInboxes(&enq.stubEnqueuer), bobInbox)
}

// TestDeliverService_DeliverToFollowersCollect pins the returned set: the
// inboxes enqueued to, limited by allow, and nil on error.
func TestDeliverService_DeliverToFollowersCollect(t *testing.T) {
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	svc := federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, activitypub.NewURLBuilder("https://example.com"), nil)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{sharedFollowerInbox, soloFollowerInbox}

	sent, err := svc.DeliverToFollowersCollect(author.ID, []byte(`{}`), nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{sharedFollowerInbox: true, soloFollowerInbox: true}, sent)

	enq.calls = nil
	sent, err = svc.DeliverToFollowersCollect(author.ID, []byte(`{}`), func(host string) bool { return host == "solo.example" })
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{soloFollowerInbox: true}, sent)
	assert.Equal(t, []string{soloFollowerInbox}, enqueuedInboxes(enq))

	enq.err = errors.New("down")
	sent, err = svc.DeliverToFollowersCollect(author.ID, []byte(`{}`), nil)
	require.Error(t, err)
	assert.Nil(t, sent)
}

// TestNoteDeliveryHook_BlockedFollowerInboxDoesNotCoverDirect pins that an
// inbox the follower fan-out dropped (blocked host) does not count as
// delivered: the mentioned user, whose own inbox is on an allowed host, still
// gets the 1:1 copy.
func TestNoteDeliveryHook_BlockedFollowerInboxDoesNotCoverDirect(t *testing.T) {
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	blocker := &stubHostBlocker{blocked: map[string]bool{"shared.example": true}}
	hook := federation.NewNoteDeliveryHook(federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls, blocker),
		activitypub.NewRenderer(urls), urls, idGen, userRepo, noteRepo, nil)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	directRecipientsFixture(t, userRepo, noteRepo, followingRepo, author.ID)
	// bob の個別 inbox は、ブロックしていない別のホストにある。
	bobInbox := "https://bob-home.example/users/bob/inbox"
	userRepo.Users["bob"].Inbox = &bobInbox

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.Mentions = model.StringArray{"bob"}
	hook.OnNoteCreated(note, author)

	assert.ElementsMatch(t, []string{soloFollowerInbox, bobInbox}, enqueuedInboxes(enq))
}

// TestDeliverService_FollowerSharedFlagFromAnyRow pins that an inbox URL
// listed both as one follower's sharedInbox and as another's own inbox is
// marked shared whatever the row order, so a 410 still reaches the host-wide
// gone check (#1811).
func TestDeliverService_FollowerSharedFlagFromAnyRow(t *testing.T) {
	for _, sharedFirst := range []bool{true, false} {
		enq := &stubEnqueuer{}
		userRepo := testutil.NewMockUserRepository()
		keypairRepo := testutil.NewMockUserKeypairRepository()
		author := makeLocalAuthor(t, userRepo, keypairRepo)
		rows := []model.RemoteInbox{{Inbox: sharedFollowerInbox, Shared: false}, {Inbox: sharedFollowerInbox, Shared: true}}
		if sharedFirst {
			rows[0], rows[1] = rows[1], rows[0]
		}
		followingRepo := &rowsFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository(), rows: rows}
		svc := federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, activitypub.NewURLBuilder("https://example.com"), nil)

		require.NoError(t, svc.DeliverToFollowersExcluding(author.ID, []byte(`{}`), nil))
		require.Len(t, enq.calls, 1)
		assert.True(t, enq.calls[0].IsSharedInbox, "shared first: %v", sharedFirst)
	}
}

// rowsFollowingRepo returns fixed follower inbox rows.
type rowsFollowingRepo struct {
	*testutil.MockFollowingRepository
	rows []model.RemoteInbox
}

func (r *rowsFollowingRepo) ListRemoteFollowerInboxes(string) ([]model.RemoteInbox, error) {
	return r.rows, nil
}
