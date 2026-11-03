package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"

	"gorm.io/gorm"
)

// ErrRowNotFound is returned by Repository helpers when a lookup did not
// match any row. The chart engine treats this case the same as "no
// records yet" and falls back to interpolation logic, so callers must
// not surface this error to API responses.
var ErrRowNotFound = errors.New("chart: row not found")

// Row is the in-memory representation of a single chart record. The
// Cols map contains both `___<name>` integer columns (int64) and
// `unique_temp___<name>` varchar[] columns ([]string). Callers read
// values via the dot-notation key, which Repository implementations
// translate to/from raw column names.
type Row struct {
	ID    int64
	Date  int64
	Group sql.NullString
	// Cols maps the dot-notation column name to the stored value. Int
	// columns are int64, unique-temp columns are []string. Empty maps
	// are valid for newly-created buckets that have not yet been
	// updated.
	Cols map[string]any
}

// Repository abstracts the SQL access for one chart's `__chart__<name>`
// and `__chart_day__<name>` tables. The interface intentionally exposes
// span as a parameter so a single implementation can serve both spans.
type Repository interface {
	// FindCurrent returns the row with exactly the given timestamp, or
	// ErrRowNotFound when no row exists yet for that bucket.
	FindCurrent(ctx context.Context, span Span, group string, ts int64) (*Row, error)
	// FindLatest returns the most recent row (regardless of timestamp)
	// for the given group, or ErrRowNotFound when the group has never
	// been written. Used by claimCurrentLog to seed accumulate columns.
	FindLatest(ctx context.Context, span Span, group string) (*Row, error)
	// FindBefore returns the most recent row strictly before the given
	// timestamp. Used by GetChart to anchor interpolation when the
	// requested range starts in a gap.
	FindBefore(ctx context.Context, span Span, group string, ts int64) (*Row, error)
	// FindRange returns all rows whose timestamp lies in [gt, lt]
	// inclusive, ordered date DESC. Used by GetChart for the bulk of
	// the result.
	FindRange(ctx context.Context, span Span, group string, gt, lt int64) ([]*Row, error)
	// Insert creates a new row with the given timestamp/group and
	// initial column values. Returns the persisted row including the
	// generated id.
	Insert(ctx context.Context, span Span, group string, ts int64, cols map[string]any) (*Row, error)
	// ApplyDeltas applies the given int deltas (positive or negative)
	// and unique-temp appends to an existing row. Implementations must
	// use server-side arithmetic so concurrent writers do not lose
	// updates. The `setInts` map overrides absolute values for
	// uniqueIncrement / intersection columns where the engine has
	// already computed cardinality.
	ApplyDeltas(ctx context.Context, span Span, id int64, deltas map[string]int64, uniqueAppends map[string][]string, setInts map[string]int64) error
	// SetColumns replaces the integer values of the named columns
	// directly. Used by Tick() to overwrite tickMajor/tickMinor results.
	SetColumns(ctx context.Context, span Span, id int64, cols map[string]int64) error
	// ResetUniqueTempColumns clears the listed unique-temp columns to
	// the empty array for all rows whose timestamp falls in (gt, lt).
	// Used by Clean().
	ResetUniqueTempColumns(ctx context.Context, span Span, gt, lt int64, columns []string) error
}

// gormRepository is the production Repository backed by a *gorm.DB.
// One instance is constructed per chart and embeds the chart's Schema
// so it can translate dot-notation keys into the SQL column names.
type gormRepository struct {
	db     *gorm.DB
	schema Schema
}

// NewRepository builds a Repository for the given Schema. The schema is
// used to know the hour/day table names and to translate column names.
func NewRepository(db *gorm.DB, schema Schema) Repository {
	return &gormRepository{db: db, schema: schema}
}

func (r *gormRepository) tableFor(span Span) string {
	if span == SpanDay {
		return DayTableName(r.schema.Name)
	}
	return HourTableName(r.schema.Name)
}

// scanRow maps a single map[string]any returned by gorm raw query into
// a Row. Integer column values can come back as int64, int32 or
// []byte depending on the driver, so we coerce them defensively.
func (r *gormRepository) scanRow(raw map[string]any) *Row {
	row := &Row{Cols: make(map[string]any, len(raw))}
	for k, v := range raw {
		switch k {
		case "id":
			row.ID = toInt64(v)
		case "date":
			row.Date = toInt64(v)
		case "group":
			if v == nil {
				row.Group = sql.NullString{Valid: false}
			} else {
				row.Group = sql.NullString{Valid: true, String: fmt.Sprint(v)}
			}
		default:
			if name, ok := fromColumnName(k); ok {
				row.Cols[name] = v
				continue
			}
			if strings.HasPrefix(k, uniqueTempPrefix) {
				rest := k[len(uniqueTempPrefix):]
				name := strings.ReplaceAll(rest, columnDelimiter, ".")
				// driver の生の値をそのまま入れてはいけない。pgx は varchar[] を
				// Go の string (`"{u1,u2}"`) で返すので、`[]string` を期待する
				// 読み手 (bakeUniqueAndIntersection) の型アサーションが必ず失敗
				// し、行側の集合が常に空として扱われていた (#2652)。
				row.Cols[name+":unique"] = toStringSlice(v)
			}
		}
	}
	return row
}

// toInt64 coerces a database driver value into int64. Postgres `smallint`
// (Range: chart.RangeSmall) comes back as int16 from pgx, so we MUST handle
// it explicitly — without this the federation / charts API silently returns
// 0 for every smallint column even though the underlying row has real data
// (#421).
//
// The full set covers every signed integer width and a couple of unsigned
// edge cases just in case a future driver hands us those.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	case int:
		return int64(x)
	case uint64:
		return int64(x)
	case uint32:
		return int64(x)
	case uint16:
		return int64(x)
	case uint8:
		return int64(x)
	case uint:
		return int64(x)
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case []byte:
		var n int64
		// 読めない値は 0 として扱う。呼び出し側は chart の列値を読む経路で、
		// 0 と「記録なし」を区別していない。
		if _, err := fmt.Sscan(string(x), &n); err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// findOne runs a parameterised SELECT * query and returns the first row
// or ErrRowNotFound. The callers always sort/filter so this helper does
// not impose ordering.
func (r *gormRepository) findOne(ctx context.Context, span Span, where string, args []any, order string) (*Row, error) {
	table := r.tableFor(span)
	q := fmt.Sprintf(`SELECT * FROM "%s" WHERE %s ORDER BY %s LIMIT 1`, table, where, order)
	var rows []map[string]any
	if err := r.db.WithContext(ctx).Raw(q, args...).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrRowNotFound
	}
	return r.scanRow(rows[0]), nil
}

func (r *gormRepository) FindCurrent(ctx context.Context, span Span, group string, ts int64) (*Row, error) {
	if r.schema.Grouped {
		return r.findOne(ctx, span, `"date" = ? AND "group" = ?`, []any{ts, group}, `"date" DESC`)
	}
	return r.findOne(ctx, span, `"date" = ?`, []any{ts}, `"date" DESC`)
}

func (r *gormRepository) FindLatest(ctx context.Context, span Span, group string) (*Row, error) {
	if r.schema.Grouped {
		return r.findOne(ctx, span, `"group" = ?`, []any{group}, `"date" DESC`)
	}
	return r.findOne(ctx, span, `1 = 1`, nil, `"date" DESC`)
}

func (r *gormRepository) FindBefore(ctx context.Context, span Span, group string, ts int64) (*Row, error) {
	if r.schema.Grouped {
		return r.findOne(ctx, span, `"date" < ? AND "group" = ?`, []any{ts, group}, `"date" DESC`)
	}
	return r.findOne(ctx, span, `"date" < ?`, []any{ts}, `"date" DESC`)
}

func (r *gormRepository) FindRange(ctx context.Context, span Span, group string, gt, lt int64) ([]*Row, error) {
	table := r.tableFor(span)
	var (
		q    string
		args []any
	)
	if r.schema.Grouped {
		q = fmt.Sprintf(`SELECT * FROM "%s" WHERE "date" BETWEEN ? AND ? AND "group" = ? ORDER BY "date" DESC`, table)
		args = []any{gt, lt, group}
	} else {
		q = fmt.Sprintf(`SELECT * FROM "%s" WHERE "date" BETWEEN ? AND ? ORDER BY "date" DESC`, table)
		args = []any{gt, lt}
	}
	var raws []map[string]any
	if err := r.db.WithContext(ctx).Raw(q, args...).Find(&raws).Error; err != nil {
		return nil, err
	}
	out := make([]*Row, len(raws))
	for i, raw := range raws {
		out[i] = r.scanRow(raw)
	}
	return out, nil
}

func (r *gormRepository) Insert(ctx context.Context, span Span, group string, ts int64, cols map[string]any) (*Row, error) {
	table := r.tableFor(span)
	colNames := []string{`"date"`}
	placeholders := []string{"?"}
	args := []any{ts}
	if r.schema.Grouped {
		colNames = append(colNames, `"group"`)
		placeholders = append(placeholders, "?")
		args = append(args, group)
	}
	// 列順を安定させて debugging しやすくする
	keys := make([]string, 0, len(cols))
	for k := range cols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, name := range keys {
		v := cols[name]
		def, ok := r.schema.columnByName(name)
		if !ok {
			continue
		}
		colNames = append(colNames, fmt.Sprintf(`"%s"`, toColumnName(name)))
		placeholders = append(placeholders, "?")
		args = append(args, v)
		if def.UniqueIncrement {
			// uniqueIncrement 列は temp 配列も明示的に空で初期化する
			// (DEFAULT を持っているのでなくても良いが、明示することで Insert
			// 後の Row 状態と DB 状態をズラさない)
			colNames = append(colNames, fmt.Sprintf(`"%s"`, toUniqueTempColumnName(name)))
			placeholders = append(placeholders, "'{}'::varchar[]")
		}
	}
	q := fmt.Sprintf(
		`INSERT INTO "%s" (%s) VALUES (%s) RETURNING *`,
		table, strings.Join(colNames, ","), strings.Join(placeholders, ","),
	)
	var rows []map[string]any
	if err := r.db.WithContext(ctx).Raw(q, args...).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("chart: insert returned no row for %s", table)
	}
	return r.scanRow(rows[0]), nil
}

func (r *gormRepository) ApplyDeltas(ctx context.Context, span Span, id int64, deltas map[string]int64, uniqueAppends map[string][]string, setInts map[string]int64) error {
	if len(deltas) == 0 && len(uniqueAppends) == 0 && len(setInts) == 0 {
		return nil
	}
	table := r.tableFor(span)
	sets := make([]string, 0, len(deltas)+len(uniqueAppends)+len(setInts))
	args := make([]any, 0, len(deltas)+len(uniqueAppends)+len(setInts)+1)
	for _, name := range sortedKeysInt64(deltas) {
		col := toColumnName(name)
		d := deltas[name]
		lo, hi, bounded := r.columnLimits(name)
		switch {
		case !bounded:
			sets = append(sets, fmt.Sprintf(`"%s" = "%s" + ?`, col, col))
			args = append(args, d)
		case d >= 0:
			// **列の型の範囲で頭打ちにする (upstream 2026.9.1 #17931)。** 範囲を
			// 超えると UPDATE 全体が失敗し、同じ行の他の列の記録まで落ちる。
			// 加算の途中で溢れないよう bigint に広げてから丸める。
			sets = append(sets, fmt.Sprintf(`"%s" = LEAST("%s"::bigint + ?, ?)`, col, col))
			args = append(args, d, hi)
		default:
			sets = append(sets, fmt.Sprintf(`"%s" = GREATEST("%s"::bigint + ?, ?)`, col, col))
			args = append(args, d, lo)
		}
	}
	for _, name := range sortedKeysStrings(uniqueAppends) {
		col := toUniqueTempColumnName(name)
		sets = append(sets, fmt.Sprintf(`"%s" = array_cat("%s", ?::varchar[])`, col, col))
		args = append(args, pgArrayLiteral(uniqueAppends[name]))
	}
	for _, name := range sortedKeysInt64(setInts) {
		col := toColumnName(name)
		sets = append(sets, fmt.Sprintf(`"%s" = ?`, col))
		args = append(args, r.clampToColumn(name, setInts[name]))
	}
	args = append(args, id)
	q := fmt.Sprintf(`UPDATE "%s" SET %s WHERE "id" = ?`, table, strings.Join(sets, ","))
	return r.db.WithContext(ctx).Exec(q, args...).Error
}

// columnLimits returns the value range of the column's SQL type. bounded is
// false for bigint columns (and unknown names), which are left unclamped
// like upstream's `range: 'big'`.
func (r *gormRepository) columnLimits(name string) (lo, hi int64, bounded bool) {
	c, ok := r.schema.columnByName(name)
	if !ok {
		return 0, 0, false
	}
	switch c.Range {
	case RangeSmall:
		return math.MinInt16, math.MaxInt16, true
	case RangeMedium:
		return math.MinInt32, math.MaxInt32, true
	default:
		return 0, 0, false
	}
}

// clampToColumn clamps a value written as-is (unique cardinality,
// accumulated copies) into the column's range, as upstream does.
func (r *gormRepository) clampToColumn(name string, v int64) int64 {
	lo, hi, bounded := r.columnLimits(name)
	if !bounded {
		return v
	}
	return min(max(v, lo), hi)
}

func (r *gormRepository) SetColumns(ctx context.Context, span Span, id int64, cols map[string]int64) error {
	if len(cols) == 0 {
		return nil
	}
	table := r.tableFor(span)
	sets := make([]string, 0, len(cols))
	args := make([]any, 0, len(cols)+1)
	for _, name := range sortedKeysInt64(cols) {
		sets = append(sets, fmt.Sprintf(`"%s" = ?`, toColumnName(name)))
		args = append(args, r.clampToColumn(name, cols[name]))
	}
	args = append(args, id)
	q := fmt.Sprintf(`UPDATE "%s" SET %s WHERE "id" = ?`, table, strings.Join(sets, ","))
	return r.db.WithContext(ctx).Exec(q, args...).Error
}

func (r *gormRepository) ResetUniqueTempColumns(ctx context.Context, span Span, gt, lt int64, columns []string) error {
	if len(columns) == 0 {
		return nil
	}
	table := r.tableFor(span)
	sets := make([]string, 0, len(columns))
	for _, name := range columns {
		sets = append(sets, fmt.Sprintf(`"%s" = '{}'::varchar[]`, toUniqueTempColumnName(name)))
	}
	q := fmt.Sprintf(`UPDATE "%s" SET %s WHERE "date" > ? AND "date" < ?`, table, strings.Join(sets, ","))
	return r.db.WithContext(ctx).Exec(q, gt, lt).Error
}

// uniqueTempDecodeWarnOnce keeps the decode warning to one line per process.
//
// scanRow は FindRange が返す行ごと・unique 列ごとに呼ばれる。activeUsers は
// unique 列を 8 本持ち、/api/charts/* の limit は 500 まで通るので、抑制しないと
// **未認証の GET 1 本で最大 4000 行**の warn が出る。1 回出れば気付けるので
// それで足りる。
var uniqueTempDecodeWarnOnce sync.Once

// warnUniqueTempDecode reports that a unique-temp column could not be decoded.
//
// **ここはグローバルの slog を使う** (#2872 で Chart.warnDropped は構築時の
// logger に移したが、こちらは移していない)。理由は 2 つ。パッケージ関数で
// Chart を持たないこと、そして経路が `Repository` 実装の内部
// (scanRow → toStringSlice) にあり、実 DB を叩くときにしか発火しないこと
// (chart のテストは fakeRepo を使う)。
//
// **ticker goroutine から到達はする** (loop → SaveAll → Save → saveGroup →
// claimCurrentLog → FindCurrent → findOne → scanRow)。ここを数えるテストを
// 足すなら、先に Chart と同じ形へ寄せること。
//
// **`uniqueTempDecodeWarnOnce` の張り替えも同時に見直すこと。** テストの
// captureWarnings はこの package 変数を素の代入でリセットするので、ticker が
// 生きたまま実 repo を使うテストを足すと `sync.Once` 値への競合になる。
func warnUniqueTempDecode(reason string, v any) {
	uniqueTempDecodeWarnOnce.Do(func() {
		slog.Warn("chart: cannot decode unique-temp column; unique cardinality will be undercounted",
			"reason", reason, "type", fmt.Sprintf("%T", v))
	})
}

// toStringSlice coerces a driver value for a `varchar[]` column into a string
// slice. pgx (through database/sql) hands back the array literal as a string;
// other drivers may use []byte.
//
// **黙って nil に落とさない。** それが #2652 そのもので、行側の集合が常に空に
// なり、bake が buffer だけの濃度を絶対値で SET し続けて既存の値を壊していた。
// 自己修復しない壊れ方なので、気付けるように warn を出す。
//
// 型が変わる経路だけでなく、**string では受け取れたが配列リテラルとして読めない**
// 経路も同じ壊れ方をする (PostgreSQL 側の出力形式が変わった場合など) ので、
// 両方を拾う。
func toStringSlice(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case string:
		return decodePgTextArray(x, v)
	case []byte:
		return decodePgTextArray(string(x), v)
	default:
		warnUniqueTempDecode("unexpected driver type", v)
		return nil
	}
}

// decodePgTextArray parses the literal and warns when it is not one.
func decodePgTextArray(lit string, raw any) []string {
	out := parsePgTextArray(lit)
	if out == nil {
		warnUniqueTempDecode("not a PostgreSQL array literal", raw)
	}
	return out
}

// parsePgTextArray parses a one-dimensional PostgreSQL text array literal
// (`{}`, `{a,b}`, `{"a,b","c\"d"}`) into its elements. It is the inverse of
// pgArrayLiteral, but must also accept the unquoted form because PostgreSQL
// only quotes elements that need it on output.
//
// SQL NULL の要素 (引用符なしの `NULL`) は落とす。chart の unique-temp 配列に
// NULL は入らないが、入っていたときに空文字列として濃度に数えてしまうより
// 無視する方が安全側。
// 注意: integration test は testutil が PreferSimpleProtocol: true で接続する
// のに対し、本番は extended protocol (pgx の文のキャッシュ、internal/db) で走る。
// varchar[] はどちらでも string で返ってくることを実測で確認しているが、
// テストが本番の protocol を pin しているわけではない。
func parsePgTextArray(s string) []string {
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return []string{}
	}
	out := make([]string, 0, strings.Count(inner, ",")+1)
	var buf strings.Builder
	inQuotes := false
	escaped := false
	quoted := false
	flush := func() {
		v := buf.String()
		buf.Reset()
		// 引用符なしの NULL だけが SQL NULL。`"NULL"` は文字列の NULL。
		if !quoted && v == "NULL" {
			quoted = false
			return
		}
		quoted = false
		out = append(out, v)
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case escaped:
			buf.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
			quoted = true
		case c == ',' && !inQuotes:
			flush()
		default:
			buf.WriteByte(c)
		}
	}
	flush()
	return out
}

// pgArrayLiteral builds a PostgreSQL array literal string for a slice of
// strings. Values are wrapped in double quotes and embedded characters
// are escaped to keep the string parseable. Used by ApplyDeltas to feed
// the unique-temp `array_cat` operands.
func pgArrayLiteral(values []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		// escape backslash and double-quote
		for _, r := range v {
			if r == '\\' || r == '"' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func sortedKeysInt64(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysStrings(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
