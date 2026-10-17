package backfill

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/maintenance"
	"github.com/elythia-network/elythia/internal/misc/id"
)

// ChatReads implements "elythia backfill chat-reads": it carries the read
// state of direct messages that Misskey / CherryPick kept in Redis into
// `chat_message.reads` (#3653).
//
// 本家と CherryPick は DM の既読を Redis にだけ持ち、`reads` を書かない。
// Elythia は既読を `reads` で判定するので、移行した DB では受け取った DM が
// すべて未読になり、/api/i の hasUnreadChatMessages が true のまま残る。
//
// -before (移行した時点) は必須。それより前に作られた DM だけを対象にし、
// 移行元の Redis に未読の記録が無い会話を既読にする。記録がある会話は触らない
// (本家の値は最新の未読の id だけで、どこまで読んだかは分からない)。
// Redis の記録が残っていないときは -assume-read で対象をすべて既読にする。
//
// 冪等なので途中で失敗しても再実行して安全。まず -dry-run で件数を見ること。
// 手順は docs/migration-from-ts.md。
//
//	elythia backfill chat-reads -config .config/default.yml -before 2026-10-01T00:00:00+09:00 -dry-run
//	elythia backfill chat-reads -config .config/default.yml -before 2026-10-01T00:00:00+09:00
//	elythia backfill chat-reads -config .config/default.yml -before 2026-10-01T00:00:00+09:00 -assume-read
func ChatReads(args []string) int { return chatReads(defaultEnv(), args) }

func chatReads(e env, args []string) int {
	fs, cfgPath := newFlags(e, "chat-reads")
	before := fs.String("before", "", "required: when the server was migrated (RFC 3339, e.g. 2026-10-01T00:00:00+09:00); only direct messages created before it are touched")
	assumeRead := fs.Bool("assume-read", false, "mark every targeted message read without reading Redis (when the Redis records of the source server are gone)")
	batchSize := fs.Int("batch", 1000, "messages per keyset batch")
	sleepMs := fs.Int("sleep-ms", 100, "sleep between batches to limit DB load")
	fromID := fs.String("from", "", "resume from this message id (exclusive)")
	dryRun := fs.Bool("dry-run", false, "print the counts without writing")
	tooLateMargin := fs.Duration("too-late-margin", maintenance.DefaultChatReadsTooLateMargin,
		"a message must be this much newer than the Redis record of the source server to count as evidence that -before is later than the migration")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	// **日時の指定が無ければ何もしない。** 既定値を置くと、移行の後に Elythia が
	// 書いた未読まで Redis の記録 (Elythia は書かない) で判定して既読にしてしまう。
	if *before == "" {
		fmt.Fprintf(e.stderr, "elythia backfill chat-reads: -before is required (the time the server was migrated)\n")
		fs.Usage()
		return 2
	}
	cutoff, err := time.Parse(time.RFC3339, *before)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backfill chat-reads: -before %q: %v\n", *before, err)
		fs.Usage()
		return 2
	}

	// **未来の -before は弾く。** 稼働中に流すと、1 周目と 2 周目の間に Elythia で
	// 受けた DM が 2 周目の対象に入り、1 周目で見ていない会話は検出されずに既読になる。
	if cutoff.After(e.now()) {
		fmt.Fprintf(e.stderr, "elythia backfill chat-reads: -before %q is in the future; pass the time the server was migrated\n", *before)
		fs.Usage()
		return 2
	}
	if *tooLateMargin < 0 {
		fmt.Fprintf(e.stderr, "elythia backfill chat-reads: -too-late-margin must not be negative\n")
		fs.Usage()
		return 2
	}

	cfg, db, ok := open(e, *cfgPath)
	if !ok {
		return 1
	}
	idGen, err := id.NewGenerator(cfg.ID)
	if err != nil {
		e.logger.Printf("id generator %q: %v", cfg.ID, err)
		return 1
	}

	var source maintenance.ChatReadStateSource = maintenance.AssumeReadSource{}
	sourceLabel := "assume-read"
	if !*assumeRead {
		client, closeRedis, err := e.openRedis(cfg)
		if err != nil {
			e.logger.Printf("open redis: %v (if the Redis records of the source server are gone, run with -assume-read)", err)
			return 1
		}
		defer closeRedis()
		source = &maintenance.RedisChatReadStateSource{Client: client, Prefix: cfg.Redis.KeyPrefix()}
		sourceLabel = "redis prefix " + fmt.Sprintf("%q", cfg.Redis.KeyPrefix())
	}
	e.logger.Printf("chat-reads: before=%s source=%s", cutoff.Format(time.RFC3339), sourceLabel)

	ctx, stop := e.signalContext()
	defer stop()

	opts := maintenance.ChatReadsBackfillOptions{
		Before:        cutoff,
		ParseTime:     idGen.ParseTime,
		Source:        source,
		TooLateMargin: *tooLateMargin,
	}
	run := func(dry bool) (*chatReadsTally, bool) {
		opts.DryRun = dry
		tally := newChatReadsTally()
		cursor := *fromID
		for {
			if ctx.Err() != nil {
				e.logger.Printf("interrupted (resume with -from %q)", cursor)
				tally.report(e, dry, "aborted")
				return tally, false
			}
			res, err := e.chatReadsBatch(ctx, db, cursor, *batchSize, opts)
			tally.add(res)
			if err != nil {
				tally.report(e, dry, "aborted")
				switch {
				case errors.Is(err, maintenance.ErrNoChatReadRecords):
					e.logger.Printf("Redis (prefix %q) にチャットの未読の記録が 1 つも無い。prefix や接続先が移行元と違うか、"+
						"記録が残っていないか、すべて読まれていた。確かめたうえで、対象をすべて既読にしてよければ -assume-read を付けて流す",
						cfg.Redis.KeyPrefix())
				case errors.Is(err, maintenance.ErrChatReadsCutoffTooLate):
					e.logger.Printf("既読にせずに止めた: %v", err)
				default:
					// 失敗したバッチは書かれていない (書くのは最後の 1 本の UPDATE) ので、
					// このバッチの開始位置から流し直せばよい。
					e.logger.Printf("backfill failed (resume with -from %q): %v", cursor, err)
				}
				return tally, false
			}
			if res.Scanned == 0 {
				return tally, true
			}
			cursor = res.LastID
			e.logger.Printf("scanned=%d marked=%d kept=%d cursor=%s", tally.scanned, tally.marked, tally.kept, cursor)
			pause(e, *sleepMs)
		}
	}

	// **書く前に全部を見る。** -before が遅すぎる証拠 (本家の marker より新しい DM) は
	// 新しい DM ほど後ろのバッチに出るので、書きながら見ると、見つかる前に Elythia で
	// 受けた未読を既読にしてしまう。dry-run の結果はこの 1 周目そのもの。会話の判定は
	// source が覚えているので、2 周目も同じ判定になる。
	checked, ok := run(true)
	if !ok {
		return 1
	}
	if *dryRun {
		checked.report(e, true, "done")
		return 0
	}
	if len(checked.tooLate) > 0 {
		checked.report(e, true, "aborted")
		e.logger.Printf("既読にせずに止めた: %v", maintenance.ErrChatReadsCutoffTooLate)
		return 1
	}
	applied, ok := run(false)
	if !ok {
		return 1
	}
	applied.report(e, false, "done")
	return 0
}

// chatReadsTally sums the batches. It holds ids and counts only, never the
// text of a message.
type chatReadsTally struct {
	scanned, afterCutoff, unparsable, marked, kept int
	convs                                          map[maintenance.ChatConversation]*maintenance.ChatReadsConversationResult
	newest                                         time.Time
	tooLate                                        []maintenance.ChatReadsTooLate
}

func newChatReadsTally() *chatReadsTally {
	return &chatReadsTally{convs: make(map[maintenance.ChatConversation]*maintenance.ChatReadsConversationResult)}
}

func (t *chatReadsTally) add(res maintenance.ChatReadsBackfillResult) {
	t.scanned += res.Scanned
	t.afterCutoff += res.AfterCutoff
	t.unparsable += res.Unparsable
	if res.Newest.After(t.newest) {
		t.newest = res.Newest
	}
	t.tooLate = append(t.tooLate, res.TooLate...)
	for _, c := range res.Conversations {
		t.marked += c.Marked
		t.kept += c.Kept
		cur, ok := t.convs[c.Conversation]
		if !ok {
			cp := c
			t.convs[c.Conversation] = &cp
			continue
		}
		cur.Marked += c.Marked
		cur.Kept += c.Kept
	}
}

// report prints one line per conversation, one per reader and the totals.
func (t *chatReadsTally) report(e env, dryRun bool, verb string) {
	convs := make([]*maintenance.ChatReadsConversationResult, 0, len(t.convs))
	for _, c := range t.convs {
		convs = append(convs, c)
	}
	slices.SortFunc(convs, func(a, b *maintenance.ChatReadsConversationResult) int {
		if c := strings.Compare(a.Conversation.ReaderID, b.Conversation.ReaderID); c != 0 {
			return c
		}
		return strings.Compare(a.Conversation.SenderID, b.Conversation.SenderID)
	})
	markedLabel := "marked"
	if dryRun {
		markedLabel = "would-mark"
	}
	type readerSum struct{ convs, marked, kept int }
	readers := map[string]*readerSum{}
	var readerIDs []string
	broken := 0
	for _, c := range convs {
		fmt.Fprintf(e.stderr, "conversation reader=%s sender=%s state=%s %s=%d kept=%d\n",
			c.Conversation.ReaderID, c.Conversation.SenderID, c.State, markedLabel, c.Marked, c.Kept)
		r, ok := readers[c.Conversation.ReaderID]
		if !ok {
			r = &readerSum{}
			readers[c.Conversation.ReaderID] = r
			readerIDs = append(readerIDs, c.Conversation.ReaderID)
		}
		r.convs++
		r.marked += c.Marked
		r.kept += c.Kept
		if c.State == maintenance.ChatReadStateBroken {
			broken++
		}
	}
	for _, id := range readerIDs {
		r := readers[id]
		fmt.Fprintf(e.stderr, "reader %s conversations=%d %s=%d kept=%d\n", id, r.convs, markedLabel, r.marked, r.kept)
	}
	fmt.Fprintf(e.stderr, "%s [%s]: scanned=%d conversations=%d %s=%d kept=%d broken-conversations=%d after-cutoff=%d unparsable-id=%d\n",
		verb, modeLabel(dryRun, "dry-run (no writes)"), t.scanned, len(convs), markedLabel, t.marked, t.kept,
		broken, t.afterCutoff, t.unparsable)
	newest := "none"
	if !t.newest.IsZero() {
		newest = t.newest.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(e.stderr, "newest targeted message: %s (compare it with the time of the migration)\n", newest)
	for _, l := range t.tooLate {
		fmt.Fprintf(e.stderr, "too-late reader=%s sender=%s marker=%s marker-at=%s newer=%d newest-at=%s\n",
			l.Conversation.ReaderID, l.Conversation.SenderID, l.MarkerID,
			l.MarkerAt.UTC().Format(time.RFC3339Nano), l.Newer, l.NewestAt.UTC().Format(time.RFC3339Nano))
	}
	if len(t.tooLate) > 0 {
		fmt.Fprintf(e.stderr, "警告: 移行元の Redis の記録 (marker) より新しい DM が -before より前にある会話が %d 件ある。"+
			"本家は新着のたびに marker を上書きするので、これらは移行の後に Elythia で受けたもの。-before が移行より遅い。"+
			"このまま流すと、Elythia で受けた未読を既読にしてしまうので、書き込みはしない。-before を移行した時点に直す。"+
			"-before が正しいなら、marker-at と newest-at の差を見て、移行元が止まる直前の DM による誤検出なら "+
			"-too-late-margin を広げて流し直す (手順は docs/migration-from-ts.md)\n", len(t.tooLate))
	}
	if broken > 0 {
		fmt.Fprintf(e.stderr, "Redis の記録が読めない会話が %d 件ある (キーの型が違う)。その会話は未読のまま残した。利用者が開けば既読になる\n", broken)
	}
}
