package driver_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

func TestJobLog(t *testing.T) {
	// ロガーの無い context では何もしない (panic しない)。
	driver.JobLog(context.Background(), "ignored")

	var got []string
	ctx := driver.WithJobLogger(context.Background(), func(_ context.Context, line string) error {
		got = append(got, line)
		return nil
	})
	driver.JobLog(ctx, "one")
	driver.JobLog(ctx, "two")
	assert.Equal(t, []string{"one", "two"}, got)

	// 書けなくても呼び出し側には返さない。
	failing := driver.WithJobLogger(context.Background(), func(context.Context, string) error {
		return errors.New("redis down")
	})
	driver.JobLog(failing, "lost")

	// nil のロガーを載せても何もしない。
	driver.JobLog(driver.WithJobLogger(context.Background(), nil), "ignored")
}
