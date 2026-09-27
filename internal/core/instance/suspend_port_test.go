package instance_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// モデレーターが停止したインスタンスは、同じホスト名の別ポートで actor を
// 公開し直しても配送が止まったままであること。instance 行は host 完全一致で
// 引くので、ポート付きの host は別の行になる。
func TestShouldSkipDelivery_ManualSuspendCoversOtherPorts(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["evil.example"] = &model.Instance{
		ID: "i1", Host: "evil.example", SuspensionState: model.SuspensionStateManuallySuspended,
	}
	repo.Instances["evil.example:8443"] = &model.Instance{
		ID: "i2", Host: "evil.example:8443", SuspensionState: model.SuspensionStateNone,
	}

	assert.True(t, svc.ShouldSkipDelivery("evil.example"))
	assert.True(t, svc.ShouldSkipDelivery("evil.example:8443"), "別ポートで停止を回避できてはいけない")
	assert.True(t, svc.ShouldSkipDelivery("evil.example:9000"), "行が無いポートも同じ")
	assert.True(t, svc.ShouldSkipDelivery("evil.example."), "末尾ドットも同じ名前")
	assert.False(t, svc.CanFetchOptionalRemoteData("evil.example:8443"))
	assert.False(t, svc.ShouldSkipDelivery("sub.evil.example:8443"), "サブドメインには広げない (完全一致)")
	assert.False(t, svc.ShouldSkipDelivery("other.example:8443"))
}

// 自動停止 (応答なし / gone) はその authority の観測なので、別ポートには広げない。
func TestShouldSkipDelivery_AutoSuspendStaysOnItsPort(t *testing.T) {
	for _, state := range []model.SuspensionState{
		model.SuspensionStateAutoSuspendedForNotResponding,
		model.SuspensionStateGoneSuspended,
	} {
		t.Run(string(state), func(t *testing.T) {
			svc, repo, _ := newService(t)
			repo.Instances["slow.example"] = &model.Instance{ID: "i1", Host: "slow.example", SuspensionState: state}

			assert.True(t, svc.ShouldSkipDelivery("slow.example"))
			assert.False(t, svc.ShouldSkipDelivery("slow.example:8443"))
		})
	}
}

// ポート付きの行自身の停止は従来どおり効く。
func TestShouldSkipDelivery_PortRowSuspendedItself(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["x.example:8443"] = &model.Instance{
		ID: "i1", Host: "x.example:8443", SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding,
	}
	assert.True(t, svc.ShouldSkipDelivery("x.example:8443"))
	assert.False(t, svc.ShouldSkipDelivery("x.example"))
}

// 停止したとき、別ポートのキャッシュ済み判定も捨てて即時に反映すること。
func TestShouldSkipDelivery_SuspendInvalidatesOtherPorts(t *testing.T) {
	svc, repo, _ := newService(t)
	repo.Instances["host.example"] = &model.Instance{
		ID: "i1", Host: "host.example", SuspensionState: model.SuspensionStateNone,
	}
	repo.Instances["other.example:8443"] = &model.Instance{
		ID: "i2", Host: "other.example:8443", SuspensionState: model.SuspensionStateNone,
	}

	assert.False(t, svc.ShouldSkipDelivery("host.example:8443"))
	assert.False(t, svc.ShouldSkipDelivery("other.example:8443"))
	require.Equal(t, 2, svc.SuspendCacheLen())

	require.NoError(t, svc.Suspend("host.example", model.SuspensionStateManuallySuspended))
	assert.Equal(t, 1, svc.SuspendCacheLen(), "無関係な host のキャッシュは残す")
	assert.True(t, svc.ShouldSkipDelivery("host.example:8443"), "suspend must take effect immediately")
}
