package backfill

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/maintenance"
	"github.com/shiroha-a/mk/internal/testutil"
)

var errBoom = errors.New("boom")

// harness is an env whose batches are scripted and whose output is captured.
type harness struct {
	e      env
	stderr bytes.Buffer
	log    bytes.Buffer
	sleeps []time.Duration
	db     *gorm.DB
}

func newHarness() *harness {
	h := &harness{db: &gorm.DB{}}
	e := defaultEnv()
	e.stderr = &h.stderr
	e.logger = log.New(&h.log, "", 0)
	e.loadConfig = func(string) (*config.Config, error) { return &config.Config{ID: "aidx"}, nil }
	e.openDB = func(*config.Config) (*gorm.DB, error) { return h.db, nil }
	e.sleep = func(d time.Duration) { h.sleeps = append(h.sleeps, d) }
	e.signalContext = func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	}
	h.e = e
	return h
}

// commands lists every batch so the behavior they share can be checked once
// for all of them.
var commands = map[string]func(env, []string) int{
	"avatar-public-url": avatarPublicURL,
	"emoji-system-file": emojiSystemFile,
	"instance-counts":   instanceCounts,
	"note-tags":         noteTags,
	"remote-host":       remoteHost,
}

func TestCommands_SharedBehavior(t *testing.T) {
	for name, run := range commands {
		t.Run(name+" help", func(t *testing.T) {
			h := newHarness()
			assert.Equal(t, 0, run(h.e, []string{"-h"}))
			assert.Contains(t, h.stderr.String(), "Usage: elythia backfill "+name)
			assert.Contains(t, h.stderr.String(), "/app/.config/default.yml", "the -config default must stay the image path")
		})
		t.Run(name+" bad flag", func(t *testing.T) {
			h := newHarness()
			assert.Equal(t, 2, run(h.e, []string{"-nope"}))
		})
		t.Run(name+" stray argument", func(t *testing.T) {
			// `-dry-run` を `dry-run` と書き間違えたら、書き込みに進まず止まること。
			h := newHarness()
			opened := false
			h.e.openDB = func(*config.Config) (*gorm.DB, error) { opened = true; return h.db, nil }
			assert.Equal(t, 2, run(h.e, []string{"dry-run"}))
			assert.False(t, opened)
		})
		t.Run(name+" config error", func(t *testing.T) {
			h := newHarness()
			h.e.loadConfig = func(string) (*config.Config, error) { return nil, errBoom }
			assert.Equal(t, 1, run(h.e, nil))
			assert.Contains(t, h.log.String(), "load config: boom")
		})
		t.Run(name+" db error", func(t *testing.T) {
			h := newHarness()
			h.e.openDB = func(*config.Config) (*gorm.DB, error) { return nil, errBoom }
			assert.Equal(t, 1, run(h.e, nil))
			assert.Contains(t, h.log.String(), "open db: boom")
		})
	}
}

func TestExportedEntryPoints_MissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yml")
	for name, run := range map[string]func([]string) int{
		"avatar-public-url": AvatarPublicURL,
		"emoji-system-file": EmojiSystemFile,
		"instance-counts":   InstanceCounts,
		"note-tags":         NoteTags,
		"remote-host":       RemoteHost,
	} {
		assert.Equal(t, 1, run([]string{"-config", missing}), name)
	}
}

// 既定の batch 関数が実 DB に対して配線されていることを、空の schema で確かめる。
// 1 バッチ分の SQL の正しさは maintenance パッケージのテストが見ている。
func TestCommands_RunAgainstEmptyDatabase(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.db = db
			assert.Equal(t, 0, run(h.e, nil), h.log.String()+h.stderr.String())
		})
	}
}

func TestDefaultEnv(t *testing.T) {
	e := defaultEnv()
	assert.Equal(t, os.Stderr, e.stderr)
	assert.Len(t, e.hostColumnList, len(maintenance.HostColumns))
	ctx, cancel := e.signalContext()
	cancel()
	<-ctx.Done()
	// 誰も listen していない宛先へは、接続確認の段で失敗する。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	cfg := &config.Config{}
	cfg.DB.Host = "127.0.0.1"
	cfg.DB.Port = port
	cfg.DB.User = "none"
	cfg.DB.DB = "none"
	_, err = e.openDB(cfg)
	assert.Error(t, err)
}

func TestPauseAndModeLabel(t *testing.T) {
	h := newHarness()
	pause(h.e, 0)
	assert.Empty(t, h.sleeps)
	pause(h.e, 250)
	assert.Equal(t, []time.Duration{250 * time.Millisecond}, h.sleeps)
	assert.Equal(t, "applied", modeLabel(false, "dry"))
	assert.Equal(t, "dry", modeLabel(true, "dry"))
}

func TestAvatarPublicURL(t *testing.T) {
	type call struct {
		cursor string
		batch  int
		dry    bool
	}
	t.Run("walks the cursor until an empty batch", func(t *testing.T) {
		h := newHarness()
		var calls []call
		results := []maintenance.AvatarPublicURLBackfillResult{
			{Scanned: 2, Updated: 1, LastID: "b"},
			{Scanned: 1, Updated: 1, LastID: "c"},
			{},
		}
		h.e.avatarBatch = func(db *gorm.DB, from string, n int, dry bool) (maintenance.AvatarPublicURLBackfillResult, error) {
			assert.Same(t, h.db, db)
			calls = append(calls, call{from, n, dry})
			r := results[0]
			results = results[1:]
			return r, nil
		}
		assert.Equal(t, 0, avatarPublicURL(h.e, []string{"-from", "a", "-batch", "5", "-dry-run", "-sleep-ms", "7"}))
		assert.Equal(t, []call{{"a", 5, true}, {"b", 5, true}, {"c", 5, true}}, calls)
		assert.Len(t, h.sleeps, 2)
		assert.Contains(t, h.log.String(), "done (dry-run): scanned=3 updated=2")
	})
	t.Run("error reports the resume point", func(t *testing.T) {
		h := newHarness()
		n := 0
		h.e.avatarBatch = func(*gorm.DB, string, int, bool) (maintenance.AvatarPublicURLBackfillResult, error) {
			n++
			if n == 1 {
				return maintenance.AvatarPublicURLBackfillResult{Scanned: 4, LastID: "d"}, nil
			}
			return maintenance.AvatarPublicURLBackfillResult{Scanned: 9}, errBoom
		}
		assert.Equal(t, 1, avatarPublicURL(h.e, nil))
		assert.Contains(t, h.log.String(), `backfill failed (scanned=4 before this batch; resume with -from "d"): boom`)
	})
	t.Run("defaults", func(t *testing.T) {
		h := newHarness()
		var got call
		h.e.avatarBatch = func(_ *gorm.DB, from string, n int, dry bool) (maintenance.AvatarPublicURLBackfillResult, error) {
			got = call{from, n, dry}
			return maintenance.AvatarPublicURLBackfillResult{}, nil
		}
		assert.Equal(t, 0, avatarPublicURL(h.e, nil))
		assert.Equal(t, call{"", 1000, false}, got)
		assert.Contains(t, h.log.String(), "done (applied)")
	})
}

func TestInstanceCounts(t *testing.T) {
	t.Run("logs every change and the totals", func(t *testing.T) {
		h := newHarness()
		var cursors []string
		var batch int
		results := []maintenance.InstanceCountsBackfillResult{
			{Scanned: 2, LastID: "i2", Changes: []maintenance.InstanceCountChange{
				{Host: "remote.example", OldNotes: 0, NewNotes: 5, OldUsers: 1, NewUsers: 3},
			}},
			{},
		}
		h.e.instanceBatch = func(_ *gorm.DB, from string, n int, _ bool) (maintenance.InstanceCountsBackfillResult, error) {
			cursors = append(cursors, from)
			batch = n
			r := results[0]
			results = results[1:]
			return r, nil
		}
		assert.Equal(t, 0, instanceCounts(h.e, nil))
		assert.Equal(t, []string{"", "i2"}, cursors)
		assert.Equal(t, 100, batch)
		assert.Contains(t, h.log.String(), "instance remote.example notesCount 0 -> 5 usersCount 1 -> 3")
		assert.Contains(t, h.log.String(), "done [applied]: scanned=2 changed=1 notesCountDelta=+5 usersCountDelta=+2")
		assert.Len(t, h.sleeps, 1)
	})
	t.Run("dry-run label and error", func(t *testing.T) {
		h := newHarness()
		h.e.instanceBatch = func(*gorm.DB, string, int, bool) (maintenance.InstanceCountsBackfillResult, error) {
			return maintenance.InstanceCountsBackfillResult{}, nil
		}
		assert.Equal(t, 0, instanceCounts(h.e, []string{"-dry-run"}))
		assert.Contains(t, h.log.String(), "done [dry-run (no writes)]")

		h = newHarness()
		h.e.instanceBatch = func(*gorm.DB, string, int, bool) (maintenance.InstanceCountsBackfillResult, error) {
			return maintenance.InstanceCountsBackfillResult{}, errBoom
		}
		assert.Equal(t, 1, instanceCounts(h.e, []string{"-from", "x"}))
		assert.Contains(t, h.log.String(), `resume with -from "x"`)
	})
}

func TestNoteTags(t *testing.T) {
	t.Run("walks the cursor", func(t *testing.T) {
		h := newHarness()
		var cursors []string
		results := []maintenance.NoteTagsBackfillResult{{Scanned: 3, Updated: 2, LastID: "n3"}, {}}
		h.e.noteTagsBatch = func(_ *gorm.DB, from string, n int, dry bool) (maintenance.NoteTagsBackfillResult, error) {
			cursors = append(cursors, from)
			assert.Equal(t, 1000, n)
			assert.True(t, dry)
			r := results[0]
			results = results[1:]
			return r, nil
		}
		assert.Equal(t, 0, noteTags(h.e, []string{"-dry-run"}))
		assert.Equal(t, []string{"", "n3"}, cursors)
		assert.Contains(t, h.log.String(), "batch scanned=3 updated=2 cursor=n3 (total scanned=3 updated=2)")
		assert.Contains(t, h.log.String(), "done [dry-run (no writes)]: scanned=3 updated=2")
	})
	t.Run("error", func(t *testing.T) {
		h := newHarness()
		h.e.noteTagsBatch = func(*gorm.DB, string, int, bool) (maintenance.NoteTagsBackfillResult, error) {
			return maintenance.NoteTagsBackfillResult{}, errBoom
		}
		assert.Equal(t, 1, noteTags(h.e, []string{"-from", "n9"}))
		assert.Contains(t, h.log.String(), `backfill batch (cursor="n9"): boom`)
	})
}

func TestRemoteHost(t *testing.T) {
	cols := []maintenance.HostColumn{
		{Table: "user", KeysetColumn: "id", Column: "host"},
		{Table: "instance", KeysetColumn: "id", Column: "host"},
		{Table: "meta", KeysetColumn: "id", Column: "blockedHosts"},
		{Table: "meta", KeysetColumn: "id", Column: "silencedHosts"},
	}
	t.Run("walks every column", func(t *testing.T) {
		h := newHarness()
		h.e.hostColumnList = cols
		seen := map[string][]string{}
		h.e.hostBatch = func(_ *gorm.DB, col maintenance.HostColumn, from string, _ int, _ bool) (maintenance.HostBackfillResult, error) {
			key := col.Table + "." + col.Column
			seen[key] = append(seen[key], from)
			if from == "" {
				return maintenance.HostBackfillResult{Scanned: 1, Updated: 1, Conflicts: 1, LastKey: "k1",
					ConflictKeys: []maintenance.HostConflict{{Key: "k1", Host: "Mixed.Example", Normalized: "mixed.example"}}}, nil
			}
			return maintenance.HostBackfillResult{}, nil
		}
		assert.Equal(t, 0, remoteHost(h.e, nil))
		assert.Len(t, seen, 4)
		for _, cursors := range seen {
			assert.Equal(t, []string{"", "k1"}, cursors)
		}
		assert.Contains(t, h.log.String(), `conflict user.host id="k1" host="Mixed.Example" -> "mixed.example"`)
		assert.Contains(t, h.log.String(), "done [applied]: scanned=4 updated=4 conflicts=4")
		assert.Contains(t, h.log.String(), "conflicts があるので手当てが要る")
	})
	t.Run("restricted to one column with -from", func(t *testing.T) {
		h := newHarness()
		h.e.hostColumnList = cols
		var got []string
		h.e.hostBatch = func(_ *gorm.DB, col maintenance.HostColumn, from string, _ int, dry bool) (maintenance.HostBackfillResult, error) {
			got = append(got, col.Table+"."+col.Column+"@"+from)
			assert.True(t, dry)
			return maintenance.HostBackfillResult{}, nil
		}
		assert.Equal(t, 0, remoteHost(h.e, []string{"-table", "meta", "-column", "silencedHosts", "-from", "m1", "-dry-run"}))
		assert.Equal(t, []string{"meta.silencedHosts@m1"}, got)
		assert.Contains(t, h.log.String(), "done [dry-run (no writes)]")
		assert.NotContains(t, h.log.String(), "手当てが要る")
	})
	t.Run("rejects -from across several columns", func(t *testing.T) {
		h := newHarness()
		h.e.hostColumnList = cols
		h.e.hostBatch = func(*gorm.DB, maintenance.HostColumn, string, int, bool) (maintenance.HostBackfillResult, error) {
			t.Fatal("must not run")
			return maintenance.HostBackfillResult{}, nil
		}
		assert.Equal(t, 1, remoteHost(h.e, []string{"-table", "meta", "-from", "m1"}))
		assert.Contains(t, h.log.String(), "-from requires -table (and -column when ambiguous); 2 columns matched")
	})
	t.Run("no matching column", func(t *testing.T) {
		h := newHarness()
		h.e.hostColumnList = cols
		assert.Equal(t, 1, remoteHost(h.e, []string{"-table", "nope"}))
		assert.Contains(t, h.log.String(), `no host column matches -table="nope" -column=""`)
	})
	t.Run("cursor that does not advance", func(t *testing.T) {
		h := newHarness()
		h.e.hostColumnList = cols[:1]
		h.e.hostBatch = func(*gorm.DB, maintenance.HostColumn, string, int, bool) (maintenance.HostBackfillResult, error) {
			return maintenance.HostBackfillResult{Scanned: 1, LastKey: ""}, nil
		}
		assert.Equal(t, 1, remoteHost(h.e, nil))
		assert.Contains(t, h.log.String(), "cursor が進まない")
	})
	t.Run("batch error", func(t *testing.T) {
		h := newHarness()
		h.e.hostColumnList = cols[:1]
		h.e.hostBatch = func(*gorm.DB, maintenance.HostColumn, string, int, bool) (maintenance.HostBackfillResult, error) {
			return maintenance.HostBackfillResult{}, errBoom
		}
		assert.Equal(t, 1, remoteHost(h.e, nil))
		assert.Contains(t, h.log.String(), `backfill user.host (cursor=""): boom`)
	})
}

func TestSelectTargets(t *testing.T) {
	all := maintenance.HostColumns
	assert.Len(t, selectTargets(all, "", ""), len(all))
	for _, c := range selectTargets(all, "user", "") {
		assert.Equal(t, "user", c.Table)
	}
	assert.Empty(t, selectTargets(all, "user", "nope"))
}

func TestEmojiSystemFile(t *testing.T) {
	t.Run("dry-run by default", func(t *testing.T) {
		h := newHarness()
		var got maintenance.EmojiSystemFileBackfillOptions
		h.e.emojiBackfill = func(_ context.Context, db *gorm.DB, copier maintenance.SystemFileCopier, opts maintenance.EmojiSystemFileBackfillOptions) (maintenance.EmojiSystemFileBackfillResult, error) {
			assert.Same(t, h.db, db)
			assert.NotNil(t, copier)
			got = opts
			return maintenance.EmojiSystemFileBackfillResult{Scanned: 1, Copied: 1, Entries: []maintenance.EmojiSystemFileEntry{
				{ApplicationID: "a1", EmojiID: "e1", EmojiName: "blob", Outcome: maintenance.EmojiSystemFileCopied},
			}}, nil
		}
		assert.Equal(t, 0, emojiSystemFile(h.e, []string{"-limit", "3"}))
		assert.Equal(t, maintenance.EmojiSystemFileBackfillOptions{Apply: false, Limit: 3}, got)
		assert.Contains(t, h.stderr.String(), "would-copy")
		assert.Contains(t, h.stderr.String(), "done [dry-run (no writes)]")
	})
	t.Run("apply", func(t *testing.T) {
		h := newHarness()
		var got maintenance.EmojiSystemFileBackfillOptions
		h.e.emojiBackfill = func(_ context.Context, _ *gorm.DB, _ maintenance.SystemFileCopier, opts maintenance.EmojiSystemFileBackfillOptions) (maintenance.EmojiSystemFileBackfillResult, error) {
			got = opts
			return maintenance.EmojiSystemFileBackfillResult{}, nil
		}
		assert.Equal(t, 0, emojiSystemFile(h.e, []string{"-apply"}))
		assert.True(t, got.Apply)
		assert.Contains(t, h.stderr.String(), "done [applied]")
	})
	t.Run("-dry-run and -apply together", func(t *testing.T) {
		h := newHarness()
		assert.Equal(t, 1, emojiSystemFile(h.e, []string{"-dry-run", "-apply"}))
		assert.Contains(t, h.log.String(), "-dry-run と -apply は同時に指定できない")
	})
	t.Run("rows that need attention fail the run", func(t *testing.T) {
		h := newHarness()
		h.e.emojiBackfill = func(context.Context, *gorm.DB, maintenance.SystemFileCopier, maintenance.EmojiSystemFileBackfillOptions) (maintenance.EmojiSystemFileBackfillResult, error) {
			return maintenance.EmojiSystemFileBackfillResult{Unrepairable: 1}, nil
		}
		assert.Equal(t, 1, emojiSystemFile(h.e, nil))
		assert.Contains(t, h.stderr.String(), "要対応が 1 件ある")
	})
	t.Run("error still reports progress", func(t *testing.T) {
		h := newHarness()
		h.e.emojiBackfill = func(context.Context, *gorm.DB, maintenance.SystemFileCopier, maintenance.EmojiSystemFileBackfillOptions) (maintenance.EmojiSystemFileBackfillResult, error) {
			return maintenance.EmojiSystemFileBackfillResult{Scanned: 2}, errBoom
		}
		assert.Equal(t, 1, emojiSystemFile(h.e, nil))
		assert.Contains(t, h.stderr.String(), "aborted [dry-run (no writes)]: scanned=2")
		assert.Contains(t, h.log.String(), "backfill: boom")
	})
	t.Run("bad id generator", func(t *testing.T) {
		h := newHarness()
		h.e.loadConfig = func(string) (*config.Config, error) { return &config.Config{ID: "nope"}, nil }
		assert.Equal(t, 1, emojiSystemFile(h.e, nil))
		assert.Contains(t, h.log.String(), `id generator "nope"`)
	})
}

func TestReport_SkipsAlreadyCopiedRows(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, maintenance.EmojiSystemFileBackfillResult{Entries: []maintenance.EmojiSystemFileEntry{
		{ApplicationID: "a1", Outcome: maintenance.EmojiSystemFileAlready},
		{ApplicationID: "a2", Outcome: maintenance.EmojiSystemFileCopied},
	}}, true, nil)
	assert.NotContains(t, buf.String(), "application=a1")
	require.Contains(t, buf.String(), "application=a2")
	assert.Contains(t, buf.String(), "copied ")
}
