package server

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/api/users"
	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/core/event"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/stream"
)

// selfCheckAdapter runs the federation / dependency self-checks for the
// admin endpoint (#2463).
//
// **検査の宛先は config の `url` に固定する。** 自ホストは loopback / private IP
// に解決されうるので検査用 client は SSRF ガードを通さない。宛先を外から
// 与えられる口を作らないことが、その安全性の前提になっている。
type selfCheckAdapter struct {
	checker *selfcheck.Checker
	deps    selfcheck.LocalDeps
}

func (a *selfCheckAdapter) RunSelfCheck(ctx context.Context) selfcheck.Report {
	return selfcheck.Run(ctx, a.checker, a.deps)
}

// notifReaderAdapter bridges stream.NotificationReader to
// core/notification.Service.MarkAllAsRead.
type notifReaderAdapter struct {
	svc *corenotification.Service
}

func (a *notifReaderAdapter) ReadAll(userID string) error {
	// WebSocket の readNotification は upstream Connection.onReadNotification と
	// 同じく暗黙既読なので force を立てない (#2831)。
	return a.svc.MarkAllAsRead(context.Background(), userID, false)
}

// hardMuteLookupAdapter bridges UserRepository to stream.HardMuteRulesLookup
// so the streaming Manager can attach the persisted hardMutedWords (#787) at
// connection setup. Returns nil on lookup failure / empty rule set so the
// streaming filter degrades to no-op rather than dropping the connection.
// lastActiveRecorderAdapter bumps user.lastActiveDate for a live WebSocket
// connection (upstream StreamingApiServerService の updateLastActiveDate 相当)。
// 書き込みは goroutine に逃がして接続処理を待たせない。
type lastActiveRecorderAdapter struct {
	userRepo repository.UserRepository
	// background は更新の goroutine を起動する世代の箱 (#3661)。接続の close
	// handler からも呼ばれるので、世代の停止で待たないと閉じた DB へ書きに行く。
	background *bgwork.Group
}

func (a *lastActiveRecorderAdapter) RecordActive(userID string) {
	if a.userRepo == nil || userID == "" {
		return
	}
	a.background.Go(func(context.Context) {
		if err := a.userRepo.UpdateUser(userID, map[string]any{"lastActiveDate": time.Now()}); err != nil {
			slog.Debug("streaming: lastActiveDate update failed", "userId", userID, "err", err)
		}
	})
}

// sensitiveChannelLookup answers "is this channel sensitive?" for antenna
// filtering. note は channel を埋めて持たないので channelId から都度引く。
type sensitiveChannelLookup struct {
	repo repository.ChannelRepository
}

func (l *sensitiveChannelLookup) IsSensitiveChannel(channelID string) bool {
	if l.repo == nil || channelID == "" {
		return false
	}
	ch, err := l.repo.FindByID(channelID)
	if err != nil || ch == nil {
		return false
	}
	return ch.IsSensitive
}

type hardMuteLookupAdapter struct {
	userRepo repository.UserRepository
}

func (a *hardMuteLookupAdapter) HardMutedWordsForUser(userID string) []byte {
	if a.userRepo == nil || userID == "" {
		return nil
	}
	profile, err := a.userRepo.FindProfileByUserID(userID)
	if err != nil || profile == nil || len(profile.HardMutedWords) == 0 {
		return nil
	}
	return []byte(profile.HardMutedWords)
}

// followingSnapshotAdapter bridges FollowingRepository to
// stream.FollowingSnapshotLookup so the streaming Manager can snapshot the
// viewer's followee list at connection setup (#1063). 戻り値は followeeID →
// withReplies の map で、home/hybrid/local timeline の reply gate に使う。
//
// 依存: `repository.FollowingRepository.ListFollowing(userID, limit, offset)`
// (`internal/repository/following.go`)。既存 method なので追加配線は不要。
//
// fetch は ListFollowing をページネーションで全件読みだす。フォロー数 N が
// 巨大な場合 (10K+) のメモリ / DB 負荷はあるが、upstream Misskey TS も同等
// な map を connection ごとに保持しているので drop-in 互換性回復のためには
// 必要なコスト。pageSize は upstream の channel-following-service と同じく
// 200 / 上限は 10000 で cap し、それ以上 follow している power user は
// snapshot から外れて escape hatch のみ動作する degrade に倒す (= 旧来の
// "全 reply drop" よりは upstream 寄り)。
//
// 改善案 (本 PR scope outside): 同一 user の WebSocket 再接続ごとに毎回
// 全 followee を fetch するので、tab 多重・短期再接続の多い production
// では DB 負荷になる。per-user LRU cache を Manager に持たせれば
// connection accept のコストを下げられる。invalidation は Following 変更
// の subscriber (#791 と同じパターン) で粒度を合わせる想定。
type followingSnapshotAdapter struct {
	repo repository.FollowingRepository
}

func (a *followingSnapshotAdapter) FollowingSnapshotForUser(userID string) map[string]bool {
	if a.repo == nil || userID == "" {
		return nil
	}
	const pageSize = 200
	const maxEntries = 10000
	snap := make(map[string]bool)
	offset := 0
	for {
		rows, err := a.repo.ListFollowing(userID, pageSize, offset)
		if err != nil {
			return snap
		}
		if len(rows) == 0 {
			return snap
		}
		for _, r := range rows {
			snap[r.FolloweeID] = r.WithReplies
			if len(snap) >= maxEntries {
				return snap
			}
		}
		if len(rows) < pageSize {
			return snap
		}
		offset += pageSize
	}
}

// muteBlockSnapshotAdapter bridges the muting / blocking / renote-muting /
// channel-muting repositories + user profile to stream.MuteBlockSnapshotLookup
// so the streaming Manager can snapshot the viewer's mute/block relationships
// at connection setup (#1711). The main / notifications channels consult it to
// drop notifications / mentions involving muted instances, muted users, users
// blocking the viewer, renote-muted authors, or muted channels.
//
// 各 set fetch は best-effort: 個別の repo error は空 set に degrade させ、
// snapshot 全体は常に non-nil で返す (= 取得できた範囲だけ filter する)。
// followingSnapshotAdapter と同じく接続ごとに 1 回読みだす connection-time
// snapshot だが、変更時は SubscribeRelationReload (上の配線を参照) が
// 同じ lookup を呼び直して接続中の snapshot を差し替える (#2400)。
type muteBlockSnapshotAdapter struct {
	muting        repository.MutingRepository
	blocking      repository.BlockingRepository
	renoteMuting  repository.RenoteMutingRepository
	channelMuting repository.ChannelMutingRepository
	userRepo      repository.UserRepository
}

func (a *muteBlockSnapshotAdapter) MuteBlockSnapshotForUser(userID string) *stream.MuteBlockSnapshot {
	if userID == "" {
		return nil
	}
	snap := &stream.MuteBlockSnapshot{
		Muting:         map[string]struct{}{},
		BlockingMe:     map[string]struct{}{},
		RenoteMuting:   map[string]struct{}{},
		MutedInstances: map[string]struct{}{},
		MutingChannels: map[string]struct{}{},
	}
	if a.muting != nil {
		if ids, err := a.muting.ListMuteeIDs(userID); err == nil {
			for _, id := range ids {
				snap.Muting[id] = struct{}{}
			}
		}
	}
	if a.blocking != nil {
		if ids, err := a.blocking.ListBlockerIDs(userID); err == nil {
			for _, id := range ids {
				snap.BlockingMe[id] = struct{}{}
			}
		}
	}
	if a.renoteMuting != nil {
		if ids, err := a.renoteMuting.ListMuteeIDs(userID); err == nil {
			for _, id := range ids {
				snap.RenoteMuting[id] = struct{}{}
			}
		}
	}
	if a.channelMuting != nil {
		if rows, err := a.channelMuting.ListByUser(userID); err == nil {
			for _, r := range rows {
				snap.MutingChannels[r.ChannelID] = struct{}{}
			}
		}
	}
	if a.userRepo != nil {
		if profile, err := a.userRepo.FindProfileByUserID(userID); err == nil && profile != nil && len(profile.MutedInstances) > 0 {
			var hosts []string
			if json.Unmarshal(profile.MutedInstances, &hosts) == nil {
				for _, h := range hosts {
					if h != "" {
						// host は AP canonical の lowercase で比較する
						// (timeline filter / loadMutedInstances と統一)。
						snap.MutedInstances[strings.ToLower(h)] = struct{}{}
					}
				}
			}
		}
	}
	return snap
}

// relationReloadPublisherAdapter bridges PubSubService to the various
// RelationReloadPublisher interfaces (core/muting, core/blocking,
// core/following, api/channels, api/i) so relation changes reach every active
// streaming connection of the user (#2400).
//
// mutation 側は「誰の」「どちらの snapshot が」変わったかだけを知り、pubsub の
// 存在を知らない。hardMutePublisherAdapter (#791) と同じ形。
//
// 失敗は log のみで握る。realtime nudge が届かなくても API 応答は成功させる
// (client は次の再接続で新しい snapshot を取る)。
type relationReloadPublisherAdapter struct {
	pubsub *event.PubSubService
}

func (a *relationReloadPublisherAdapter) publish(userID string, scope stream.RelationScope) {
	if a.pubsub == nil || userID == "" {
		return
	}
	payload := stream.RelationReloadPayload{UserID: userID, Scope: scope}
	if err := a.pubsub.Publish(context.Background(), stream.RelationReloadTopic, payload); err != nil {
		slog.Warn("router: relation reload publish failed",
			"userID", userID, "scope", scope, "err", err)
	}
}

func (a *relationReloadPublisherAdapter) PublishMuteBlockReload(userID string) {
	a.publish(userID, stream.RelationScopeMuteBlock)
}

func (a *relationReloadPublisherAdapter) PublishFollowingReload(userID string) {
	a.publish(userID, stream.RelationScopeFollowing)
}

// hardMutePublisherAdapter bridges PubSubService to i.HardMutePublisher so
// i/update can flush a wordmute reload event to every active streaming
// connection of the user (#791). Failures are logged but never bubbled — the
// API response should not fail just because the realtime nudge couldn't be
// delivered (clients still pick up the new rules at the next reconnect).
type hardMutePublisherAdapter struct {
	pubsub *event.PubSubService
}

func (a *hardMutePublisherAdapter) PublishHardMuteReload(userID string) {
	if a.pubsub == nil || userID == "" {
		return
	}
	payload := stream.WordMuteReloadPayload{UserID: userID}
	if err := a.pubsub.Publish(context.Background(), stream.WordMuteReloadTopic, payload); err != nil {
		slog.Warn("router: wordmute reload publish failed", "userID", userID, "err", err)
	}
}

// reactionNoteStreamAdapter bridges core/reaction.NoteStreamHook to the
// shared stream.NoteEventPublisher. Misskey TS upstream の wire format に
// 揃え、`{type: "reacted"|"unreacted", body: {reaction, emoji?, userId}}`
// を `noteStream:<noteID>` に publish する (#700)。
type reactionNoteStreamAdapter struct {
	pub *stream.NoteEventPublisher
}

func (a *reactionNoteStreamAdapter) OnReacted(noteID, userID, reaction string, emoji *corereaction.NoteStreamEmoji) {
	body := map[string]any{
		"reaction": reaction,
		"userId":   userID,
		"emoji":    nil,
	}
	if emoji != nil {
		body["emoji"] = map[string]any{"name": emoji.Name, "url": emoji.URL}
	}
	a.pub.PublishToNoteStream(noteID, "reacted", body)
}

func (a *reactionNoteStreamAdapter) OnUnreacted(noteID, userID, reaction string) {
	a.pub.PublishToNoteStream(noteID, "unreacted", map[string]any{
		"reaction": reaction,
		"userId":   userID,
	})
}

// noteDeleteStreamAdapter bridges core/note.DeleteNoteStreamHook to the
// shared stream.NoteEventPublisher (#700)。
type noteDeleteStreamAdapter struct {
	pub *stream.NoteEventPublisher
}

func (a *noteDeleteStreamAdapter) OnNoteDeleted(noteID string, deletedAt time.Time) {
	a.pub.PublishToNoteStream(noteID, "deleted", map[string]any{
		"deletedAt": deletedAt.UTC().Format(time.RFC3339Nano),
	})
}

// remoteStatsFetcherAdapter は corefederation.RemoteStatsFetcher を
// users.RemoteStatsFetcher interface に橋渡しする (#943)。layered architecture
// (api → core → repository) を保つため users package が federation を直接 import
// しない設計を維持する。
type remoteStatsFetcherAdapter struct {
	fetcher *corefederation.RemoteStatsFetcher
}

func (a *remoteStatsFetcherAdapter) Fetch(ctx context.Context, host, username string) *users.RemoteUserStatsView {
	stats := a.fetcher.Fetch(ctx, host, username)
	if stats == nil {
		return nil
	}
	return &users.RemoteUserStatsView{
		NotesCount:     stats.NotesCount,
		FollowersCount: stats.FollowersCount,
		FollowingCount: stats.FollowingCount,
	}
}
