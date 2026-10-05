package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaDrive returns the schema for the instance-wide "drive" chart.
// All increments are integer counters; sizes are stored in kilobytes
// (matching the upstream convention).
func SchemaDrive() chart.Schema {
	return chart.Schema{
		Name: "drive",
		Columns: []chart.ColumnDef{
			{Name: "local.incCount"},
			{Name: "local.incSize"},
			{Name: "local.decCount"},
			{Name: "local.decSize"},
			{Name: "remote.incCount"},
			{Name: "remote.incSize"},
			{Name: "remote.decCount"},
			{Name: "remote.decSize"},
		},
	}
}

// DriveChart aggregates drive file upload / delete events for the
// instance-wide drive chart.
type DriveChart struct {
	c *chart.Chart
}

// NewDriveChart constructs a DriveChart wrapping the supplied engine.
func NewDriveChart(c *chart.Chart) *DriveChart { return &DriveChart{c: c} }

// Chart returns the underlying engine pointer.
func (d *DriveChart) Chart() *chart.Chart { return d.c }

// Update commits the delta from a drive file upload (isAdditional=true)
// or delete (isAdditional=false). The size is converted from bytes to
// kilobytes (truncating, since the underlying column is integer).
func (d *DriveChart) Update(file *model.DriveFile, isAdditional bool) error {
	prefix := "local"
	if file.UserHost != nil && *file.UserHost != "" {
		prefix = "remote"
	}
	// 本家は kilobyte 単位で保存する。整数列なので 1000 で割って切り捨てる。
	sizeKb := int64(file.Size) / 1000

	incCount := int64(0)
	decCount := int64(0)
	incSize := int64(0)
	decSize := int64(0)
	if isAdditional {
		incCount = 1
		incSize = sizeKb
	} else {
		decCount = 1
		decSize = sizeKb
	}
	diff := chart.Diff{
		prefix + ".incCount": incCount,
		prefix + ".incSize":  incSize,
		prefix + ".decCount": decCount,
		prefix + ".decSize":  decSize,
	}
	return d.c.Commit(diff, "")
}
