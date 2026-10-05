package federation_test

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	federation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPinHook(t *testing.T) (*federation.PinDeliveryHook, *stubEnqueuer, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockUserKeypairRepository, *testutil.MockNoteRepository) {
	t.Helper()
	deliver, enq, userRepo, followingRepo, keypairRepo := newDeliverService(t)
	noteRepo := testutil.NewMockNoteRepository()
	renderer := activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com"))
	hook := federation.NewPinDeliveryHook(deliver, renderer, userRepo, noteRepo)
	return hook, enq, userRepo, followingRepo, keypairRepo, noteRepo
}

// #2024: public/home note の pin は Add(featured)、unpin は Remove を followers へ配信。
func TestPinHook_DeliversAddRemoveForPublicNote(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newPinHook(t)
	installLocalSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/inbox"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}

	hook.OnLocalPinChanged("alice", "n1", true)
	require.Len(t, enq.calls, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Add", got["type"])
	assert.Equal(t, "https://example.com/users/alice/collections/featured", got["target"])
	assert.Equal(t, "https://example.com/notes/n1", got["object"])

	enq.calls = nil
	hook.OnLocalPinChanged("alice", "n1", false)
	require.Len(t, enq.calls, 1)
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Remove", got["type"], "unpin は Remove (#2024)")
}

// #2024: localOnly / followers-or-specified note は連合露出範囲外なので配信しない。
func TestPinHook_SkipsLocalOnlyAndFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newPinHook(t)
	installLocalSigner(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/inbox"}
	noteRepo.Notes["local"] = &model.Note{ID: "local", UserID: "alice", Visibility: model.NoteVisibilityPublic, LocalOnly: true}
	noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "alice", Visibility: model.NoteVisibilityFollowers}

	hook.OnLocalPinChanged("alice", "local", true)
	hook.OnLocalPinChanged("alice", "fol", true)
	assert.Empty(t, enq.calls, "localOnly / followers note は連合配信しない (#2024)")
}

// #2024: remote user / 不在 note は配信しない。
func TestPinHook_SkipsRemoteAndMissing(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, noteRepo := newPinHook(t)
	installLocalSigner(t, userRepo, keypairRepo)
	host := "remote.example"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &host}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: model.NoteVisibilityPublic}

	hook.OnLocalPinChanged("bob", "n1", true)      // remote user
	hook.OnLocalPinChanged("alice", "ghost", true) // 不在 note
	assert.Empty(t, enq.calls)
}
