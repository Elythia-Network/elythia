package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

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
