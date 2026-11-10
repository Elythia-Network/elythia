package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a not-found", nil, false},
		{"gorm sentinel", gorm.ErrRecordNotFound, true},
		{"our alias", ErrNotFound, true},
		// **ラップされていても効く。** repository が
		// `fmt.Errorf("...: %w", err)` を挟むことがある。
		{"wrapped", fmt.Errorf("find note: %w", gorm.ErrRecordNotFound), true},
		// **DB 障害は not-found ではない。** ここが true になると
		// 接続断が「そんなノートは無い」に化ける (#2792)。
		{"connection failure", errors.New("dial tcp: connection refused"), false},
		{"invalid syntax", gorm.ErrInvalidField, false},
		// GORM 以外の経路に変わっても not-found を取りこぼさない保険。
		{"database/sql sentinel", sql.ErrNoRows, true},
		{"wrapped sql sentinel", fmt.Errorf("scan: %w", sql.ErrNoRows), true},
		{"duplicated key", gorm.ErrDuplicatedKey, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsNotFound(tt.err))
		})
	}
}

func TestIsRejectedValue(t *testing.T) {
	assert.False(t, IsRejectedValue(nil))
	assert.False(t, IsRejectedValue(errors.New("x")))
	assert.False(t, IsRejectedValue(&pgconn.PgError{Code: "23505"}), "一意制約の違反は別物")
	assert.False(t, IsRejectedValue(&pgconn.PgError{Code: "23503"}), "外部キーの違反は値の形の誤りではない")
	assert.False(t, IsRejectedValue(&pgconn.PgError{Code: "57014"}), "取り消された問い合わせは 500 のまま")
	for _, code := range []string{"22001", "22003", "22008", "22021", "22P05"} {
		assert.True(t, IsRejectedValue(fmt.Errorf("wrap: %w", &pgconn.PgError{Code: code})), code)
	}

	// 実 DB が利用者の値を断る形 (GORM が返す形) を判定できる (#4120)。NUL は、テストの
	// 接続 (simple protocol) では 08P01 になり、本番の pgx の extended protocol での
	// 22021 にならないので、上の値で見る (docs/gates.md の nulparam-check)。08P01 は
	// ほかの原因でも出るので、判定には入れない
	u := insertTestUser(t, "u_rejv", "rejvuser")
	defer cleanupUser(t, u.ID)
	target := insertTestUser(t, "u_rejv2", "rejvtarget")
	defer cleanupUser(t, target.ID)
	repo := NewUserMemoRepository(testDB)
	long := model.UserMemo{ID: "m_rejv1", UserID: u.ID, TargetUserID: target.ID, Memo: strings.Repeat("a", 2049)}
	err := repo.CreateOrUpdate(&long)
	require.Error(t, err)
	assert.True(t, IsRejectedValue(err), "too long: %v", err)
	// 外部キーの違反は判定しない (500 のまま)
	missing := model.UserMemo{ID: "m_rejv3", UserID: u.ID, TargetUserID: "u_rejv_missing", Memo: "x"}
	err = repo.CreateOrUpdate(&missing)
	require.Error(t, err)
	assert.False(t, IsRejectedValue(err), "no such user: %v", err)
}

func TestIsUniqueViolation(t *testing.T) {
	assert.False(t, IsUniqueViolation(nil))
	assert.False(t, IsUniqueViolation(errors.New("x")))
	assert.False(t, IsUniqueViolation(&pgconn.PgError{Code: "23503"}), "FK 違反は別物")
	assert.True(t, IsUniqueViolation(&pgconn.PgError{Code: "23505"}))
	assert.True(t, IsUniqueViolation(fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "23505"})))

	// 実 DB の一意制約違反 (GORM が返す形) も判定できる。
	u := insertTestUser(t, "u_uniqv", "uniqvuser")
	defer cleanupUser(t, u.ID)
	dup := *u
	dup.ID = "u_uniqv2"
	err := NewUserRepository(testDB).Create(&dup)
	require.Error(t, err)
	assert.True(t, IsUniqueViolation(err), "%v", err)
}
