package federation

import "github.com/elythia-network/elythia/internal/model"

// deriveVisibility maps an AS to/cc audience pair addressed by actor to a
// Misskey visibility, mirroring upstream ApAudienceService.parseAudience
// (#1864):
//
//   - to に Public があれば public
//   - cc に Public があれば home
//   - to / cc のいずれかに actor 自身の followers collection があれば followers
//   - それ以外 (specific actor 列挙) は specified
//
// Public は upstream isPublic と同じく full IRI / as:Public / 裸 Public の 3 形式を
// 受ける。followers collection は upstream isFollowers と同じく actor の
// followersUri (無ければ `uri + '/followers'`) との完全一致で判定する。以前は
// `/followers` で終わる URI を何でも followers 扱いにしていたので、他人の
// followers collection に宛てた note が本家では specified なのに followers に
// なっていた (#3330)。
func deriveVisibility(actor *model.User, to, cc []string) model.NoteVisibility {
	if hasPublicAudience(to) {
		return model.NoteVisibilityPublic
	}
	if hasPublicAudience(cc) {
		return model.NoteVisibilityHome
	}
	followers := authorFollowersURI(actor)
	if followers != "" {
		for _, list := range [][]string{to, cc} {
			for _, id := range list {
				if id == followers {
					return model.NoteVisibilityFollowers
				}
			}
		}
	}
	return model.NoteVisibilitySpecified
}

// authorFollowersURI returns the followers collection upstream
// ApAudienceService.isFollowers compares against: the actor's followersUri,
// or its uri + "/followers" when that is unknown. It returns "" for a nil
// actor or one with neither.
func authorFollowersURI(actor *model.User) string {
	switch {
	case actor == nil:
		return ""
	case actor.FollowersURI != nil && *actor.FollowersURI != "":
		return *actor.FollowersURI
	case actor.URI != nil && *actor.URI != "":
		return *actor.URI + "/followers"
	}
	return ""
}
