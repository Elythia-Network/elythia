package notesfilter

import (
	"github.com/shiroha-a/mk/internal/misc/idnhost"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// LoadBlockedHosts reads meta.blockedHosts for ApplyBlockedHosts.
//
// **Fail-closed**: meta が読めなければ error を返し、呼び出し側は 500 を返す
// (#1544 と同じ判断)。nil を返して素通しにすると、DB 障害のあいだブロック済み
// インスタンスのノートが一覧に出る。repo 未配線 (テスト) のときだけ no-op。
func LoadBlockedHosts(meta repository.MetaRepository) ([]string, error) {
	if meta == nil {
		return nil, nil
	}
	m, err := meta.Fetch()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return m.BlockedHosts, nil
}

// ApplyBlockedHosts drops notes whose author, reply author, or renote author
// belongs to an admin-blocked instance, mirroring upstream
// QueryService.generateBlockedHostQueryForNote (#1562)。
//
// upstream は meta.blockedHosts を [x, %.x] に展開して NOT ILIKE ALL で突合
// する = exact 一致または subdomain 一致で除外。local user (host IS NULL) は
// 常に通す。blockedHosts が空なら no-op。
func ApplyBlockedHosts(notes []*model.Note, blockedHosts []string) []*model.Note {
	if len(notes) == 0 || len(blockedHosts) == 0 {
		return notes
	}
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if n == nil {
			continue
		}
		if hostBlocked(n.UserHost, blockedHosts) ||
			hostBlocked(n.ReplyUserHost, blockedHosts) ||
			hostBlocked(n.RenoteUserHost, blockedHosts) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// hostBlocked reports whether host matches any blocked entry exactly or as a
// subdomain (`%.entry` 相当)。ILIKE 突合なので case-insensitive。
//
// 判定は inbox / 配送側 (`instance.HostMatchesAny`) と同じ関数に寄せる。
// 別実装のままだと、あちらでポート付き host (`evil.example:8443`) を塞いでも
// 既に取り込まれたノートは一覧から消えない、という片側だけの修正になる。
func hostBlocked(host *string, blockedHosts []string) bool {
	if host == nil {
		return false
	}
	return idnhost.MatchesBlockList(blockedHosts, *host)
}
