package server

import (
	apicharts "github.com/elythia-network/elythia/internal/api/charts"
	"github.com/elythia-network/elythia/internal/charttick"
	"github.com/elythia-network/elythia/internal/core/chart"
	corechartcharts "github.com/elythia-network/elythia/internal/core/chart/charts"
	"gorm.io/gorm"
)

// buildChartBundle constructs every chart engine the server needs and
// returns them packaged for the API handler. Each engine gets its own
// gormRepository instance bound to its specific schema; lock and clock
// are shared singletons. Charts whose upstream tickMajor/tickMinor
// performs an absolute count get a TickFunc closure (see
// internal/charttick) so the periodic chart cron can correct
// event-driven drift (#151).
func buildChartBundle(db *gorm.DB) apicharts.Charts {
	locker := chart.NewMemoryLocker()
	mk := func(schema chart.Schema, tick chart.TickFunc) *chart.Chart {
		c, err := chart.New(chart.Config{
			Schema: schema,
			Repo:   chart.NewRepository(db, schema),
			Lock:   locker,
			Tick:   tick,
		})
		if err != nil {
			// Schema 不正は起動時に検出したいので panic で弾く。
			// 各 SchemaXxx() は定数を返すので実運用ではここに来ない。
			panic(err)
		}
		return c
	}
	return apicharts.Charts{
		Notes:            mk(corechartcharts.SchemaNotes(), charttick.Notes(db)),
		Users:            mk(corechartcharts.SchemaUsers(), charttick.Users(db)),
		Drive:            mk(corechartcharts.SchemaDrive(), nil),
		Federation:       mk(corechartcharts.SchemaFederation(), charttick.Federation(db)),
		Instance:         mk(corechartcharts.SchemaInstance(), nil),
		ApRequest:        mk(corechartcharts.SchemaApRequest(), nil),
		ActiveUsers:      mk(corechartcharts.SchemaActiveUsers(), nil),
		PerUserNotes:     mk(corechartcharts.SchemaPerUserNotes(), nil),
		PerUserDrive:     mk(corechartcharts.SchemaPerUserDrive(), nil),
		PerUserFollowing: mk(corechartcharts.SchemaPerUserFollowing(), nil),
		PerUserPv:        mk(corechartcharts.SchemaPerUserPv(), nil),
		PerUserReaction:  mk(corechartcharts.SchemaPerUserReaction(), nil),
	}
}
