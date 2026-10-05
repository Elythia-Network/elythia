package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaInstance returns the schema for the per-instance "instance"
// chart. The chart is grouped by host so callers must always supply a
// non-empty host string when committing.
func SchemaInstance() chart.Schema {
	return chart.Schema{
		Name:    "instance",
		Grouped: true,
		Columns: []chart.ColumnDef{
			{Name: "requests.failed", Range: chart.RangeSmall},
			{Name: "requests.succeeded", Range: chart.RangeSmall},
			{Name: "requests.received", Range: chart.RangeSmall},
			{Name: "notes.total", Accumulate: true},
			{Name: "notes.inc"},
			{Name: "notes.dec"},
			{Name: "notes.diffs.normal"},
			{Name: "notes.diffs.reply"},
			{Name: "notes.diffs.renote"},
			{Name: "notes.diffs.withFile"},
			{Name: "users.total", Accumulate: true},
			{Name: "users.inc", Range: chart.RangeSmall},
			{Name: "users.dec", Range: chart.RangeSmall},
			{Name: "following.total", Accumulate: true},
			{Name: "following.inc", Range: chart.RangeSmall},
			{Name: "following.dec", Range: chart.RangeSmall},
			{Name: "followers.total", Accumulate: true},
			{Name: "followers.inc", Range: chart.RangeSmall},
			{Name: "followers.dec", Range: chart.RangeSmall},
			{Name: "drive.totalFiles", Accumulate: true},
			{Name: "drive.incFiles"},
			{Name: "drive.decFiles"},
			{Name: "drive.incUsage"},
			{Name: "drive.decUsage"},
		},
	}
}

// InstanceChart aggregates per-host federation traffic. Every Commit
// targets one host group and the engine maintains separate buckets per
// host.
type InstanceChart struct {
	c *chart.Chart
}

// NewInstanceChart wraps an engine instance built with SchemaInstance.
func NewInstanceChart(c *chart.Chart) *InstanceChart {
	return &InstanceChart{c: c}
}

// Chart returns the underlying engine pointer.
func (i *InstanceChart) Chart() *chart.Chart { return i.c }

// RequestReceived bumps the request received counter for the host.
func (i *InstanceChart) RequestReceived(host string) error {
	return i.c.Commit(chart.Diff{
		"requests.received": int64(1),
	}, host)
}

// RequestSent bumps the succeeded or failed counter depending on the
// outcome of an outbound deliver attempt.
func (i *InstanceChart) RequestSent(host string, succeeded bool) error {
	succ := int64(0)
	fail := int64(0)
	if succeeded {
		succ = 1
	} else {
		fail = 1
	}
	return i.c.Commit(chart.Diff{
		"requests.succeeded": succ,
		"requests.failed":    fail,
	}, host)
}

// NewUser bumps the host's running user total when a remote user is
// observed for the first time.
func (i *InstanceChart) NewUser(host string) error {
	return i.c.Commit(chart.Diff{
		"users.total": int64(1),
		"users.inc":   int64(1),
	}, host)
}

// UpdateNote commits a remote note create / delete delta into the
// host's bucket. Mirrors `InstanceChart.updateNote` upstream.
func (i *InstanceChart) UpdateNote(host string, note *model.Note, isAdditional bool) error {
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
		"notes.total": sign,
		"notes.inc":   incVal,
		"notes.dec":   decVal,
	}
	if note.ReplyID == nil && note.RenoteID == nil {
		diff["notes.diffs.normal"] = sign
	}
	if note.RenoteID != nil {
		diff["notes.diffs.renote"] = sign
	}
	if note.ReplyID != nil {
		diff["notes.diffs.reply"] = sign
	}
	if len(note.FileIDs) > 0 {
		diff["notes.diffs.withFile"] = sign
	}
	return i.c.Commit(diff, host)
}

// UpdateFollowing commits a follow / unfollow delta originating from a
// local user following a remote `host`.
func (i *InstanceChart) UpdateFollowing(host string, isAdditional bool) error {
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
	return i.c.Commit(chart.Diff{
		"following.total": sign,
		"following.inc":   incVal,
		"following.dec":   decVal,
	}, host)
}

// UpdateFollowers commits a follow / unfollow delta originating from a
// remote `host` following a local user.
func (i *InstanceChart) UpdateFollowers(host string, isAdditional bool) error {
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
	return i.c.Commit(chart.Diff{
		"followers.total": sign,
		"followers.inc":   incVal,
		"followers.dec":   decVal,
	}, host)
}

// UpdateDrive commits a drive file upload / delete delta into the
// remote owner's host bucket. Mirrors `InstanceChart.updateDrive`
// upstream including the (apparent) bug where dec values fire even
// when isAdditional=true; we keep the same shape for compatibility but
// the engine drops zero entries so the result still makes sense.
func (i *InstanceChart) UpdateDrive(file *model.DriveFile, isAdditional bool) error {
	if file.UserHost == nil || *file.UserHost == "" {
		// 本家は file.userHost が null のときも commit するが、grouped
		// chart は group が空文字だとエラーになるので local file は
		// この chart の対象外とする (notes.ts と同じ "instance" 用途)。
		return nil
	}
	host := *file.UserHost
	sign := int64(1)
	if !isAdditional {
		sign = -1
	}
	sizeKb := int64(file.Size) / 1000

	totalDelta := sign
	incFiles := int64(0)
	decFiles := int64(0)
	incUsage := int64(0)
	decUsage := int64(0)
	if isAdditional {
		incFiles = 1
		incUsage = sizeKb
	} else {
		decFiles = 1
		decUsage = sizeKb
	}
	return i.c.Commit(chart.Diff{
		"drive.totalFiles": totalDelta,
		"drive.incFiles":   incFiles,
		"drive.decFiles":   decFiles,
		"drive.incUsage":   incUsage,
		"drive.decUsage":   decUsage,
	}, host)
}
