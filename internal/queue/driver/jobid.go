package driver

import "context"

type jobIDKey struct{}

// WithJobID returns a context that carries the ID of the running job. Drivers
// call it before invoking the handler.
func WithJobID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, jobIDKey{}, id)
}

// JobID returns the ID of the running job, or "" when the driver did not
// pass one (tests and other drivers).
//
// 同じジョブの再試行 (ワーカーが落ちて取り直したときなど) は同じ ID で
// 走るので、処理中の印を「どのジョブが置いたか」で見分けるのに使う。
func JobID(ctx context.Context) string {
	id, _ := ctx.Value(jobIDKey{}).(string)
	return id
}
