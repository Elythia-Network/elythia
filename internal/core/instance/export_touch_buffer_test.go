package instance

// SetTouchBufferTimerHook sets the function called each time the flush timer
// of b fires. Call it before Start.
func SetTouchBufferTimerHook(b *TouchBuffer, fn func()) {
	b.onTimer = fn
}

// DrainTouchBufferArm discards a pending arm signal of b.
func DrainTouchBufferArm(b *TouchBuffer) {
	select {
	case <-b.armCh:
	default:
	}
}
