package backup

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// literal の結果は standard_conforming_strings に依らず元の値に戻ること。
//
// 単引用符で囲むだけの形では、off のとき `\'` が引用符の終わりになり、meta.json の
// databaseSettings の値から後ろへ SQL を続けられた。
func TestLiteralIndependentOfStandardConformingStrings(t *testing.T) {
	e := getVerifyEnv(t)
	ctx := context.Background()
	values := []string{
		`plain`,
		`it's`,
		`back\slash`,
		`\'; SELECT 1; --`,
		`trailing\`,
		`\\double`,
	}
	for _, scs := range []string{"on", "off"} {
		// **設定ごとに接続を作り直す。** pgx は文を準備してキャッシュするので、
		// 同じ接続で設定を変えると、前の設定で解析した文が使い回される。
		cfg, err := pgx.ParseConfig(e.url)
		require.NoError(t, err)
		cfg.RuntimeParams["standard_conforming_strings"] = scs
		conn, err := pgx.ConnectConfig(ctx, cfg)
		require.NoError(t, err)
		for _, v := range values {
			var got string
			err := conn.QueryRow(ctx, "SELECT "+literal(v)+"::text").Scan(&got)
			require.NoError(t, err, "scs=%s value=%q literal=%s", scs, v, literal(v))
			assert.Equal(t, v, got, "scs=%s literal=%s", scs, literal(v))
		}
		require.NoError(t, conn.Close(ctx))
	}
	// バックスラッシュを含まない値は、従来どおりの '' の形。
	assert.Equal(t, `'it''s'`, literal(`it's`))
}
