package driver

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRawTask(t *testing.T) {
	body := []byte("hello")
	rt := RawTask{TypeName: "x", Body: body}
	if rt.Type() != "x" {
		t.Fatalf("Type: got %q", rt.Type())
	}
	if string(rt.Payload()) != "hello" {
		t.Fatalf("Payload: got %q", string(rt.Payload()))
	}
}

func TestSkipRetryIdentity(t *testing.T) {
	wrapped := fmt.Errorf("decode: %w: %w", errors.New("boom"), ErrSkipRetry)
	if !errors.Is(wrapped, ErrSkipRetry) {
		t.Fatalf("errors.Is should match ErrSkipRetry")
	}
}

func TestApplyEnqueueOptions(t *testing.T) {
	got := ApplyEnqueueOptions([]EnqueueOption{
		WithQueue("deliver"),
		WithMaxRetry(3),
		WithUnique(time.Hour),
		WithProcessIn(5 * time.Second),
		WithKeepFailed(500),
		WithKeepCompleted(30),
		WithKeepCompletedAge(7 * 24 * time.Hour),
		WithKeepFailedAge(7 * 24 * time.Hour),
	})
	want := EnqueueOptions{
		Queue:            "deliver",
		MaxRetry:         3,
		MaxRetrySet:      true,
		UniqueTTL:        time.Hour,
		ProcessIn:        5 * time.Second,
		KeepFailed:       500,
		KeepFailedSet:    true,
		KeepCompleted:    30,
		KeepCompletedSet: true,
		KeepCompletedAge: 7 * 24 * time.Hour,
		KeepFailedAge:    7 * 24 * time.Hour,
	}
	if got != want {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestApplyEnqueueOptionsZeroSentinel(t *testing.T) {
	// MaxRetrySet must distinguish "explicit 0" from "default" so
	// drivers can decide between "use default" vs "no retries".
	got := ApplyEnqueueOptions([]EnqueueOption{WithMaxRetry(0)})
	if got.MaxRetry != 0 || !got.MaxRetrySet {
		t.Fatalf("explicit zero not recorded: %#v", got)
	}

	def := ApplyEnqueueOptions(nil)
	if def.MaxRetrySet {
		t.Fatalf("default options must not record MaxRetrySet")
	}
}

// TestApplyEnqueueOptions_KeepFailedZeroSentinel: WithKeepFailed(0) は
// 「operator が明示的に retention 解除 (= unlimited 蓄積)」を表す。
// 未指定 (= default) と区別するため KeepFailedSet を使う (#1184)。
func TestApplyEnqueueOptions_KeepFailedZeroSentinel(t *testing.T) {
	got := ApplyEnqueueOptions([]EnqueueOption{WithKeepFailed(0)})
	if got.KeepFailed != 0 || !got.KeepFailedSet {
		t.Fatalf("explicit zero not recorded: %#v", got)
	}

	def := ApplyEnqueueOptions(nil)
	if def.KeepFailedSet {
		t.Fatalf("default options must not record KeepFailedSet")
	}
}

// TestApplyEnqueueOptions_KeepCompletedZeroSentinel: WithKeepCompleted(0)
// も同じく「明示的 opt-out」を表し、KeepCompletedSet で default と区別
// できる必要がある (#1193、KeepFailed と対称)。
func TestApplyEnqueueOptions_KeepCompletedZeroSentinel(t *testing.T) {
	got := ApplyEnqueueOptions([]EnqueueOption{WithKeepCompleted(0)})
	if got.KeepCompleted != 0 || !got.KeepCompletedSet {
		t.Fatalf("explicit zero not recorded: %#v", got)
	}

	def := ApplyEnqueueOptions(nil)
	if def.KeepCompletedSet {
		t.Fatalf("default options must not record KeepCompletedSet")
	}
}

// TestApplyEnqueueOptions_AgeOptions: age-based retention は Set sentinel
// を持たない (age=0 は「設定なし」と同義扱い、BullMQ 仕様で age を 0 に
// する意味のあるケースが無いため)。
func TestApplyEnqueueOptions_AgeOptions(t *testing.T) {
	got := ApplyEnqueueOptions([]EnqueueOption{
		WithKeepCompletedAge(7 * 24 * time.Hour),
		WithKeepFailedAge(3 * 24 * time.Hour),
	})
	if got.KeepCompletedAge != 7*24*time.Hour {
		t.Fatalf("KeepCompletedAge: got %v", got.KeepCompletedAge)
	}
	if got.KeepFailedAge != 3*24*time.Hour {
		t.Fatalf("KeepFailedAge: got %v", got.KeepFailedAge)
	}
}

func TestDelay(t *testing.T) {
	err := fmt.Errorf("held: %w", Delay(3*time.Second))
	var d *DelayError
	if !errors.As(err, &d) || d.Delay != 3*time.Second {
		t.Fatalf("Delay must unwrap to *DelayError(3s), got %v", err)
	}
	if !strings.Contains(err.Error(), "3s") {
		t.Fatalf("message should carry the delay, got %q", err.Error())
	}
}
