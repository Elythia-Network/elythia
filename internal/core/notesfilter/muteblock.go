package notesfilter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/gorm"
)

// MuteBlockSets holds the viewer-scoped exclusion sets used by
// ApplyMuteBlockChannel. They are resolved up front (one query each) and then
// applied in-memory to a fetched []*model.Note.
//
// 各 set は upstream Misskey TS の QueryService.generateBaseNoteFilteringQuery
// のうち muted-user / blocked-user / muted-instances の 3 次元 +
// ChannelMutingService の channel-mute に対応する (#1544 / #1630):
//   - MutedUserIDs:    viewer が mute した user (active) — note/reply/renote
//     のいずれかの author なら除外 (generateMutedUserQueryForNotes 相当)
//   - BlockerIDs:      viewer を block している user — note/reply/renote の
//     いずれかの author なら除外 (generateBlockedUserQueryForNotes 相当、
//     「被 block」= blockeeId が viewer の行の blockerId 集合)
//   - MutedChannelIDs: viewer が mute した channel — note.channelId /
//     note.renoteChannelId が一致なら除外
//   - MutedInstances:  viewer の user_profile.mutedInstances に含まれる host —
//     note/reply/renote の author host が一致なら除外 (#1630、upstream の
//     mutingInstanceQuery brackets 相当。jsonb `?` = exact host 一致で
//     subdomain 展開はしない)
//
// upstream が併せ持つ blocked-host (admin の meta.blockedHosts) は
// ApplyBlockedHosts、suspended-user は未対応の follow-up。
type MuteBlockSets struct {
	MutedUserIDs    map[string]struct{}
	BlockerIDs      map[string]struct{}
	MutedChannelIDs map[string]struct{}
	MutedInstances  map[string]struct{}
}

// empty reports whether every dimension is a no-op.
func (s MuteBlockSets) empty() bool {
	return len(s.MutedUserIDs) == 0 && len(s.BlockerIDs) == 0 &&
		len(s.MutedChannelIDs) == 0 && len(s.MutedInstances) == 0
}

// RenoteLookup is the minimal note-repository subset used to resolve renote
// target rows for the nested mute/block check (#1630)。
// repository.NoteRepository がそのまま満たす。
type RenoteLookup interface {
	FindManyByIDsWithUser(ids []string) ([]*model.Note, error)
}

// LoadMuteBlockSets resolves the viewer's muted-user / blocker /
// muted-channel / muted-instances sets for use by ApplyMuteBlockChannel.
//
// Fail-closed: any repository error is returned to the caller so the endpoint
// can surface a 500 rather than silently serving notes that should have been
// filtered (security item, #1544). A nil viewer or a nil repo yields an empty
// (no-op) set for that dimension — anonymous viewers can't mute/block and an
// unwired repo means the feature is disabled in that deployment.
func LoadMuteBlockSets(
	viewer *model.User,
	mutingRepo repository.MutingRepository,
	blockingRepo repository.BlockingRepository,
	channelMutingRepo repository.ChannelMutingRepository,
	profiles ProfileLookup,
) (MuteBlockSets, error) {
	sets := MuteBlockSets{}
	if viewer == nil {
		return sets, nil
	}

	if mutingRepo != nil {
		ids, err := mutingRepo.ListMuteeIDs(viewer.ID)
		if err != nil {
			return MuteBlockSets{}, fmt.Errorf("load muted users: %w", err)
		}
		sets.MutedUserIDs = toSet(ids)
	}

	if blockingRepo != nil {
		ids, err := blockingRepo.ListBlockerIDs(viewer.ID)
		if err != nil {
			return MuteBlockSets{}, fmt.Errorf("load blockers: %w", err)
		}
		sets.BlockerIDs = toSet(ids)
	}

	if channelMutingRepo != nil {
		rows, err := channelMutingRepo.ListByUser(viewer.ID)
		if err != nil {
			return MuteBlockSets{}, fmt.Errorf("load muted channels: %w", err)
		}
		if len(rows) > 0 {
			m := make(map[string]struct{}, len(rows))
			for _, row := range rows {
				m[row.ChannelID] = struct{}{}
			}
			sets.MutedChannelIDs = m
		}
	}

	if profiles != nil {
		instances, err := loadMutedInstances(profiles, viewer.ID)
		if err != nil {
			return MuteBlockSets{}, err
		}
		sets.MutedInstances = instances
	}

	return sets, nil
}

// loadMutedInstances reads user_profile.mutedInstances (jsonb の host 配列)
// into a lower-cased set (#1630)。profile 行が無いのは正常 (空 set)、それ
// 以外の repo error / jsonb 破損は fail-closed で返す。
func loadMutedInstances(profiles ProfileLookup, viewerID string) (map[string]struct{}, error) {
	profile, err := profiles.FindProfileByUserID(viewerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("load muted instances: %w", err)
	}
	if profile == nil || len(profile.MutedInstances) == 0 {
		return nil, nil
	}
	var hosts []string
	if err := json.Unmarshal(profile.MutedInstances, &hosts); err != nil {
		return nil, fmt.Errorf("load muted instances: parse: %w", err)
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	m := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		if h == "" {
			continue
		}
		// upstream の jsonb `?` は exact 比較だが、host は正規化済み小文字
		// 格納が前提のため lower 化して case-insensitive に倒す (安全側、
		// ApplyBlockedHosts と同方針)。
		m[strings.ToLower(h)] = struct{}{}
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// ApplyMuteBlockChannel drops notes the viewer should not see because of a
// mute, a block, a channel mute, or an instance mute, mirroring upstream
// antennas/notes and roles/notes which call generateBaseNoteFilteringQuery +
// the muted-channel brackets after fetching the note ID set.
//
// A note is dropped when:
//   - its author, reply author, or renote author is in MutedUserIDs, OR
//   - its author, reply author, or renote author is in BlockerIDs
//     (= that user has blocked the viewer), OR
//   - its author / reply author / renote author's host is in MutedInstances, OR
//   - its channelId or renoteChannelId is in MutedChannelIDs, OR
//   - its renote target's reply/renote author (or their host) hits the
//     muted-user / blocked-user / muted-instances sets (#1630、upstream の
//     noteColumn:'renote' 変種。renotes lookup 非 nil のとき renote 先行を
//     batch fetch して検査する。channel mute は upstream の renote 変種に
//     含まれないため renote 先には適用しない)
//
// renotes が nil の場合は入れ子検査を skip する (未配線 degrade)。renote 先
// 行が見つからない note は upstream の leftJoin null 相当で通す。fetch error
// は fail-closed で返す。
//
// The viewer's own posts are NOT exempted here: upstream applies the same
// query regardless, and a viewer can't mute/block themselves so self-posts
// pass through naturally. An all-empty MuteBlockSets is a no-op.
func ApplyMuteBlockChannel(notes []*model.Note, sets MuteBlockSets, renotes RenoteLookup) ([]*model.Note, error) {
	if len(notes) == 0 || sets.empty() {
		return notes, nil
	}

	// renote 入れ子検査用に renote 先行を 1 query で引く (#1630)。muted-user /
	// blocked-user / muted-instances のいずれかが非空のときだけ必要になる
	// (channel mute 単独では入れ子検査対象が無い)。
	var renoteRefs map[string]*model.Note
	if renotes != nil &&
		(len(sets.MutedUserIDs) > 0 || len(sets.BlockerIDs) > 0 || len(sets.MutedInstances) > 0) {
		ids := make([]string, 0)
		seen := make(map[string]struct{})
		for _, n := range notes {
			if n == nil || n.RenoteID == nil || *n.RenoteID == "" {
				continue
			}
			if _, dup := seen[*n.RenoteID]; dup {
				continue
			}
			seen[*n.RenoteID] = struct{}{}
			ids = append(ids, *n.RenoteID)
		}
		if len(ids) > 0 {
			rows, err := renotes.FindManyByIDsWithUser(ids)
			if err != nil {
				return nil, fmt.Errorf("load renote refs: %w", err)
			}
			renoteRefs = make(map[string]*model.Note, len(rows))
			for _, r := range rows {
				if r != nil {
					renoteRefs[r.ID] = r
				}
			}
		}
	}

	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if n == nil {
			continue
		}
		if userExcluded(sets.MutedUserIDs, n) || userExcluded(sets.BlockerIDs, n) {
			continue
		}
		if instanceExcluded(sets.MutedInstances, n) {
			continue
		}
		if channelExcluded(sets.MutedChannelIDs, n) {
			continue
		}
		if n.RenoteID != nil && renoteRefs != nil {
			if ref := renoteRefs[*n.RenoteID]; ref != nil {
				if userExcluded(sets.MutedUserIDs, ref) || userExcluded(sets.BlockerIDs, ref) ||
					instanceExcluded(sets.MutedInstances, ref) {
					continue
				}
			}
		}
		out = append(out, n)
	}
	return out, nil
}

// userExcluded reports whether the note's author, reply author, or renote
// author is in the given exclusion set. Mirrors the note/reply/renote
// userId NOT IN (...) brackets in generateMutedUserQueryForNotes /
// generateBlockedUserQueryForNotes.
func userExcluded(set map[string]struct{}, n *model.Note) bool {
	if len(set) == 0 {
		return false
	}
	if _, hit := set[n.UserID]; hit {
		return true
	}
	if n.ReplyUserID != nil {
		if _, hit := set[*n.ReplyUserID]; hit {
			return true
		}
	}
	if n.RenoteUserID != nil {
		if _, hit := set[*n.RenoteUserID]; hit {
			return true
		}
	}
	return false
}

// instanceExcluded reports whether the note's author, reply author, or renote
// author belongs to a viewer-muted instance (#1630)。host が nil (= local) は
// upstream の `userHost IS NULL` bracket と同じく常に通す。
func instanceExcluded(set map[string]struct{}, n *model.Note) bool {
	if len(set) == 0 {
		return false
	}
	for _, host := range []*string{n.UserHost, n.ReplyUserHost, n.RenoteUserHost} {
		if host == nil || *host == "" {
			continue
		}
		if _, hit := set[strings.ToLower(*host)]; hit {
			return true
		}
	}
	return false
}

// channelExcluded reports whether the note's channelId or renoteChannelId is
// muted. Mirrors the two muted-channel brackets in upstream notes.ts.
func channelExcluded(set map[string]struct{}, n *model.Note) bool {
	if len(set) == 0 {
		return false
	}
	if n.ChannelID != nil {
		if _, hit := set[*n.ChannelID]; hit {
			return true
		}
	}
	if n.RenoteChannelID != nil {
		if _, hit := set[*n.RenoteChannelID]; hit {
			return true
		}
	}
	return false
}

func toSet(ids []string) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}
