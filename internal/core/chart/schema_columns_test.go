package chart_test

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/core/chart/charts"
)

// productionSchemas is every chart wired in internal/server/charts.go.
// TestChartColumnTypesMatchMigrations が、DB にある chart の表がすべてここに
// 含まれることを確かめるので、chart を足して載せ忘れると落ちる。
func productionSchemas() []chart.Schema {
	return []chart.Schema{
		charts.SchemaNotes(),
		charts.SchemaUsers(),
		charts.SchemaDrive(),
		charts.SchemaFederation(),
		charts.SchemaInstance(),
		charts.SchemaApRequest(),
		charts.SchemaActiveUsers(),
		charts.SchemaPerUserNotes(),
		charts.SchemaPerUserDrive(),
		charts.SchemaPerUserFollowing(),
		charts.SchemaPerUserPv(),
		charts.SchemaPerUserReaction(),
	}
}

func requireMigratedDB(t *testing.T) {
	t.Helper()
	if chart.MigratedTestDB() == nil {
		t.Skip("test database not available")
	}
}

// **列の型と Range を突き合わせる (#3873)。** 上限で止める処理は Range から
// 範囲を決めるので、migration で列を広げても Range が small のままだと
// 32767 で止まり続け、逆に Range だけ広げると範囲外の UPDATE が失敗して
// 同じ行の他の列の記録まで落ちる。
func TestChartColumnTypesMatchMigrations(t *testing.T) {
	requireMigratedDB(t)
	db := chart.MigratedTestDB()

	type column struct{ Table, Column, DataType string }
	var cols []column
	require.NoError(t, db.Raw(`
		SELECT table_name AS "table", column_name AS "column", data_type AS "data_type"
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND (left(table_name, 9) = '__chart__' OR left(table_name, 13) = '__chart_day__')
		  AND left(column_name, 3) = '___'`).Scan(&cols).Error)

	actual := map[string]map[string]string{}
	for _, c := range cols {
		if actual[c.Table] == nil {
			actual[c.Table] = map[string]string{}
		}
		actual[c.Table][c.Column] = c.DataType
	}
	// 抽出が空振りしても緑にならないよう、実在する表と列を名指しする。
	require.Equal(t, "integer", actual[chart.HourTableName("instance")]["___requests_received"],
		"the query must find the instance chart's columns")

	seen := map[string]map[string]bool{}
	for _, s := range productionSchemas() {
		for _, table := range []string{chart.HourTableName(s.Name), chart.DayTableName(s.Name)} {
			got, ok := actual[table]
			if !assert.Truef(t, ok, "table %s for chart %q is missing from the migrated DB", table, s.Name) {
				continue
			}
			seen[table] = map[string]bool{}
			for _, c := range s.Columns {
				name := chart.ColumnName(c.Name)
				seen[table][name] = true
				assert.Equalf(t, chart.SQLTypeOf(c.Range), got[name],
					"%s.%s: the column type and the schema's Range disagree", table, name)
			}
		}
	}
	for table, columns := range actual {
		if seen[table] == nil {
			t.Errorf("chart table %s has no schema in productionSchemas", table)
			continue
		}
		for name := range columns {
			assert.Truef(t, seen[table][name], "%s.%s is in the DB but not in the chart's schema", table, name)
		}
	}
}

// **32767 を超えるリクエスト数をそのまま記録する (#3873)。** 本家は smallint で
// 32767 が上限で、大きなサーバーとの間の 1 日の数が頭打ちになる。
func TestInstanceChartRecordsRequestsAboveSmallintRange(t *testing.T) {
	requireMigratedDB(t)
	db := chart.MigratedTestDB()
	repo := chart.NewRepository(db, charts.SchemaInstance())
	ctx := context.Background()
	const host = "requests-above-smallint.example"
	// 前の実行が途中で止まって行が残っていると、("date", "group") の unique
	// index で Insert が落ち続けるので、始める前にも消す。
	cleanup := func() {
		for _, table := range []string{chart.HourTableName("instance"), chart.DayTableName("instance")} {
			require.NoError(t, db.Exec(`DELETE FROM "`+table+`" WHERE "group" = ?`, host).Error)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	const want = int64(math.MaxInt16) + 10000
	for _, span := range []chart.Span{chart.SpanHour, chart.SpanDay} {
		row, err := repo.Insert(ctx, span, host, 1000, map[string]any{})
		require.NoError(t, err)
		require.NoError(t, repo.ApplyDeltas(ctx, span, row.ID, map[string]int64{
			"requests.received":  want,
			"requests.succeeded": want + 1,
			"requests.failed":    want + 2,
		}, nil, nil))

		got, err := repo.FindCurrent(ctx, span, host, 1000)
		require.NoError(t, err)
		assert.EqualValues(t, want, asInt64(t, got.Cols["requests.received"]))
		assert.EqualValues(t, want+1, asInt64(t, got.Cols["requests.succeeded"]))
		assert.EqualValues(t, want+2, asInt64(t, got.Cols["requests.failed"]))
	}
}

func asInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int:
		return int64(x)
	}
	t.Fatalf("unexpected column value %T", v)
	return 0
}
