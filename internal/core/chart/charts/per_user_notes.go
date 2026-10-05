package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaPerUserNotes returns the schema for the per-user "perUserNotes"
// chart. Grouped by user id; column shape mirrors the upstream
// `entities/per-user-notes.ts` definition.
func SchemaPerUserNotes() chart.Schema {
	return chart.Schema{
		Name:    "perUserNotes",
		Grouped: true,
		Columns: []chart.ColumnDef{
			{Name: "total", Accumulate: true},
			{Name: "inc", Range: chart.RangeSmall},
			{Name: "dec", Range: chart.RangeSmall},
			{Name: "diffs.normal", Range: chart.RangeSmall},
			{Name: "diffs.reply", Range: chart.RangeSmall},
			{Name: "diffs.renote", Range: chart.RangeSmall},
			{Name: "diffs.withFile", Range: chart.RangeSmall},
		},
	}
}

// PerUserNotesChart aggregates per-user note create / delete events.
type PerUserNotesChart struct {
	c *chart.Chart
}

// NewPerUserNotesChart wraps an engine instance built with
// SchemaPerUserNotes.
func NewPerUserNotesChart(c *chart.Chart) *PerUserNotesChart {
	return &PerUserNotesChart{c: c}
}

// Chart returns the underlying engine pointer.
func (p *PerUserNotesChart) Chart() *chart.Chart { return p.c }

// Update commits the delta from a single note create / delete event.
// `userID` is the note's author and becomes the chart group key. The
// branch logic mirrors NotesChart but writes into the per-user table.
func (p *PerUserNotesChart) Update(userID string, note *model.Note, isAdditional bool) error {
	sign := int64(1)
	if !isAdditional {
		sign = -1
	}
	incVal := int64(0)
	decVal := int64(0)
	if isAdditional {
		incVal = 1
	} else {
		decVal = 1
	}
	diff := chart.Diff{
		"total": sign,
		"inc":   incVal,
		"dec":   decVal,
	}
	if note.ReplyID == nil && note.RenoteID == nil {
		diff["diffs.normal"] = sign
	}
	if note.RenoteID != nil {
		diff["diffs.renote"] = sign
	}
	if note.ReplyID != nil {
		diff["diffs.reply"] = sign
	}
	if len(note.FileIDs) > 0 {
		diff["diffs.withFile"] = sign
	}
	return p.c.Commit(diff, userID)
}
