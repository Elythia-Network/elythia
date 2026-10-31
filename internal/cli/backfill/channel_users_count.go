package backfill

import "github.com/elythia-network/elythia/internal/cli/cliflag"

// ChannelUsersCount implements "elythia backfill channel-users-count": it
// recomputes `channel.usersCount` as the number of users who have a note in
// the channel (#3369).
//
// #3369 まで Elythia は usersCount をフォロー・解除で増減しており、本家の
// 「チャンネルに投稿した人の数」と違う値が入っている。#3369 から投稿で数える
// ようになったが、積むのは差分なので過去の分は直らない。本家の値は累積値
// (投稿を消して投稿し直すとまた増える) で DB から復元できないので、いまの実数へ
// 数え直す。
//
// 冪等なので途中で失敗しても再実行して安全。まず -dry-run で差分を見ること。
//
//	elythia backfill channel-users-count -config .config/default.yml -dry-run
//	elythia backfill channel-users-count -config .config/default.yml -batch 100 -sleep-ms 200
//	elythia backfill channel-users-count -config .config/default.yml -from <last-channel-id>
func ChannelUsersCount(args []string) int { return channelUsersCount(defaultEnv(), args) }

func channelUsersCount(e env, args []string) int {
	fs, cfgPath := newFlags(e, "channel-users-count")
	batchSize := fs.Int("batch", 100, "channel rows per keyset batch")
	sleepMs := fs.Int("sleep-ms", 100, "sleep between batches to limit DB load")
	fromID := fs.String("from", "", "resume from this channel id (exclusive)")
	dryRun := fs.Bool("dry-run", false, "print the differences without writing")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}

	_, db, ok := open(e, *cfgPath)
	if !ok {
		return 1
	}

	cursor := *fromID
	var scanned, changed int
	var usersDelta int64
	for {
		res, err := e.channelUsersBatch(db, cursor, *batchSize, *dryRun)
		if err != nil {
			// 失敗したバッチはまるごと書かれていない (1 本の UPDATE) ので、
			// このバッチの開始位置から流し直せばよい。
			e.logger.Printf("backfill failed (scanned=%d before this batch; resume with -from %q): %v",
				scanned, cursor, err)
			return 1
		}
		if res.Scanned == 0 {
			break
		}
		cursor = res.LastID
		scanned += res.Scanned
		changed += len(res.Changes)
		for _, c := range res.Changes {
			usersDelta += int64(c.NewUsers - c.OldUsers)
			e.logger.Printf("channel %s usersCount %d -> %d", c.ID, c.OldUsers, c.NewUsers)
		}
		e.logger.Printf("scanned=%d changed=%d cursor=%s", scanned, changed, cursor)
		pause(e, *sleepMs)
	}

	e.logger.Printf("done [%s]: scanned=%d changed=%d usersCountDelta=%+d",
		modeLabel(*dryRun, "dry-run (no writes)"), scanned, changed, usersDelta)
	return 0
}
