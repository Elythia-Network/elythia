package cherrypick

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintenance"
	"github.com/elythia-network/elythia/internal/testutil"
)

func testEnv(r maintenance.CherryPickReport, inspectErr error) (env, *bytes.Buffer, *bytes.Buffer, *string) {
	var stdout, stderr bytes.Buffer
	var loaded string
	return env{
		stdout: &stdout,
		stderr: &stderr,
		loadConfig: func(path string) (*config.Config, error) {
			loaded = path
			return &config.Config{}, nil
		},
		inspect: func(*config.Config) (maintenance.CherryPickReport, error) { return r, inspectErr },
	}, &stdout, &stderr, &loaded
}

func TestCheck_NotCherryPickPrintsNothing(t *testing.T) {
	e, stdout, stderr, loaded := testEnv(maintenance.CherryPickReport{Status: maintenance.NotCherryPick}, nil)
	assert.Equal(t, 0, check(e, []string{"-config", "x.yml"}))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
	assert.Equal(t, "x.yml", *loaded)
}

func TestCheck_NothingToDo(t *testing.T) {
	e, stdout, _, loaded := testEnv(maintenance.CherryPickReport{Status: maintenance.CherryPickSupported, NoteHistories: 3}, nil)
	assert.Equal(t, 0, check(e, nil))
	assert.Equal(t, ".config/default.yml", *loaded)
	assert.Contains(t, stdout.String(), "this database came from CherryPick 4.18.0-beta.1")
	assert.Contains(t, stdout.String(), "note edit history                          3")
	assert.Contains(t, stdout.String(), "nothing needs action")
}

func TestCheck_NeedsAction(t *testing.T) {
	reason := `参加したい "です"`
	e, stdout, _, _ := testEnv(maintenance.CherryPickReport{
		Status: maintenance.CherryPickSupported,
		PendingUsers: []maintenance.CherryPickPendingUser{
			{ID: "u1", Username: "carol", SignupReason: &reason},
			{ID: "u2", Username: "dave"},
		},
		AbuseReportResolvers:   2,
		QuarantinedHosts:       []string{"remote.example"},
		ScheduledDeletesDue:    4,
		ScheduledDeletesLater:  5,
		RepositoryURL:          "https://github.com/kokonect-link/cherrypick",
		Events:                 6,
		AutoDeleteUsers:        7,
		DraftScheduledDeletes:  8,
		RemoteScheduledDeletes: 9,
		UnapprovedMembers:      10,
	}, nil)
	assert.Equal(t, ExitNeedsAction, check(e, nil))
	out := stdout.String()
	for _, want := range []string{
		"users waiting for approval (they can sign in on Elythia)  2",
		`@carol  u1  "参加したい \"です\""`,
		"@dave  u2  (no reason)",
		"abuse report resolvers (not applied)                      2",
		"quarantined instances (the restriction is lost)           1",
		"    remote.example",
		"scheduled deletes (not deleted): due                      4",
		"scheduled deletes (not deleted): not yet due              5",
		"repositoryUrl still points at CherryPick                  https://github.com/kokonect-link/cherrypick",
		"events                                     6",
		"users with automatic deletion of old notes 7",
		"drafts with a scheduled delete             8",
		"remote notes with a scheduled delete       9",
		"members without the approval flag          10",
		"5 kind(s) of items need action",
	} {
		assert.Contains(t, out, want)
	}
}

func TestCheck_Unsupported(t *testing.T) {
	e, stdout, _, _ := testEnv(maintenance.CherryPickReport{Status: maintenance.CherryPickUnsupported, Reason: "too old"}, nil)
	assert.Equal(t, ExitNeedsAction, check(e, nil))
	assert.Contains(t, stdout.String(), "other than 4.18.0-beta.1")
	assert.Contains(t, stdout.String(), "too old")
	assert.NotContains(t, stdout.String(), "needs action")
}

func TestCheck_Errors(t *testing.T) {
	t.Run("inspect", func(t *testing.T) {
		e, _, stderr, _ := testEnv(maintenance.CherryPickReport{}, errors.New("boom"))
		assert.Equal(t, 1, check(e, nil))
		assert.Contains(t, stderr.String(), "boom")
	})
	t.Run("config", func(t *testing.T) {
		e, _, stderr, _ := testEnv(maintenance.CherryPickReport{}, nil)
		e.loadConfig = func(string) (*config.Config, error) { return nil, errors.New("no file") }
		assert.Equal(t, 1, check(e, nil))
		assert.Contains(t, stderr.String(), "no file")
	})
	t.Run("flag", func(t *testing.T) {
		e, _, _, _ := testEnv(maintenance.CherryPickReport{}, nil)
		assert.Equal(t, 2, check(e, []string{"-nope"}))
	})
	t.Run("argument", func(t *testing.T) {
		e, _, stderr, _ := testEnv(maintenance.CherryPickReport{}, nil)
		assert.Equal(t, 2, check(e, []string{"extra"}))
		assert.Contains(t, stderr.String(), `unexpected argument "extra"`)
	})
}

// 実 DB に繋いで数える経路。CherryPick 4.18.0-beta.1 の形の schema に承認待ちを 1 人置く。
func TestInspectDSN_AgainstDatabase(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("cherrypick")
	require.NoError(t, err)
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	require.Equal(t, "internal_cli_cherrypick_cherrypick", schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	require.NoError(t, testutil.LoadCherryPickSchema(db))
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved, "signupReason") VALUES ('u1', 'carol', 'carol', false, 'hi')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO meta (id, "approvalRequiredForSignup") VALUES ('x', true)`).Error)

	r, err := inspectDSN(testutil.TestDSNForSchema(schema), time.Now(), maintenance.InspectCherryPick)
	require.NoError(t, err)
	assert.Equal(t, maintenance.CherryPickSupported, r.Status, r.Reason)
	require.Len(t, r.PendingUsers, 1)
	assert.Equal(t, "carol", r.PendingUsers[0].Username)

	_, err = inspectDSN("host=127.0.0.1 port=1 user=x dbname=x sslmode=disable connect_timeout=1", time.Now(), maintenance.InspectCherryPick)
	assert.Error(t, err)
}

// 設定の接続先に繋げなければ、エラーを返す (数えずに 0 件と答えない)。
func TestInspect_UnreachableDatabase(t *testing.T) {
	cfg := &config.Config{}
	cfg.DB.Host = "127.0.0.1"
	cfg.DB.Port = 1
	cfg.DB.User = "x"
	cfg.DB.DB = "x"
	var statusErr *maintenance.CherryPickStatusError
	_, err := Inspect(cfg)
	assert.ErrorAs(t, err, &statusErr)
	_, err = InspectBeforeMigrate(cfg)
	assert.ErrorAs(t, err, &statusErr, "繋げなければ版も確かめられない")
}

func TestCheck_UsesTheRealEnv(t *testing.T) {
	e := defaultEnv()
	assert.NotNil(t, e.stdout)
	assert.NotNil(t, e.loadConfig)
	assert.Equal(t, funcPtr(Inspect), funcPtr(e.inspect))
	assert.Equal(t, 2, Check([]string{"-nope"}))
}

func funcPtr(f any) uintptr { return reflect.ValueOf(f).Pointer() }
