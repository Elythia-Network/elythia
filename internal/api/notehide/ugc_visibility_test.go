package notehide

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/ugcvisibility"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// policyHider returns a Hider whose visitor policy is fixed to policy and
// which has no follow repository.
func policyHider(policy string) *Hider {
	return NewHider(nil, func() string { return policy })
}

func publicNote(id, author string) *entity.NoteEntity {
	return &entity.NoteEntity{
		ID: id, UserID: author, CreatedAt: "2026-01-02T03:04:05.000Z",
		User: entity.UserLite{ID: author}, Visibility: "public",
		Text: heStr("text " + id), FileIDs: []string{"f-" + id}, Files: []any{"file"},
		VisibleUserIDs: []string{}, Mentions: []string{},
	}
}

// fullyEmbeddedBatch returns a public top-level note whose renote carries a
// depth-2 quote target and reply, plus a top-level reply: every level the
// packer emits.
func fullyEmbeddedBatch() []entity.NoteEntity {
	renote := publicNote("rn", "a2")
	renote.Renote = publicNote("rnrn", "a3")
	renote.Reply = publicNote("rnrp", "a4")
	top := *publicNote("top", "a1")
	top.Renote = renote
	top.Reply = publicNote("rp", "a5")
	return []entity.NoteEntity{top}
}

func levels(n *entity.NoteEntity) map[string]*entity.NoteEntity {
	return map[string]*entity.NoteEntity{
		"top":           n,
		"renote":        n.Renote,
		"reply":         n.Reply,
		"renote.renote": n.Renote.Renote,
		"renote.reply":  n.Renote.Reply,
	}
}

func TestUGCVisibility_PackedNotesForVisitor(t *testing.T) {
	signedIn := &model.User{ID: "viewer"}
	cases := []struct {
		name   string
		policy string
		viewer *model.User
		hidden bool
	}{
		{name: "visitor under none is hidden", policy: ugcvisibility.None, hidden: true},
		{name: "visitor under local is not hidden", policy: ugcvisibility.Local},
		{name: "visitor under all is not hidden", policy: ugcvisibility.All},
		{name: "signed-in viewer under none is not hidden", policy: ugcvisibility.None, viewer: signedIn},
	}
	hiders := map[string]func(*Hider, *model.User, []entity.NoteEntity){
		"HideEmbeds":      (*Hider).HideEmbeds,
		"HideStoredNotes": (*Hider).HideStoredNotes,
	}
	for _, tc := range cases {
		for hname, hide := range hiders {
			t.Run(hname+"/"+tc.name, func(t *testing.T) {
				packed := fullyEmbeddedBatch()
				hide(policyHider(tc.policy), tc.viewer, packed)
				for level, n := range levels(&packed[0]) {
					if n.IsHidden != tc.hidden {
						t.Errorf("%s: IsHidden = %v, want %v", level, n.IsHidden, tc.hidden)
					}
					if tc.hidden && (n.Text != nil || len(n.Files) != 0) {
						t.Errorf("%s: content must be blanked", level)
					}
					if !tc.hidden && n.Text == nil {
						t.Errorf("%s: content must be kept", level)
					}
				}
			})
		}
	}
}

func TestUGCVisibility_NotificationNotesUseSameGate(t *testing.T) {
	// 通知は常にログイン済みの受信者向けなので、'none' でも隠さない。
	note := *publicNote("n", "a1")
	out := []map[string]any{notifNoteMap(note)}
	policyHider(ugcvisibility.None).HideNotificationNotes(&model.User{ID: "viewer"}, out)
	if out[0]["note"].(entity.NoteEntity).IsHidden {
		t.Error("notification note for a signed-in recipient must not be hidden")
	}
}

// 未配線 (nil の *Hider と、lookup が nil の Hider) は公開範囲を見ない。
// 本番で渡し忘れないよう、使う部品は criticalWiring で起動時に検査する。
func TestUGCVisibility_UnwiredLookupDoesNotHide(t *testing.T) {
	for name, h := range map[string]*Hider{"nil hider": nil, "nil lookup": NewHider(nil, nil)} {
		packed := fullyEmbeddedBatch()
		h.HideEmbeds(nil, packed)
		if packed[0].IsHidden {
			t.Errorf("%s: an unwired lookup must not hide public notes", name)
		}
	}
}
