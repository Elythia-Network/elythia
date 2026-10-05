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
