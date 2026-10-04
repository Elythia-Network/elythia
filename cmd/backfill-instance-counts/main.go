// Command backfill-instance-counts recomputes `instance.notesCount` /
// `instance.usersCount` from the note / user tables (#3330).
//
// #3330 まで mk-go は 2 列を動かしておらず、それより前に作られた instance 行は
// notesCount が 0、usersCount が行を作ったときの 1 のまま残っている。#3330 から
// 投稿・利用者の取り込みで増減を積むようになったが、過去の分は埋まらない。
// 再計算は note の該当範囲の走査になるので、起動時には行わずこのバッチで流す。
//
// 値の定義は本家の instance chart の total と同じ (note は userHost、user は host
// で数え、renote・削除済み・凍結中も含める)。
//
// 冪等なので途中で失敗しても再実行して安全。まず -dry-run で差分を見ること。
// 稼働中の CounterBuffer (30 秒の窓) と並走すると、集計と書き込みの間の増減が
// 少しずれる。空いている時間帯に流せば実害は無い (docs/deployment.md)。
//
//	backfill-instance-counts -config .config/default.yml -dry-run
//	backfill-instance-counts -config .config/default.yml -batch 100 -sleep-ms 200
//	backfill-instance-counts -config .config/default.yml -from <last-instance-id>
package main

import (
	"flag"
	"log"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/maintenance"
)

func main() {
	cfgPath := flag.String("config", "/app/.config/default.yml", "path to mk-go config file")
	batchSize := flag.Int("batch", 100, "instance rows per keyset batch")
	sleepMs := flag.Int("sleep-ms", 100, "sleep between batches to limit DB load")
	fromID := flag.String("from", "", "resume from this instance id (exclusive)")
	dryRun := flag.Bool("dry-run", false, "print the differences without writing")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	db, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{})
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	cursor := *fromID
	var scanned, changed int
	var notesDelta, usersDelta int64
	for {
		res, err := maintenance.BackfillInstanceCountsBatch(db, cursor, *batchSize, *dryRun)
		if err != nil {
			// 失敗したバッチはまるごと書かれていない (1 本の UPDATE) ので、
			// このバッチの開始位置から流し直せばよい。
			log.Fatalf("backfill failed (scanned=%d before this batch; resume with -from %q): %v",
				scanned, cursor, err)
		}
		if res.Scanned == 0 {
			break
		}
		cursor = res.LastID
		scanned += res.Scanned
		changed += len(res.Changes)
		for _, c := range res.Changes {
			notesDelta += int64(c.NewNotes - c.OldNotes)
			usersDelta += int64(c.NewUsers - c.OldUsers)
			log.Printf("instance %s notesCount %d -> %d usersCount %d -> %d",
				c.Host, c.OldNotes, c.NewNotes, c.OldUsers, c.NewUsers)
		}
		log.Printf("scanned=%d changed=%d cursor=%s", scanned, changed, cursor)
		if *sleepMs > 0 {
			time.Sleep(time.Duration(*sleepMs) * time.Millisecond)
		}
	}

	mode := "applied"
	if *dryRun {
		mode = "dry-run (no writes)"
	}
	log.Printf("done [%s]: scanned=%d changed=%d notesCountDelta=%+d usersCountDelta=%+d",
		mode, scanned, changed, notesDelta, usersDelta)
}
