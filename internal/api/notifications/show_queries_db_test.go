package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

var (
	showQueriesDBOnce sync.Once
	showQueriesDB     *gorm.DB
)

func openShowQueriesDB(t *testing.T) *gorm.DB {
	t.Helper()
	showQueriesDBOnce.Do(func() {
		showQueriesDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(showQueriesDB)
	})
	return showQueriesDB
}

// stmtLog records every statement sent through a stmtLogPool, including the
// BEGIN / COMMIT / ROLLBACK of GORM's default write transaction.
type stmtLog struct {
	mu    sync.Mutex
	stmts []string
}

func (l *stmtLog) add(s string) {
	l.mu.Lock()
	l.stmts = append(l.stmts, strings.Join(strings.Fields(s), " "))
	l.mu.Unlock()
}

func (l *stmtLog) reset() {
	l.mu.Lock()
	l.stmts = nil
	l.mu.Unlock()
}

func (l *stmtLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.stmts...)
}

// stmtLogPool is a gorm.ConnPool over a *sql.DB that logs each statement.
//
// GORM の callback では BEGIN / COMMIT が数えられないので、接続の層で数える。
// ConnPoolBeginner を実装すると、GORM は既定のトランザクションもここから始める。
type stmtLogPool struct {
	db  *sql.DB
	log *stmtLog
}

func (p *stmtLogPool) PrepareContext(ctx context.Context, q string) (*sql.Stmt, error) {
	p.log.add(q)
	return p.db.PrepareContext(ctx, q)
}

func (p *stmtLogPool) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	p.log.add(q)
	return p.db.ExecContext(ctx, q, args...)
}

func (p *stmtLogPool) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	p.log.add(q)
	return p.db.QueryContext(ctx, q, args...)
}

func (p *stmtLogPool) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	p.log.add(q)
	return p.db.QueryRowContext(ctx, q, args...)
}

func (p *stmtLogPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	p.log.add("BEGIN")
	tx, err := p.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &stmtLogTx{tx: tx, log: p.log}, nil
}

type stmtLogTx struct {
	tx  *sql.Tx
	log *stmtLog
}

func (t *stmtLogTx) PrepareContext(ctx context.Context, q string) (*sql.Stmt, error) {
	t.log.add(q)
	return t.tx.PrepareContext(ctx, q)
}

func (t *stmtLogTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	t.log.add(q)
	return t.tx.ExecContext(ctx, q, args...)
}

func (t *stmtLogTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	t.log.add(q)
	return t.tx.QueryContext(ctx, q, args...)
}

func (t *stmtLogTx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	t.log.add(q)
	return t.tx.QueryRowContext(ctx, q, args...)
}

func (t *stmtLogTx) Commit() error {
	t.log.add("COMMIT")
	return t.tx.Commit()
}

func (t *stmtLogTx) Rollback() error {
	t.log.add("ROLLBACK")
	return t.tx.Rollback()
}

// loggingDB returns a GORM handle on base's connections (same search_path)
// whose statements are recorded in the returned log.
func loggingDB(t *testing.T, base *gorm.DB) (*gorm.DB, *stmtLog) {
	t.Helper()
	sqlDB, err := base.DB()
	require.NoError(t, err)
	log := &stmtLog{}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: &stmtLogPool{db: sqlDB, log: log}}), &gorm.Config{
		Logger:                   logger.Default.LogMode(logger.Silent),
		DisableNestedTransaction: true,
	})
	require.NoError(t, err)
	return db, log
}

// showQueriesEnv is a notifications Handler on the real repositories, wired
// the way the router wires them (CachedUserRepository, RelationCache, the
// note_unread repository on the notification service).
type showQueriesEnv struct {
	h           *Handler
	svc         *notification.Service
	log         *stmtLog
	viewer      *model.User
	mutedID     string
	notifierB   string
	specifiedID string
	muting      repository.MutingRepository
	noteUnread  repository.NoteUnreadRepository
}

func newShowQueriesEnv(t *testing.T, prefix string) *showQueriesEnv {
	t.Helper()
	base := openShowQueriesDB(t)
	cleanup := func() {
		base.Exec(`DELETE FROM "note" WHERE "userId" LIKE ?`, prefix+"%")
		base.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	mkUser := func(name string) string {
		uid := prefix + name
		require.NoError(t, base.Create(&model.User{ID: uid, Username: uid, UsernameLower: uid, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
		return uid
	}
	viewerID := mkUser("v")
	a, b, c, muted := mkUser("a"), mkUser("b"), mkUser("c"), mkUser("m")
	require.NoError(t, base.Create(&model.UserProfile{UserID: viewerID, Fields: datatypes.JSON([]byte("[]")), MutedInstances: datatypes.JSON([]byte("[]"))}).Error)

	now := time.Now()
	nid := func(i int) string { return idGen.Generate(now.Add(time.Duration(i-100) * time.Second)) }
	own1, own2, reply, mention, specified := nid(1), nid(2), nid(3), nid(4), nid(5)
	mkNote := func(n *model.Note) {
		t.Helper()
		require.NoError(t, base.Create(n).Error)
	}
	mkNote(&model.Note{ID: own1, UserID: viewerID, Text: strPtr("own 1"), Visibility: "public"})
	mkNote(&model.Note{ID: own2, UserID: viewerID, Text: strPtr("own 2"), Visibility: "public"})
	mkNote(&model.Note{ID: reply, UserID: c, Text: strPtr("reply"), Visibility: "public", ReplyID: &own1, ReplyUserID: &viewerID})
	mkNote(&model.Note{ID: mention, UserID: a, Text: strPtr("@v hi"), Visibility: "public", Mentions: []string{viewerID}})
	mkNote(&model.Note{ID: specified, UserID: b, Text: strPtr("dm"), Visibility: "specified", VisibleUserIDs: []string{viewerID}})

	db, log := loggingDB(t, base)
	rc := repository.NewRelationCacheWithTTL(db, time.Hour)
	userRepo := repository.NewCachedUserRepository(repository.NewUserRepositoryWithRelationCache(db, rc))
	noteRepo := repository.NewNoteRepository(db)
	mutingRepo := repository.NewMutingRepositoryWithRelationCache(db, rc)
	noteUnreadRepo := repository.NewNoteUnreadRepository(db)
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: prefix + "mu", MuterID: viewerID, MuteeID: muted}))
	for _, row := range []*model.NoteUnread{
		{ID: prefix + "u1", UserID: viewerID, NoteID: mention, NoteUserID: a, IsMentioned: true},
		{ID: prefix + "u2", UserID: viewerID, NoteID: specified, NoteUserID: b, IsSpecified: true},
	} {
		require.NoError(t, noteUnreadRepo.Upsert(row))
	}

	h, svc := newTestHandler(t)
	svc.SetNoteUnreadRepo(noteUnreadRepo)
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, repository.NewFollowingRepositoryWithRelationCache(db, rc)))
	h.SetMutingRepo(mutingRepo)
	h.SetRelationCache(rc)

	ctx := context.Background()
	for _, in := range []notification.CreateInput{
		{NotifierID: a, Type: notification.TypeReaction, NoteID: own1, Reaction: ":ok@.:"},
		{NotifierID: b, Type: notification.TypeReaction, NoteID: own2, Reaction: ":ok@.:"},
		{NotifierID: c, Type: notification.TypeReply, NoteID: reply},
		{NotifierID: a, Type: notification.TypeMention, NoteID: mention},
		{NotifierID: b, Type: notification.TypeFollow},
		{NotifierID: muted, Type: notification.TypeFollow},
	} {
		in.NotifieeID = viewerID
		_, err := svc.Create(ctx, in)
		require.NoError(t, err)
	}

	viewer, err := userRepo.FindByID(viewerID)
	require.NoError(t, err)
	return &showQueriesEnv{h: h, svc: svc, log: log, viewer: viewer, mutedID: muted, notifierB: b,
		specifiedID: specified, muting: mutingRepo, noteUnread: noteUnreadRepo}
}

func (e *showQueriesEnv) show(t *testing.T) []map[string]any {
	t.Helper()
	c, rec := newJSONRequest(t, "/api/i/notifications", `{}`)
	setAuth(c, e.viewer)
	require.NoError(t, e.h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// One i/notifications request on a warm process sends exactly these
// statements (#4273). 直す前は 8 本 (送り手と著者の `user` の IN が 2 本、
// ミュートの一覧、BEGIN・DELETE・COMMIT) だった。
func TestShow_StatementsPerRequest(t *testing.T) {
	e := newShowQueriesEnv(t, "sq4273a")
	// 1 回目は RelationCache と CachedUserRepository の profile を温める。
	first := e.show(t)
	require.Len(t, first, 5, "the muted notifier's follow must be dropped")

	e.log.reset()
	resp := e.show(t)
	stmts := e.log.snapshot()
	require.Len(t, resp, 5)
	for _, n := range resp {
		assert.NotEqual(t, e.mutedID, n["userId"])
		assert.NotNil(t, n["user"], "notifier must be packed")
		if note, ok := n["note"].(map[string]any); ok {
			assert.NotNil(t, note["user"], "note author must be packed")
			if reply, ok := note["reply"].(map[string]any); ok {
				assert.NotNil(t, reply["user"], "reply target author must be packed")
			}
		}
	}
	// 送り手と閲覧者の `user` を 1 本、通知のノートを 1 本、返信先を 1 本、
	// note_unread の DELETE を 1 本 (トランザクションで包まない)。ミュートの
	// 一覧は RelationCache から読むので出ない。
	assert.Equal(t, []string{
		`SELECT * FROM "user" WHERE id IN ($1,$2,$3,$4,$5)`,
		`SELECT * FROM "note" WHERE id IN ($1,$2,$3,$4)`,
		`SELECT * FROM "note" WHERE id IN ($1)`,
		`DELETE FROM "note_unread" WHERE "userId" = $1`,
	}, stmts)
}

// The implicit read still resets hasUnreadSpecifiedNotes / hasUnreadMentions,
// also when the read marker does not move (#4273): note_unread rows are made
// without a notification, so the DELETE cannot be skipped on the marker.
func TestShow_ImplicitReadClearsUnreadFlags(t *testing.T) {
	e := newShowQueriesEnv(t, "sq4273b")
	ctx := context.Background()
	mentionTypes := []notification.Type{notification.TypeMention, notification.TypeReply}

	before, err := e.svc.UnreadSummary(ctx, e.viewer.ID, mentionTypes)
	require.NoError(t, err)
	require.True(t, before.HasSpecifiedNote)
	require.True(t, before.HasMentions)

	e.show(t)
	after, err := e.svc.UnreadSummary(ctx, e.viewer.ID, mentionTypes)
	require.NoError(t, err)
	assert.False(t, after.HasSpecifiedNote, "hasUnreadSpecifiedNotes must be reset")
	assert.False(t, after.HasMentions, "hasUnreadMentions must be reset")

	// 通知を作らずに note_unread だけが増えた (ミュートしている相手からの宛先
	// 指定の投稿など)。既読位置は進まないが、行は消えなければならない。
	require.NoError(t, e.noteUnread.Upsert(&model.NoteUnread{
		ID: "sq4273bu3", UserID: e.viewer.ID, NoteID: e.specifiedID, NoteUserID: e.mutedID, IsSpecified: true,
	}))
	again, err := e.svc.UnreadSummary(ctx, e.viewer.ID, mentionTypes)
	require.NoError(t, err)
	require.True(t, again.HasSpecifiedNote)

	e.show(t)
	after, err = e.svc.UnreadSummary(ctx, e.viewer.ID, mentionTypes)
	require.NoError(t, err)
	assert.False(t, after.HasSpecifiedNote, "rows without a notification must be cleared too")
}

// A mute made after the relation cache was filled applies to the next request
// (#4273: the muting set is read from RelationCache, which the muting
// repository invalidates on write).
func TestShow_MuteAfterCacheWarmDropsNotifier(t *testing.T) {
	e := newShowQueriesEnv(t, "sq4273c")
	require.Len(t, e.show(t), 5)

	require.NoError(t, e.muting.Create(&model.Muting{ID: "sq4273cmu2", MuterID: e.viewer.ID, MuteeID: e.notifierB}))
	resp := e.show(t)
	for _, n := range resp {
		assert.NotEqual(t, e.notifierB, n["userId"], "a notifier muted after the cache was filled must be dropped")
	}
	assert.Len(t, resp, 3, "the reaction and the follow of b must be dropped")
}
