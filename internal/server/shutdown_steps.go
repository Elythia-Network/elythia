package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// shutdownStep is one named step of stopping a generation (shutdownSteps)
// or of rolling back a failed construction (cleanupSteps) (#3711).
type shutdownStep struct {
	// name identifies the step in the tables, the logs and the errors. It is
	// unique within one table.
	name string
	// run does the step with the ctx of the stop. It returns the error the
	// step reports (only the lifecycle list and echo report one) or nil.
	run func(ctx context.Context) error
}

// errShutdownStep is wrapped by the error of a shutdown step that panicked.
var errShutdownStep = errors.New("server: a shutdown step failed")

// runShutdownSteps runs steps in order, passing ctx to each.
//
// When a step panics, runShutdownSteps recovers it, logs the step's name, the
// panic value and the stack, and goes on with the next step (#3711). It
// returns nil when no step failed, the error itself when exactly one step
// failed, and otherwise the errors.Join of the errors in step order. The
// error of a step that panicked wraps errShutdownStep and names the step; a
// step that returned an error contributes that error.
//
// 失敗が1つのときに包まずに返すのは、panicが無い停止の戻り値(echoのShutdownの
// エラー)を、段の表にする前(#3711より前)と同じ値のままにするため。
func runShutdownSteps(ctx context.Context, steps []shutdownStep) error {
	var errs []error
	for _, st := range steps {
		if err := runShutdownStep(ctx, st); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// runShutdownStep runs st with ctx, turning a panic into an error after
// logging it with the stack.
//
// スタックはrecoverの中でしか取れないので、ここで記録する(stopPartと同じ形。#3707)。
func runShutdownStep(ctx context.Context, st shutdownStep) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("server: a shutdown step panicked; running the rest",
				"step", st.name, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: step %q panicked: %v", errShutdownStep, st.name, r)
		}
	}()
	return st.run(ctx)
}
