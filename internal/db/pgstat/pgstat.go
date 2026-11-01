// Package pgstat makes pg_stat_statements usable by adding it to the
// PostgreSQL configuration only (#3909): when the server preloads the library,
// Elythia creates the extension in its database when it opens the database.
//
// 拡張の作成を migration に入れないのは、(1) superuser でないと作れない
// (pg_stat_statements は trusted でない) ので、superuser でない運営者の起動が
// 止まる、(2) migration は 1 回しか走らないので、後から設定を足しても作られない、
// (3) 本体の migration は全員の DB で走り、性能を調べる道具を望まない運営者の
// DB にも置くことになる、の 3 つの理由による。
package pgstat

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

// Extension is the name of the library and of the extension.
const Extension = "pg_stat_statements"

// CreateSQL is the statement an operator runs as a superuser when Elythia's
// database user may not create the extension. The extension goes to public so
// that a backup's verify, which pre-creates extensions in public, restores it.
const CreateSQL = "CREATE EXTENSION IF NOT EXISTS " + Extension + " WITH SCHEMA public"

// createSQL is the statement Ensure runs: CreateSQL without IF NOT EXISTS.
//
// **IF NOT EXISTS を付けない (#3933)。** 付けると、無いことを確かめた後に別の
// プロセスが先に作り終えていたとき、何もせずに成功で返るので、作っていないのに
// OutcomeCreated と報告してしまう。付けなければ失敗し、読み直しで
// OutcomeInstalled になる。
const createSQL = "CREATE EXTENSION " + Extension + " WITH SCHEMA public"

// loadedSetting is a parameter pg_stat_statements defines only when the server
// loaded it through shared_preload_libraries.
//
// **shared_preload_libraries そのものは読まない。** superuser でも
// pg_read_all_settings の一員でもない利用者は、この設定を読めない。一方、
// pg_stat_statements の設定 (pg_stat_statements.max など) は、ライブラリが
// 起動のときに読み込まれたときにだけ定義され、誰でも pg_settings で見られる。
// 書き方 (引用符、$libdir/、絶対パス、.so) にも左右されない。
const loadedSetting = Extension + ".max"

// State is what the database says about pg_stat_statements.
type State struct {
	// Preloaded reports whether the server loaded the library at startup
	// (shared_preload_libraries). The extension's view only works when it did.
	Preloaded bool
	// Installed reports whether the extension exists in this database.
	Installed bool
}

// Inspect reads the State. It always reads the primary: the extension is
// created there, and a replica may not have caught up yet.
func Inspect(ctx context.Context, db *gorm.DB) (State, error) {
	var st State
	// 問い合わせを 1 つにする。同じ *gorm.DB を続けて使うと、前の問い合わせの
	// エラーが次に持ち越される。
	err := db.WithContext(ctx).Clauses(dbresolver.Write).Raw(`SELECT
	EXISTS (SELECT 1 FROM pg_catalog.pg_settings WHERE name = ?) AS preloaded,
	EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = ?) AS installed`,
		loadedSetting, Extension).Scan(&st).Error
	if err != nil {
		return State{}, fmt.Errorf("pgstat: read the state: %w", err)
	}
	return st, nil
}

// Outcome is what Ensure did.
type Outcome string

const (
	// OutcomeNotPreloaded means the library is not preloaded, so nothing was
	// done. This is the default for operators who did not ask for it.
	OutcomeNotPreloaded Outcome = "not-preloaded"
	// OutcomeInstalled means the extension already existed (or another
	// process created it at the same time).
	OutcomeInstalled Outcome = "installed"
	// OutcomeCreated means Ensure created the extension.
	OutcomeCreated Outcome = "created"
)

// ErrCreate wraps the failure to create the extension, typically because the
// database user is not a superuser. The caller logs CreateSQL for an operator
// to run.
var ErrCreate = errors.New("pgstat: create extension")

// Ensure creates the extension when the server preloads the library and the
// database does not have it yet.
//
// 作れなかったとき (superuser でないなど) は ErrCreate で返す。起動を止める
// かどうかは呼び出し側が決める (止めずに警告を出す)。
func Ensure(ctx context.Context, db *gorm.DB) (Outcome, error) {
	return ensure(ctx, db, nil)
}

// ensure is Ensure with afterInspect, which runs between reading the state
// and creating the extension. Tests use it to let another process create the
// extension in between.
func ensure(ctx context.Context, db *gorm.DB, afterInspect func()) (Outcome, error) {
	st, err := Inspect(ctx, db)
	if err != nil {
		return "", err
	}
	switch {
	case st.Installed:
		return OutcomeInstalled, nil
	case !st.Preloaded:
		return OutcomeNotPreloaded, nil
	}
	if afterInspect != nil {
		afterInspect()
	}
	if err := db.WithContext(ctx).Clauses(dbresolver.Write).Exec(createSQL).Error; err != nil {
		// **同時に起動した別のプロセスが作ったなら、失敗ではない。** 先に作り
		// 終えていれば「既にある」で、同時に作っていれば pg_extension の一意制約で
		// 落ちる。読み直して拡張があれば、作られているので警告しない。
		if again, ierr := Inspect(ctx, db); ierr == nil && again.Installed {
			return OutcomeInstalled, nil
		}
		return "", fmt.Errorf("%w: %w", ErrCreate, err)
	}
	return OutcomeCreated, nil
}
