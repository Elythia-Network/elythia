package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// infoOnlyInspector answers GetQueueInfo only. 他のメソッドを呼ぶと nil の
// 埋め込みで panic するので、adapter が余計な経路を通らないことも兼ねて見る。
type infoOnlyInspector struct {
	driver.Inspector
	info *driver.InspectorInfo
}

func (i *infoOnlyInspector) GetQueueInfo(string) (*driver.InspectorInfo, error) {
	return i.info, nil
}

type inspectorOnlyDriver struct {
	driver.Driver
	ins driver.Inspector
}

func (d *inspectorOnlyDriver) Inspector() driver.Inspector { return d.ins }

func newAdapterInspector(info *driver.InspectorInfo) *queue.Inspector {
	return queue.NewInspector(&inspectorOnlyDriver{ins: &infoOnlyInspector{info: info}})
}

func TestQueueStatsInspectorAdapter_CarriesDelayed(t *testing.T) {
	a := &queueStatsInspectorAdapter{inner: newAdapterInspector(&driver.InspectorInfo{
		Active: 1, Pending: 2, Completed: 3, Delayed: 4,
	})}
	got, err := a.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.Equal(t, 1, got.Active)
	assert.Equal(t, 2, got.Pending)
	assert.Equal(t, 3, got.Completed)
	assert.Equal(t, 4, got.Delayed, "queueStats delayed must come from the driver's Delayed")
}

func TestQueueInspectorAdapter_CarriesDelayed(t *testing.T) {
	a := &queueInspectorAdapter{inner: newAdapterInspector(&driver.InspectorInfo{
		Queue: "deliver", Size: 9, Active: 1, Pending: 2, Completed: 3, Failed: 4, Delayed: 5,
		IsPaused: true, QualifiedName: "bull:deliver",
	})}
	got, err := a.GetQueueInfo("deliver")
	require.NoError(t, err)
	assert.Equal(t, "deliver", got.Queue)
	assert.Equal(t, 9, got.Size)
	assert.Equal(t, 4, got.Failed)
	assert.Equal(t, 5, got.Delayed, "admin/queue delayed must come from the driver's Delayed")
	assert.True(t, got.IsPaused)
	assert.Equal(t, "bull:deliver", got.QualifiedName)
}
