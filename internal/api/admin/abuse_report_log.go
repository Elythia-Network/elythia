package admin

import "github.com/elythia-network/elythia/internal/model"

// abuseReportLogRow returns a copy of r that carries only the
// abuse_user_report columns, for the `report` field of the
// resolveAbuseReport / forwardAbuseReport / updateAbuseReportNote moderation
// logs.
//
// 本家 AbuseReportService は関連を読み込まずに引いた行 (findBy /
// findOneByOrFail) をそのまま載せるので、ログの report は通報の列だけになる。
// FindByID は TargetUser / Reporter / Assignee を Preload するため、そのまま
// 載せると利用者の行がまるごと入る (#3267)。写しを返すのは、更新前の行を
// 控えるためでもある (モックは UpdateFields で同じポインタを書き換える)。
func abuseReportLogRow(r *model.AbuseUserReport) *model.AbuseUserReport {
	row := *r
	row.TargetUser = nil
	row.Reporter = nil
	row.Assignee = nil
	return &row
}
