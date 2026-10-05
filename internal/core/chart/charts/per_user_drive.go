package charts

import (
	"errors"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaPerUserDrive returns the schema for the per-user "perUserDrive"
// chart. totalCount/totalSize accumulate while inc*/dec* track per-bucket
// deltas. Sizes are stored in kilobytes to match the upstream wire shape.
func SchemaPerUserDrive() chart.Schema {
	return chart.Schema{
		Name:    "perUserDrive",
		Grouped: true,
		Columns: []chart.ColumnDef{
			{Name: "totalCount", Accumulate: true},
			{Name: "totalSize", Accumulate: true},
			{Name: "incCount", Range: chart.RangeSmall},
			{Name: "incSize"},
			{Name: "decCount", Range: chart.RangeSmall},
			{Name: "decSize"},
		},
	}
}

// PerUserDriveChart aggregates per-user drive upload / delete events.
type PerUserDriveChart struct {
	c *chart.Chart
}

// NewPerUserDriveChart wraps an engine instance built with
// SchemaPerUserDrive.
func NewPerUserDriveChart(c *chart.Chart) *PerUserDriveChart {
	return &PerUserDriveChart{c: c}
}

// Chart returns the underlying engine pointer.
func (p *PerUserDriveChart) Chart() *chart.Chart { return p.c }

// ErrPerUserDriveNoOwner is returned by Update when the supplied file
// has no owning user id; chart commits require a non-empty group.
var ErrPerUserDriveNoOwner = errors.New("charts: per-user drive update requires file.UserID")

// Update commits the delta from a drive upload (isAdditional=true) or
// delete (isAdditional=false). The owning user becomes the group key;
// files without an owner are rejected with ErrPerUserDriveNoOwner.
func (p *PerUserDriveChart) Update(file *model.DriveFile, isAdditional bool) error {
	if file.UserID == nil || *file.UserID == "" {
		return ErrPerUserDriveNoOwner
	}
	// 本家と同じく kilobyte 単位で記録する。整数列なので 1000 で割って切り捨てる。
	sizeKb := int64(file.Size) / 1000

	totalCountDelta := int64(1)
	totalSizeDelta := sizeKb
	if !isAdditional {
		totalCountDelta = -1
		totalSizeDelta = -sizeKb
	}
	incCount := int64(0)
	incSize := int64(0)
	decCount := int64(0)
	decSize := int64(0)
	if isAdditional {
		incCount = 1
		incSize = sizeKb
	} else {
		decCount = 1
		decSize = sizeKb
	}
	return p.c.Commit(chart.Diff{
		"totalCount": totalCountDelta,
		"totalSize":  totalSizeDelta,
		"incCount":   incCount,
		"incSize":    incSize,
		"decCount":   decCount,
		"decSize":    decSize,
	}, *file.UserID)
}
