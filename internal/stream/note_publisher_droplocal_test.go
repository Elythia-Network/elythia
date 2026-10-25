package stream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DropLocal drops the last packed note, so the same note is packed again from
// what the lookups return now (#3736).
func TestNotePublisher_DropLocal(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen, nil)
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic}
	username := func(i int) string {
		var body struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		require.NoError(t, json.Unmarshal(pub.payloads[i].(json.RawMessage), &body))
		return body.User.Username
	}

	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "before"})
	np.PublishNote("homeTimeline:u2", n, &model.User{ID: "u1", Username: "after"})
	require.Len(t, pub.payloads, 2)
	require.Equal(t, "before", username(1), "the same note reuses the packed body")

	np.DropLocal()
	np.PublishNote("homeTimeline:u3", n, &model.User{ID: "u1", Username: "after"})
	require.Len(t, pub.payloads, 3)
	assert.Equal(t, "after", username(2), "the note is packed again after DropLocal")
}
