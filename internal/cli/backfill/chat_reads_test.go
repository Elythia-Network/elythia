package backfill

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintenance"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

const chatReadsBefore = "2026-10-01T00:00:00+09:00"

// withRedis points the harness at a miniredis and records whether it was
// opened.
func (h *harness) withRedis(t *testing.T) (*miniredis.Miniredis, *bool) {
	t.Helper()
	mr := miniredis.RunT(t)
	opened := false
	h.e.openRedis = func(*config.Config) (redis.Cmdable, func(), error) {
		opened = true
		c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		return c, func() { _ = c.Close() }, nil
	}
	return mr, &opened
}

func TestChatReads_Flags(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		h := newHarness()
		assert.Equal(t, 0, chatReads(h.e, []string{"-h"}))
		assert.Contains(t, h.stderr.String(), "Usage: elythia backfill chat-reads")
		assert.Contains(t, h.stderr.String(), "/app/.config/default.yml")
	})
	for name, args := range map[string][]string{
		"missing -before":  nil,
		"empty -before":    {"-before", ""},
		"bad -before":      {"-before", "2026-10-01"},
		"bad flag":         {"-before", chatReadsBefore, "-nope"},
		"stray argument":   {"-before", chatReadsBefore, "dry-run"},
		"-before is a day": {"-before", "yesterday", "-assume-read"},
		"future -before":   {"-before", "2999-01-01T00:00:00Z"},
		"negative margin":  {"-before", chatReadsBefore, "-too-late-margin", "-1m"},
	} {
		t.Run(name, func(t *testing.T) {
			// 日時が無い・読めないときは、DB にも Redis にも触れずに使い方を出す。
			h := newHarness()
			opened := false
			h.e.openDB = func(*config.Config) (*gorm.DB, error) { opened = true; return h.db, nil }
			_, redisOpened := h.withRedis(t)
			assert.Equal(t, 2, chatReads(h.e, args))
			assert.False(t, opened)
			assert.False(t, *redisOpened)
			assert.Contains(t, h.stderr.String(), "Usage: elythia backfill chat-reads")
		})
	}
	t.Run("future -before is named", func(t *testing.T) {
		h := newHarness()
		h.e.now = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }
		// 今と同じ時刻は受け付け、1 秒でも先なら弾く。
		h.e.chatReadsBatch = func(context.Context, *gorm.DB, string, int, maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			return maintenance.ChatReadsBackfillResult{}, nil
		}
		assert.Equal(t, 0, chatReads(h.e, []string{"-before", "2026-10-01T00:00:00Z", "-assume-read"}))
		assert.Equal(t, 2, chatReads(h.e, []string{"-before", "2026-10-01T00:00:01Z", "-assume-read"}))
		assert.Contains(t, h.stderr.String(), "is in the future")
	})
	t.Run("missing -before names the flag", func(t *testing.T) {
		h := newHarness()
		assert.Equal(t, 2, chatReads(h.e, []string{"-assume-read"}))
		assert.Contains(t, h.stderr.String(), "-before is required")
	})
}

func TestChatReads_SetupErrors(t *testing.T) {
	args := []string{"-before", chatReadsBefore}
	t.Run("config error", func(t *testing.T) {
		h := newHarness()
		h.e.loadConfig = func(string) (*config.Config, error) { return nil, errBoom }
		assert.Equal(t, 1, chatReads(h.e, args))
		assert.Contains(t, h.log.String(), "load config: boom")
	})
	t.Run("db error", func(t *testing.T) {
		h := newHarness()
		h.e.openDB = func(*config.Config) (*gorm.DB, error) { return nil, errBoom }
		assert.Equal(t, 1, chatReads(h.e, args))
		assert.Contains(t, h.log.String(), "open db: boom")
	})
	t.Run("id generator error", func(t *testing.T) {
		h := newHarness()
		h.e.loadConfig = func(string) (*config.Config, error) { return &config.Config{ID: "nope"}, nil }
		assert.Equal(t, 1, chatReads(h.e, args))
		assert.Contains(t, h.log.String(), `id generator "nope"`)
	})
	t.Run("redis error", func(t *testing.T) {
		h := newHarness()
		h.e.openRedis = func(*config.Config) (redis.Cmdable, func(), error) { return nil, nil, errBoom }
		ran := false
		h.e.chatReadsBatch = func(context.Context, *gorm.DB, string, int, maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			ran = true
			return maintenance.ChatReadsBackfillResult{}, nil
		}
		assert.Equal(t, 1, chatReads(h.e, args))
		assert.Contains(t, h.log.String(), "open redis: boom")
		assert.Contains(t, h.log.String(), "-assume-read")
		assert.False(t, ran, "a Redis that cannot be opened must not fall back to marking everything read")
	})
}

func TestChatReads_Loop(t *testing.T) {
	t.Run("passes the options and walks the cursor", func(t *testing.T) {
		h := newHarness()
		h.e.loadConfig = func(string) (*config.Config, error) {
			cfg := &config.Config{ID: "aidx"}
			cfg.Redis.Prefix = "example.tld"
			return cfg, nil
		}
		h.withRedis(t)
		var froms []string
		conv := maintenance.ChatConversation{ReaderID: "r", SenderID: "s"}
		h.e.chatReadsBatch = func(_ context.Context, _ *gorm.DB, from string, n int, opts maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			froms = append(froms, from)
			assert.Equal(t, 50, n)
			assert.True(t, opts.DryRun)
			want, _ := time.Parse(time.RFC3339, chatReadsBefore)
			assert.True(t, opts.Before.Equal(want))
			assert.Equal(t, maintenance.DefaultChatReadsTooLateMargin, opts.TooLateMargin)
			src, ok := opts.Source.(*maintenance.RedisChatReadStateSource)
			require.True(t, ok)
			assert.Equal(t, "example.tld:", src.Prefix)
			switch from {
			case "start":
				return maintenance.ChatReadsBackfillResult{Scanned: 2, LastID: "m2", Conversations: []maintenance.ChatReadsConversationResult{
					{Conversation: conv, State: maintenance.ChatReadStateRead, Marked: 2},
				}}, nil
			case "m2":
				return maintenance.ChatReadsBackfillResult{Scanned: 3, LastID: "m5", AfterCutoff: 1, Unparsable: 1, Conversations: []maintenance.ChatReadsConversationResult{
					{Conversation: conv, State: maintenance.ChatReadStateRead, Marked: 1},
				}}, nil
			}
			return maintenance.ChatReadsBackfillResult{}, nil
		}
		assert.Equal(t, 0, chatReads(h.e, []string{"-before", chatReadsBefore, "-dry-run", "-batch", "50", "-sleep-ms", "7", "-from", "start"}))
		assert.Equal(t, []string{"start", "m2", "m5"}, froms)
		assert.Equal(t, []time.Duration{7 * time.Millisecond, 7 * time.Millisecond}, h.sleeps)
		out := h.stderr.String()
		assert.Contains(t, out, "conversation reader=r sender=s state=read would-mark=3 kept=0")
		assert.Contains(t, out, "reader r conversations=1 would-mark=3 kept=0")
		assert.Contains(t, out, "done [dry-run (no writes)]: scanned=5 conversations=1 would-mark=3 kept=0 broken-conversations=0 after-cutoff=1 unparsable-id=1")
	})
	t.Run("-assume-read does not open Redis", func(t *testing.T) {
		h := newHarness()
		_, opened := h.withRedis(t)
		var dry []bool
		h.e.chatReadsBatch = func(_ context.Context, _ *gorm.DB, _ string, _ int, opts maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			assert.IsType(t, maintenance.AssumeReadSource{}, opts.Source)
			assert.Equal(t, 30*time.Minute, opts.TooLateMargin)
			dry = append(dry, opts.DryRun)
			return maintenance.ChatReadsBackfillResult{}, nil
		}
		assert.Equal(t, 0, chatReads(h.e, []string{"-before", chatReadsBefore, "-assume-read", "-too-late-margin", "30m"}))
		assert.False(t, *opened)
		// 書く前に、書かない 1 周目で全部を見る。
		assert.Equal(t, []bool{true, false}, dry)
		assert.Contains(t, h.log.String(), "source=assume-read")
		assert.Contains(t, h.stderr.String(), "done [applied]")
	})
	t.Run("batch error", func(t *testing.T) {
		h := newHarness()
		h.withRedis(t)
		h.e.chatReadsBatch = func(_ context.Context, _ *gorm.DB, from string, _ int, opts maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			if from == "" {
				return maintenance.ChatReadsBackfillResult{Scanned: 1, LastID: "m1"}, nil
			}
			if opts.DryRun {
				return maintenance.ChatReadsBackfillResult{}, nil
			}
			return maintenance.ChatReadsBackfillResult{}, errBoom
		}
		assert.Equal(t, 1, chatReads(h.e, []string{"-before", chatReadsBefore}))
		assert.Contains(t, h.log.String(), `resume with -from "m1"`)
		assert.Contains(t, h.stderr.String(), "aborted [applied]")
	})
	t.Run("error while checking writes nothing", func(t *testing.T) {
		h := newHarness()
		h.withRedis(t)
		wrote := false
		h.e.chatReadsBatch = func(_ context.Context, _ *gorm.DB, _ string, _ int, opts maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			if !opts.DryRun {
				wrote = true
			}
			return maintenance.ChatReadsBackfillResult{}, errBoom
		}
		assert.Equal(t, 1, chatReads(h.e, []string{"-before", chatReadsBefore}))
		assert.False(t, wrote)
		assert.Contains(t, h.stderr.String(), "aborted [dry-run (no writes)]")
	})
	tooLate := maintenance.ChatReadsBackfillResult{
		Scanned: 2, LastID: "m2", Newest: time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC),
		TooLate: []maintenance.ChatReadsTooLate{{
			Conversation: maintenance.ChatConversation{ReaderID: "r", SenderID: "s"}, MarkerID: "m0", Newer: 2,
			MarkerAt: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC), NewestAt: time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC),
		}},
	}
	t.Run("a late -before stops before writing", func(t *testing.T) {
		h := newHarness()
		h.withRedis(t)
		wrote := false
		h.e.chatReadsBatch = func(_ context.Context, _ *gorm.DB, from string, _ int, opts maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			if !opts.DryRun {
				wrote = true
			}
			if from == "" {
				return tooLate, nil
			}
			return maintenance.ChatReadsBackfillResult{}, nil
		}
		assert.Equal(t, 1, chatReads(h.e, []string{"-before", chatReadsBefore}))
		assert.False(t, wrote, "a late -before must stop before the writing pass")
		assert.Contains(t, h.stderr.String(), "too-late reader=r sender=s marker=m0 marker-at=2026-09-29T00:00:00Z newer=2 newest-at=2026-09-30T01:02:03Z")
		assert.Contains(t, h.stderr.String(), "-before が移行より遅い")
		assert.Contains(t, h.log.String(), "既読にせずに止めた")
	})
	t.Run("a late -before is a warning in a dry-run", func(t *testing.T) {
		h := newHarness()
		h.withRedis(t)
		h.e.chatReadsBatch = func(_ context.Context, _ *gorm.DB, from string, _ int, _ maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			if from == "" {
				return tooLate, nil
			}
			return maintenance.ChatReadsBackfillResult{}, nil
		}
		assert.Equal(t, 0, chatReads(h.e, []string{"-before", chatReadsBefore, "-dry-run"}))
		out := h.stderr.String()
		assert.Contains(t, out, "too-late reader=r sender=s marker=m0 marker-at=2026-09-29T00:00:00Z newer=2 newest-at=2026-09-30T01:02:03Z")
		assert.Contains(t, out, "警告")
		assert.Contains(t, out, "newest targeted message: 2026-09-30T01:02:03Z")
	})
	t.Run("a late -before found while writing", func(t *testing.T) {
		h := newHarness()
		h.withRedis(t)
		h.e.chatReadsBatch = func(_ context.Context, _ *gorm.DB, _ string, _ int, opts maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			if opts.DryRun {
				return maintenance.ChatReadsBackfillResult{}, nil
			}
			return tooLate, maintenance.ErrChatReadsCutoffTooLate
		}
		assert.Equal(t, 1, chatReads(h.e, []string{"-before", chatReadsBefore}))
		assert.Contains(t, h.log.String(), "既読にせずに止めた")
		assert.NotContains(t, h.log.String(), "resume with")
		assert.Contains(t, h.stderr.String(), "too-late reader=r sender=s")
	})
	t.Run("no records in Redis", func(t *testing.T) {
		h := newHarness()
		h.withRedis(t)
		h.e.chatReadsBatch = func(context.Context, *gorm.DB, string, int, maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			return maintenance.ChatReadsBackfillResult{}, maintenance.ErrNoChatReadRecords
		}
		assert.Equal(t, 1, chatReads(h.e, []string{"-before", chatReadsBefore}))
		assert.Contains(t, h.log.String(), "-assume-read")
		assert.NotContains(t, h.log.String(), "resume with")
	})
	t.Run("interrupted", func(t *testing.T) {
		h := newHarness()
		h.withRedis(t)
		h.e.signalContext = func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		}
		ran := false
		h.e.chatReadsBatch = func(context.Context, *gorm.DB, string, int, maintenance.ChatReadsBackfillOptions) (maintenance.ChatReadsBackfillResult, error) {
			ran = true
			return maintenance.ChatReadsBackfillResult{}, nil
		}
		assert.Equal(t, 1, chatReads(h.e, []string{"-before", chatReadsBefore}))
		assert.False(t, ran)
		assert.Contains(t, h.log.String(), "interrupted")
	})
}

func TestChatReadsTally_ReportsBrokenConversations(t *testing.T) {
	h := newHarness()
	tally := newChatReadsTally()
	tally.add(maintenance.ChatReadsBackfillResult{Scanned: 3, Conversations: []maintenance.ChatReadsConversationResult{
		{Conversation: maintenance.ChatConversation{ReaderID: "r2", SenderID: "s"}, State: maintenance.ChatReadStateBroken, Kept: 1},
		{Conversation: maintenance.ChatConversation{ReaderID: "r1", SenderID: "t"}, State: maintenance.ChatReadStateUnread, Kept: 1},
		{Conversation: maintenance.ChatConversation{ReaderID: "r1", SenderID: "s"}, State: maintenance.ChatReadStateRead, Marked: 1},
	}})
	tally.report(h.e, false, "done")
	out := h.stderr.String()
	assert.Contains(t, out, "conversation reader=r2 sender=s state=broken marked=0 kept=1")
	assert.Contains(t, out, "reader r1 conversations=2 marked=1 kept=1")
	assert.Contains(t, out, "broken-conversations=1")
	assert.Contains(t, out, "キーの型が違う")
	// 会話は reader、sender の順に並べる。
	assert.Less(t, indexOf(out, "reader=r1 sender=s"), indexOf(out, "reader=r1 sender=t"))
	assert.Less(t, indexOf(out, "reader=r1 sender=t"), indexOf(out, "reader=r2 sender=s"))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestChatReads_MissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yml")
	assert.Equal(t, 1, ChatReads([]string{"-config", missing, "-before", chatReadsBefore}))
}

// 移行した DB (reads が空の DM) に流すと、移行元で読んでいた利用者の /api/i の
// hasUnreadChatMessages が false になり、未読の記録が残る利用者は true のまま残る。
func TestChatReads_AgainstDatabase(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	require.NoError(t, db.Exec(`DELETE FROM "chat_message"`).Error)

	users := []string{"crc_alice", "crc_bob", "crc_carol"}
	// 前の実行が途中で止まって Cleanup が走らなかったときの残りも消す。
	require.NoError(t, db.Exec(`DELETE FROM "user" WHERE id IN ?`, users).Error)

	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	cutoff, err := time.Parse(time.RFC3339, chatReadsBefore)
	require.NoError(t, err)

	for _, u := range users {
		token := "tok_" + u
		require.NoError(t, db.Create(&model.User{
			ID: u, Username: u, UsernameLower: u, Token: &token,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}).Error)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "chat_message" WHERE "fromUserId" IN ?`, users)
		db.Exec(`DELETE FROM "user" WHERE id IN ?`, users)
	})
	dm := func(at time.Time, from, to string) string {
		msgID := gen.Generate(at)
		text := "do-not-print " + msgID
		require.NoError(t, db.Create(&model.ChatMessage{
			ID: msgID, FromUserID: from, ToUserID: &to, Text: &text, Reads: model.StringArray{},
		}).Error)
		return msgID
	}
	// alice は移行の前に bob と carol からの DM をどちらも読んでいた。
	// bob は alice からの DM を読んでいない (本家の記録が残っている)。
	for i := range 11 {
		dm(cutoff.Add(-time.Duration(30-i)*time.Hour), "crc_bob", "crc_alice")
	}
	dm(cutoff.Add(-2*time.Hour), "crc_carol", "crc_alice")
	unread := dm(cutoff.Add(-time.Hour), "crc_alice", "crc_bob")

	mr := miniredis.RunT(t)
	require.NoError(t, mr.Set("example.tld:newUserChatMessageExists:crc_bob:crc_alice", unread))
	_, err = mr.SetAdd("example.tld:newChatMessagesExists:crc_bob", "user:crc_alice")
	require.NoError(t, err)

	flags := repository.NewMeUnreadFlagsRepository(db)
	hasUnread := func(userID string) bool {
		f, err := flags.ReadMeUnreadFlags(userID)
		require.NoError(t, err)
		return f.HasUnreadChatMessages
	}
	require.True(t, hasUnread("crc_alice"))
	require.True(t, hasUnread("crc_bob"))

	run := func(args ...string) (int, string) {
		h := newHarness()
		h.db = db
		h.e.loadConfig = func(string) (*config.Config, error) {
			cfg := &config.Config{ID: "aidx"}
			cfg.Redis.Prefix = "example.tld"
			return cfg, nil
		}
		h.e.openRedis = func(*config.Config) (redis.Cmdable, func(), error) {
			c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			return c, func() { _ = c.Close() }, nil
		}
		code := chatReads(h.e, append([]string{"-before", chatReadsBefore, "-batch", "5"}, args...))
		return code, h.stderr.String() + h.log.String()
	}

	code, out := run("-dry-run")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "conversation reader=crc_alice sender=crc_bob state=read would-mark=11 kept=0")
	assert.Contains(t, out, "conversation reader=crc_bob sender=crc_alice state=unread would-mark=0 kept=1")
	assert.NotContains(t, out, "do-not-print", "the text of a message must not be printed")
	assert.Contains(t, out, "newest targeted message: "+cutoff.Add(-time.Hour).UTC().Format(time.RFC3339))
	assert.True(t, hasUnread("crc_alice"), "a dry-run must not write")

	code, out = run()
	require.Equal(t, 0, code, out)
	assert.False(t, hasUnread("crc_alice"))
	assert.True(t, hasUnread("crc_bob"))

	code, out = run()
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "scanned=1 conversations=1 marked=0 kept=1")
	var doubled int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM "chat_message" WHERE cardinality("reads") > 1`).Scan(&doubled).Error)
	assert.Zero(t, doubled, "a second run must not add the reader twice")
}

func TestDefaultEnv_OpenRedis(t *testing.T) {
	e := defaultEnv()
	cfg := &config.Config{}
	cfg.Redis.Host = "127.0.0.1"
	cfg.Redis.Port = testutil.DeadPort(t)
	_, _, err := e.openRedis(cfg)
	assert.Error(t, err)

	mr := miniredis.RunT(t)
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	cfg.Redis.Port = port
	client, closeRedis, err := e.openRedis(cfg)
	require.NoError(t, err)
	require.NoError(t, client.Set(context.Background(), "k", "v", 0).Err())
	closeRedis()
	mr.CheckGet(t, "k", "v")
}
