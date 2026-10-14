package federation_test

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `abuse_user_report.comment` は varchar(2048)。通報本文も URI 一覧も相手が
// 自由に決められるので、溢れると Create が落ちて **inbox job が retry を
// 使い切る = 通報が届かない**。本文なので切って NUL を落とす (#2726)。
func TestProcess_Flag_TruncatesComment(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": ["https://example.com/users/bob"],
		"content": "` + strings.Repeat("あ", 3000) + `"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, abuseRepo.Reports, 1)
	for _, r := range abuseRepo.Reports {
		// **全角で数える。** byte で切る実装なら 2048 にならない。
		assert.Equal(t, 2048, len([]rune(r.Comment)))
	}
}

// NUL 入りの通報本文でも保存できること。SQLSTATE 22021 は長さと無関係に落ちる。
func TestProcess_Flag_StripsNULFromComment(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": ["https://example.com/users/bob"],
		"content": "sp\u0000am"
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, abuseRepo.Reports, 1)
	for _, r := range abuseRepo.Reports {
		assert.Equal(t, "spam\n[\"https://example.com/users/bob\"]", r.Comment)
	}
}

// object の読めない / 空の Flag は送り直しても直らないので、error にせず (inbox の
// job を 8 回回さず) 受け取って捨てること。通報は作らない。
func TestProcess_Flag_UnreadableObjectIsDropped(t *testing.T) {
	for _, obj := range []string{`123`, `[]`} {
		p, repo, _ := newProcessorWithBlocking(t)
		abuseRepo := testutil.NewMockAbuseReportRepository()
		idGenFlag, _ := id.NewGenerator("aidx")
		p.SetAbuseReportRepo(abuseRepo, idGenFlag)
		repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
		body := []byte(`{"type":"Flag","actor":"https://remote.example/users/alice","object":` + obj + `,"content":"x"}`)
		require.NoError(t, p.Process(body), obj)
		assert.Empty(t, abuseRepo.Reports, obj)
	}
}

// object の無い Undo / Reject は送り直しても直らないので、受け取って捨てること
// (error で返すと inbox の job が再試行を使い切ってから failed に積まれる)。
func TestProcess_UndoRejectWithoutObjectAreDropped(t *testing.T) {
	for _, typ := range []string{"Undo", "Reject"} {
		p, _, _ := newProcessorWithBlocking(t)
		body := []byte(`{"type":"` + typ + `","actor":"https://remote.example/users/alice"}`)
		assert.NoError(t, p.Process(body), typ)
	}
}
