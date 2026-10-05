package federation_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/require"
)

type recordingMover struct{ calls int }

func (m *recordingMover) PostMoveProcess(src, dst *model.User) { m.calls++ }

// movedTo actor doc.
func movedActor(host, name, featured, movedTo string) string {
	base := fmt.Sprintf("https://%s/users/%s", host, name)
	featuredLine := ""
	if featured != "" {
		featuredLine = fmt.Sprintf("\t\"featured\": %q,\n", featured)
	}
	movedLine := ""
	if movedTo != "" {
		movedLine = fmt.Sprintf("\t\"movedTo\": %q,\n", movedTo)
	}
	return fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": "Person",
	"preferredUsername": %q,
	"inbox": %q,
%s%s	"publicKey": {
		"id": %q,
		"owner": %q,
		"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
	}
}`, base, name, base+"/inbox", featuredLine, movedLine, base+"#main-key", base)
}

// REVIEW: processRemoteMove -> ResolveActor(dstURI) drops the chain (nil),
// so updateFeatured for the destination re-enters resolveNoteDepth with the
// singleflight key our own ancestor already holds.
func TestResolveNote_MovedToDestinationFeaturedDoesNotDeadlock(t *testing.T) {
	const (
		srcURI  = "https://remote.example/users/mover"
		dstURI  = "https://remote.example/users/moved"
		dstFeat = "https://remote.example/users/moved/collections/featured"
		pinned  = "https://remote.example/notes/m1"
	)
	env := newFeaturedEnv(t, map[string]string{
		srcURI:  movedActor("remote.example", "mover", "", dstURI),
		dstURI:  featuredActor("remote.example", "moved", dstFeat),
		dstFeat: featuredCollection(dstFeat, "OrderedCollection", pinned),
		pinned:  featuredNote(pinned, srcURI, "note by the moving actor"),
	})
	env.resolver.SetMoveProcessor(&recordingMover{})

	host := "remote.example"
	uri := srcURI
	existing := &model.User{ID: "9moveruser0000000000", Username: "mover", Host: &host, URI: &uri}
	require.NoError(t, env.users.Create(existing))

	done := make(chan error, 1)
	go func() {
		_, err := env.resolver.ResolveNote(pinned)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("DEADLOCK: ResolveNote did not return (movedTo -> ResolveActor(dst) drops the chain)")
	}
}
