package notesfilter

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
)

func strPtr(s string) *string { return &s }

func TestApplyBlockedHosts_EmptyListIsNoop(t *testing.T) {
	notes := []*model.Note{{ID: "n1", UserHost: strPtr("blocked.example")}}
	assert.Len(t, ApplyBlockedHosts(notes, nil), 1)
	assert.Len(t, ApplyBlockedHosts(notes, []string{}), 1)
}

func TestApplyBlockedHosts_DropsAuthorHost(t *testing.T) {
	notes := []*model.Note{
		{ID: "n1", UserHost: strPtr("blocked.example")},
		{ID: "n2", UserHost: strPtr("ok.example")},
		{ID: "n3"}, // local note (host nil) は常に通す
	}
	out := ApplyBlockedHosts(notes, []string{"blocked.example"})
	assert.Len(t, out, 2)
	assert.Equal(t, "n2", out[0].ID)
	assert.Equal(t, "n3", out[1].ID)
}

func TestApplyBlockedHosts_SubdomainMatch(t *testing.T) {
	// upstream は [x, %.x] の ILIKE 突合 = subdomain も block 対象
	notes := []*model.Note{
		{ID: "n1", UserHost: strPtr("sub.blocked.example")},
		{ID: "n2", UserHost: strPtr("notblocked.example")},
	}
	out := ApplyBlockedHosts(notes, []string{"blocked.example"})
	assert.Len(t, out, 1)
	assert.Equal(t, "n2", out[0].ID, "suffix一致は `.` 境界でのみ成立する (notblocked.example は残る)")
}

// 非既定ポートの host (`user.host` はそれを保持する) で投稿しても一覧から
// 除外される。inbox 側の判定と同じ関数を使っていることの確認でもある。
func TestApplyBlockedHosts_IgnoresPort(t *testing.T) {
	notes := []*model.Note{
		{ID: "n1", UserHost: strPtr("blocked.example:8443")},
		{ID: "n2", ReplyUserHost: strPtr("sub.blocked.example:8443")},
		{ID: "n3", RenoteUserHost: strPtr("blocked.example.")},
		{ID: "n4", UserHost: strPtr("notblocked.example:8443")},
	}
	out := ApplyBlockedHosts(notes, []string{"blocked.example"})
	assert.Len(t, out, 1)
	assert.Equal(t, "n4", out[0].ID)
}

func TestApplyBlockedHosts_CaseInsensitive(t *testing.T) {
	notes := []*model.Note{{ID: "n1", UserHost: strPtr("Blocked.Example")}}
	assert.Empty(t, ApplyBlockedHosts(notes, []string{"blocked.example"}))
}

func TestApplyBlockedHosts_ReplyAndRenoteAuthors(t *testing.T) {
	notes := []*model.Note{
		{ID: "n1", ReplyUserHost: strPtr("blocked.example")},
		{ID: "n2", RenoteUserHost: strPtr("blocked.example")},
		{ID: "n3", UserHost: strPtr("ok.example")},
	}
	out := ApplyBlockedHosts(notes, []string{"blocked.example"})
	assert.Len(t, out, 1)
	assert.Equal(t, "n3", out[0].ID)
}

func TestApplyBlockedHosts_NilNoteSkipped(t *testing.T) {
	notes := []*model.Note{nil, {ID: "n1"}}
	out := ApplyBlockedHosts(notes, []string{"blocked.example"})
	assert.Len(t, out, 1)
}

func TestApplyBlockedHosts_EmptyBlockedEntryIgnored(t *testing.T) {
	// 空文字 entry が全 host を block しないこと
	notes := []*model.Note{{ID: "n1", UserHost: strPtr("ok.example")}}
	out := ApplyBlockedHosts(notes, []string{""})
	assert.Len(t, out, 1)
}
