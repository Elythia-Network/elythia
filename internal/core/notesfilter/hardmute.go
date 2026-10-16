// Package notesfilter centralises pre-pack filtering steps applied to a
// []*model.Note before serialization. The first user is the per-viewer
// hardMutedWords filter (#787) shared across timeline / search / channel /
// antenna / user-notes / clip / role / drive endpoints, but the package
// is the natural home for similar viewer-scoped filters added later
// (e.g. per-host muting, blocked-user enforcement) so they stay together.
package notesfilter

import (
	"time"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/wordmute"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// ProfileLookup is the minimal subset of repository.UserRepository this
// package needs. Keeping the interface narrow lets handlers wire only the
// methods they actually use (the production UserRepository implementation
// satisfies it automatically) and makes test stubs trivial.
type ProfileLookup interface {
	FindProfileByUserID(userID string) (*model.UserProfile, error)
}

// ApplyHardMute drops notes that match the viewer's persisted
// hardMutedWords (jsonb on user_profile) before they reach PackNotes.
//
// Best-effort semantics: anonymous viewer / lookup not wired / profile
// fetch error / empty rule list all degrade to "return input untouched"
// so a transient failure never hides legitimate timeline traffic. The
// decision follows upstream (wordmute.MatchAppearNote): a pure renote is
// judged by the renoted note, and the reply / quote targets are checked too.
// The notes must be hydrated (Reply / Renote / Renote.Reply / Renote.Renote and
// their authors) like every list the packer reads. idGen decodes note IDs for
// the authors' time-window visibility settings; nil treats every note as
// created at epoch 0 (those notes are then not judged, never wrongly hidden).
func ApplyHardMute(lookup ProfileLookup, viewer *model.User, notes []*model.Note, idGen id.Generator) []*model.Note {
	if viewer == nil || lookup == nil || len(notes) == 0 {
		return notes
	}
	profile, err := lookup.FindProfileByUserID(viewer.ID)
	if err != nil || profile == nil || len(profile.HardMutedWords) == 0 {
		return notes
	}
	rules := []byte(profile.HardMutedWords)
	nowMs := time.Now().UnixMilli()
	out := notes[:0]
	for _, n := range notes {
		if wordmute.MatchAppearNote(rules, viewer.ID, hardMuteTree(viewer, n, idGen, nowMs)) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// hardMuteTree converts a hydrated note into the hard-mute view, marking the
// parts the viewer is not allowed to see.
//
// **見えるかどうかは、パックした後に notehide が隠すものと同じ判定で決める。**
// 本家は hideNote で本文を消した後のノートを判定する。ここはパックの前に
// 落とすので、同じ判定 (top-level は著者の設定だけ、埋め込みは全部) をここで
// 当てる。フォローの集合は引かない (フォロー中の人のフォロワー限定のノートは
// 見えない側として扱い、その中身では隠さない)。見えないノートの中身は判定に
// 使わないので、迷ったら見えない側にする。
//
// top-level に著者の設定の判定だけを当てるのは、ここに来る一覧がどれも、
// 閲覧者が見られる top-level のノートだけを引いているため (SQL の可視性の条件か
// FilterVisible)。見えない top-level を返す経路 (notes/conversation) には
// ハードミュートを掛けない。
func hardMuteTree(viewer *model.User, n *model.Note, idGen id.Generator, nowMs int64) *wordmute.Note {
	if n == nil {
		return nil
	}
	top := hardMuteNode(n)
	topFacts := corenote.FactsFromModel(n, createdAtMs(idGen, n.ID))
	if n.Reply != nil {
		topFacts.ReplyTargetAuthorID = n.Reply.UserID
	}
	top.Hidden = corenote.HideNoteByPrefsDecision(viewer, topFacts, neverFollows, nowMs)
	top.PureRenote = corenote.IsPureRenote(n)
	top.Reply = hardMuteEmbed(viewer, n.Reply, idGen, nowMs)
	top.Renote = hardMuteEmbed(viewer, n.Renote, idGen, nowMs)
	if top.Renote != nil {
		// 埋め込みの 2 段目 (renote.reply / renote.renote) はパックされるものだけ。
		// reply の埋め込みは子を持たない (packer の detail:false)。
		top.Renote.Reply = hardMuteEmbed(viewer, n.Renote.Reply, idGen, nowMs)
		top.Renote.Renote = hardMuteEmbed(viewer, n.Renote.Renote, idGen, nowMs)
	}
	return top
}

func hardMuteEmbed(viewer *model.User, n *model.Note, idGen id.Generator, nowMs int64) *wordmute.Note {
	if n == nil {
		return nil
	}
	node := hardMuteNode(n)
	node.Hidden = corenote.HideEmbedDecision(viewer, corenote.FactsFromModel(n, createdAtMs(idGen, n.ID)), neverFollows, nowMs)
	return node
}

func hardMuteNode(n *model.Note) *wordmute.Note {
	node := &wordmute.Note{UserID: n.UserID}
	if n.Text != nil {
		node.Text = *n.Text
	}
	if n.CW != nil {
		node.CW = *n.CW
	}
	return node
}

func neverFollows(string) bool { return false }

// createdAtMs decodes a note ID's timestamp. 0 (epoch) on failure fails closed
// on the time-window gates, like notehide's parseCreatedAtMs.
func createdAtMs(idGen id.Generator, noteID string) int64 {
	if idGen == nil {
		return 0
	}
	t, err := idGen.ParseTime(noteID)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
