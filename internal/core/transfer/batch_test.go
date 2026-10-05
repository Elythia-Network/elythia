package transfer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExport_Notes_MultiBatch triggers the pagination loop in
// exportNotesStream by providing more notes than the internal batch size.
// NoteRepository.ListByUserID returns at most `limit` rows per call so this
// exercises the "len == batchSize -> continue with untilID" branch.
func TestExport_Notes_MultiBatch(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)
	for i := range 105 {
		text := fmt.Sprintf("note%d", i)
		// IDs must sort lexicographically with newer IDs > older IDs.
		// Use a wide prefix so 105 IDs remain well-ordered.
		nID := fmt.Sprintf("aa%04d", 10000-i)
		noteRepo.Notes[nID] = &model.Note{
			ID: nID, UserID: user.ID, Text: &text,
			Visibility: model.NoteVisibilityPublic,
		}
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportNotes)
	require.NoError(t, err)

	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	assert.Equal(t, 105, len(arr))
}

// TestExport_Notes_PackFields verifies packNoteForExport includes the optional
// fields (VisibleUserIDs, ReplyID, RenoteID) on a fully-populated note.
func TestExport_Notes_PackFields(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)

	text := "hey"
	cw := "warn"
	reply := "p"
	ren := "r"
	noteRepo.Notes["nx"] = &model.Note{
		ID: "nx", UserID: user.ID, Text: &text, CW: &cw,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"bob", "carol"},
		ReplyID:        &reply,
		RenoteID:       &ren,
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportNotes)
	require.NoError(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	require.Len(t, arr, 1)
	assert.Equal(t, "hey", arr[0]["text"])
	assert.Equal(t, "warn", arr[0]["cw"])
	assert.NotNil(t, arr[0]["visibleUserIds"])
	assert.Equal(t, "p", arr[0]["replyId"])
	assert.Equal(t, "r", arr[0]["renoteId"])
}

// TestImport_UserLists_MemberCreateError exercises the AddMember error path
// by pre-creating a conflicting membership (mock Create is idempotent).
// Since the mock doesn't error on duplicate, we exercise the empty target
// branch which also returns Skipped.
func TestImport_UserLists_EmptyTarget(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, []byte("list1,  \nlist1,bob\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportUserLists, "fid")
	require.NoError(t, err)
	// 1 applied (bob) + 1 skipped (empty acct)
	assert.Equal(t, 1, res.Applied)
	assert.Equal(t, 1, res.Skipped)
}

// TestImport_Muting_MalformedAcct exercises parseAcct error in muting path.
func TestImport_Muting_MalformedAcct(t *testing.T) {
	deps, user, _, _, _, _ := newImporterDeps(t, []byte("@\n"))
	imp := transfer.NewImporter(deps)
	res, err := imp.Import(context.Background(), user.ID, transfer.ImportMuting, "fid")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
}
