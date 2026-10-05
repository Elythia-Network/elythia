// Package charts hosts the concrete chart definitions wired on top of
// the generic chart engine in internal/core/chart. Each file in this
// directory exports one chart's Schema plus a typed wrapper that knows
// how to translate domain events (a created note, a deleted file, an
// inbox delivery) into the dot-notation Diff payload understood by the
// engine.
//
// The wrappers intentionally hold no state of their own beyond the
// engine pointer; the buffering, save loop and locking all live in
// chart.Chart so that ManagementService can drive every chart through
// the same Save() entry point.
package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaNotes returns the chart.Schema for the instance-wide "notes"
// chart. Column ordering and names mirror the upstream
// `core/chart/charts/entities/notes.ts` definition exactly so the SQL
// table written by migration 000011 maps cleanly onto Repository reads.
func SchemaNotes() chart.Schema {
	return chart.Schema{
		Name: "notes",
		Columns: []chart.ColumnDef{
			{Name: "local.total", Accumulate: true},
			{Name: "local.inc"},
			{Name: "local.dec"},
			{Name: "local.diffs.normal"},
			{Name: "local.diffs.reply"},
			{Name: "local.diffs.renote"},
			{Name: "local.diffs.withFile"},
			{Name: "remote.total", Accumulate: true},
			{Name: "remote.inc"},
			{Name: "remote.dec"},
			{Name: "remote.diffs.normal"},
			{Name: "remote.diffs.reply"},
			{Name: "remote.diffs.renote"},
			{Name: "remote.diffs.withFile"},
		},
	}
}

// NotesChart aggregates note creation and deletion deltas for the
// instance-wide notes chart. The hook is fired by NoteCreateService and
// NoteDeleteService via the Update method.
type NotesChart struct {
	c *chart.Chart
}

// NewNotesChart wraps an already-constructed *chart.Chart. Callers are
// responsible for building the engine with SchemaNotes() so that the
// underlying repository points at the right tables.
func NewNotesChart(c *chart.Chart) *NotesChart {
	return &NotesChart{c: c}
}

// Chart returns the embedded engine pointer. Used by the chart
// management service to register the chart for the periodic save loop.
func (n *NotesChart) Chart() *chart.Chart { return n.c }

// Update commits the delta produced by a single note create / delete
// event. The isAdditional flag is true for create and false for delete;
// it controls the sign of the totals and which inc/dec slot is bumped.
//
// 本家の `NotesChart.update` と同じ式を使い、replyId/renoteId/fileIds
// の有無で diffs.* 列の対象を選別する。Diff の値が 0 のものは
// chart.Chart.Commit が落とすので明示的にスキップしなくてよい。
func (n *NotesChart) Update(note *model.Note, isAdditional bool) error {
	prefix := "local"
	if note.UserHost != nil && *note.UserHost != "" {
		prefix = "remote"
	}
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
		prefix + ".total": sign,
		prefix + ".inc":   incVal,
		prefix + ".dec":   decVal,
	}
	if note.ReplyID == nil && note.RenoteID == nil {
		diff[prefix+".diffs.normal"] = sign
	}
	if note.RenoteID != nil {
		diff[prefix+".diffs.renote"] = sign
	}
	if note.ReplyID != nil {
		diff[prefix+".diffs.reply"] = sign
	}
	if len(note.FileIDs) > 0 {
		diff[prefix+".diffs.withFile"] = sign
	}
	return n.c.Commit(diff, "")
}
