package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/stream"
)

// muteBlockUserProbe carries just the author host needed for the instance-mute
// check. A nil/absent host (= local user) maps to "" which a mutedInstances set
// never contains, so local notes are never instance-muted (upstream
// `note.user?.host ?? ”`).
type muteBlockUserProbe struct {
	ID   string  `json:"id"`
	Host *string `json:"host"`
}

// muteBlockEmbed is the depth-1 renote/reply metadata needed for isUserRelated /
// isChannelRelated / isInstanceMuted.
type muteBlockEmbed struct {
	UserID    string             `json:"userId"`
	ChannelID *string            `json:"channelId"`
	User      muteBlockUserProbe `json:"user"`
}

// muteBlockProbe decodes the minimal packed-note fields the main-channel
// mute/block gate needs. pure-renote detection mirrors upstream isQuotePacked
// (text/cw/replyId/poll/fileIds は quote の指標)。
type muteBlockProbe struct {
	UserID    string             `json:"userId"`
	ChannelID *string            `json:"channelId"`
	User      muteBlockUserProbe `json:"user"`
	RenoteID  *string            `json:"renoteId"`
	ReplyID   *string            `json:"replyId"`
	Text      *string            `json:"text"`
	CW        *string            `json:"cw"`
	Poll      json.RawMessage    `json:"poll"`
	FileIDs   []string           `json:"fileIds"`
	Reply     *muteBlockEmbed    `json:"reply"`
	Renote    *muteBlockEmbed    `json:"renote"`
}

// hostMuted reports whether host (nil → "") is in the muted-instance set.
func hostMuted(host *string, muted map[string]struct{}) bool {
	h := ""
	if host != nil {
		h = *host
	}
	_, ok := muted[h]
	return ok
}

// noteMutedOrBlocked ports upstream `Connection#isNoteMutedOrBlocked` +
// `isInstanceMuted` for a streamed packed-note payload (#1711). Returns true
// when the note (or its renote/reply target) involves a muted instance, a muted
// user, a user who blocks the viewer, a renote-muted author (pure renote only),
// or a muted channel. nil snapshot (anonymous / unwired) は fail-open で false。
func noteMutedOrBlocked(payload []byte, snap *stream.MuteBlockSnapshot) bool {
	if snap == nil {
		return false
	}
	var p muteBlockProbe
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	if mutedOrBlockedExceptChannel(p, snap) {
		return true
	}
	// channel-mute: note の所属 channel か、renote 先の channel が muted。
	return channelRelated(p, snap.MutingChannels)
}

// noteMutedOrBlockedInChannel mirrors upstream channel.ts's `override
// isNoteMutedOrBlocked` for the "channel" timeline (#1812). It shares the
// instance-mute / user-mute / block / renote-mute gate with the base, but for
// channel-mute it deliberately ignores the note's own channel: directly viewing
// a channel must keep streaming its notes even when that channel is muted. Only
// a renote whose target channel differs from the viewed channel and is muted is
// dropped (upstream: 「このソケットで見ているチャンネルがミュートされていたとしても、
// チャンネルを直接見ている以上は流すようにしたい。ただし、他のミュートしている
// チャンネルは流さないようにもしたい」). viewingChannelID is the channel this
// socket subscribed to.
func noteMutedOrBlockedInChannel(payload []byte, snap *stream.MuteBlockSnapshot, viewingChannelID string) bool {
	if snap == nil {
		return false
	}
	var p muteBlockProbe
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	if mutedOrBlockedExceptChannel(p, snap) {
		return true
	}
	// channel-mute (override): own-channel は無視し、renote 先 channel が
	// 視聴中 channel と異なり muted の場合のみ drop。
	if len(snap.MutingChannels) > 0 && p.Renote != nil && p.Renote.ChannelID != nil {
		if rc := *p.Renote.ChannelID; rc != viewingChannelID {
			if _, ok := snap.MutingChannels[rc]; ok {
				return true
			}
		}
	}
	return false
}

// mutedOrBlockedExceptChannel applies the instance-mute / user-mute / block /
// renote-mute checks shared by every channel's isNoteMutedOrBlocked, leaving the
// channel-mute branch (which differs between the base and the channel-timeline
// override) to the caller. p must already be unmarshaled.
func mutedOrBlockedExceptChannel(p muteBlockProbe, snap *stream.MuteBlockSnapshot) bool {
	// instance-mute: note / reply / renote の author host のいずれか。
	if len(snap.MutedInstances) > 0 {
		if hostMuted(p.User.Host, snap.MutedInstances) {
			return true
		}
		if p.Reply != nil && hostMuted(p.Reply.User.Host, snap.MutedInstances) {
			return true
		}
		if p.Renote != nil && hostMuted(p.Renote.User.Host, snap.MutedInstances) {
			return true
		}
	}
	// user-mute (userIdsWhoMeMuting) + block (userIdsWhoBlockingMe): いずれも
	// isUserRelated (author / reply author / renote author) で判定する。
	if userRelated(p, snap.Muting) {
		return true
	}
	if userRelated(p, snap.BlockingMe) {
		return true
	}
	// renote-mute: pure renote (= renote かつ quote でない) を行った user の
	// renote を mute している場合に drop。
	if len(snap.RenoteMuting) > 0 && isPureRenotePayload(p) {
		if _, ok := snap.RenoteMuting[p.UserID]; ok {
			return true
		}
	}
	return false
}

// userRelated ports upstream `isUserRelated`: the note's author, reply target
// author, or renote target author is in the set. reply/renote の author は
// note.userId と異なる場合のみ評価する (upstream と一致)。
func userRelated(p muteBlockProbe, set map[string]struct{}) bool {
	if len(set) == 0 {
		return false
	}
	if _, ok := set[p.UserID]; ok {
		return true
	}
	if p.Reply != nil && p.Reply.UserID != "" && p.Reply.UserID != p.UserID {
		if _, ok := set[p.Reply.UserID]; ok {
			return true
		}
	}
	if p.Renote != nil && p.Renote.UserID != "" && p.Renote.UserID != p.UserID {
		if _, ok := set[p.Renote.UserID]; ok {
			return true
		}
	}
	return false
}

// channelRelated ports upstream `isChannelRelated`: the note's own channel, or
// the renote target's channel (when different), is in the set. reply の channel
// は upstream 同様チェックしない。
func channelRelated(p muteBlockProbe, set map[string]struct{}) bool {
	if len(set) == 0 {
		return false
	}
	if p.ChannelID != nil {
		if _, ok := set[*p.ChannelID]; ok {
			return true
		}
	}
	if p.Renote != nil && p.Renote.ChannelID != nil {
		if p.ChannelID == nil || *p.Renote.ChannelID != *p.ChannelID {
			if _, ok := set[*p.Renote.ChannelID]; ok {
				return true
			}
		}
	}
	return false
}

// isPureRenotePayload ports upstream `isRenotePacked && !isQuotePacked`: a note
// with renoteId set and none of the quote indicators (text/cw/replyId/poll/
// fileIds).
func isPureRenotePayload(p muteBlockProbe) bool {
	if p.RenoteID == nil {
		return false
	}
	if p.Text != nil || p.CW != nil || p.ReplyID != nil {
		return false
	}
	if len(p.Poll) > 0 && string(p.Poll) != "null" {
		return false
	}
	if len(p.FileIDs) > 0 {
		return false
	}
	return true
}

// notificationFromMutedInstance ports upstream `isUserFromMutedInstance` for a
// streamed bare-notification payload (notifications: topic): drop when the
// notifier's host is in the viewer's muted-instance set (#1711). user-mute は
// 通知作成時 (notifyLocalUser の IsMuted) で既に適用済みのため、ここでは
// instance-mute のみを評価する (二重適用を避ける)。
func notificationFromMutedInstance(payload []byte, snap *stream.MuteBlockSnapshot) bool {
	if snap == nil || len(snap.MutedInstances) == 0 {
		return false
	}
	var p struct {
		User *muteBlockUserProbe `json:"user"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.User == nil {
		return false
	}
	return hostMuted(p.User.Host, snap.MutedInstances)
}
