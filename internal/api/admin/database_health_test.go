package admin_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/dbhealth"
)

type stubDBHealth struct {
	r   dbhealth.Report
	err error
}

func (s stubDBHealth) Report(context.Context) (dbhealth.Report, error) { return s.r, s.err }

func TestDatabaseHealth(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DatabaseHealth, `{}`, adminUser).Code, "not wired")

	at := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	size := int64(100)
	h.SetDatabaseHealth(stubDBHealth{r: dbhealth.Report{GeneratedAt: at, ReplicasConfigured: true,
		AutovacuumThreshold: 50, AutovacuumScaleFactor: 0.2,
		UnusedIndexes: []dbhealth.IndexStat{{Table: "note", Index: "IDX_x", SizeBytes: 8192, Unique: true}},
		Tables:        []dbhealth.TableStat{{Table: "note", LiveRows: 10, DeadRows: 5, DeadRatio: 1.0 / 3, SizeBytes: &size, LastVacuum: &at}},
		Problems:      []dbhealth.Problem{{Table: "note", Kind: "bloat", Detail: "note: dead"}},
	}})
	rec := doPost(h.DatabaseHealth, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"generatedAt":"2026-09-29T00:00:00Z","statsReset":null,"replicasConfigured":true,
		"autovacuumThreshold":50,"autovacuumScaleFactor":0.2,"autovacuumMaxThreshold":0,
		"unusedIndexes":[{"table":"note","index":"IDX_x","scans":0,"sizeBytes":8192,"unique":true,"primary":false}],
		"tables":[{"table":"note","liveRows":10,"deadRows":5,"deadRatio":0.3333333333333333,"sizeBytes":100,
		"lastVacuum":"2026-09-29T00:00:00Z","lastAnalyze":null,"modifiedSinceAnalyze":0,"vacuuming":false}],
		"problems":[{"table":"note","kind":"bloat","detail":"note: dead"}]}`, rec.Body.String())

	h.SetDatabaseHealth(stubDBHealth{err: errors.New("db down")})
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DatabaseHealth, `{}`, adminUser).Code)
	h.SetDatabaseHealth(stubDBHealth{err: context.Canceled})
	assert.Equal(t, http.StatusInternalServerError, doPost(h.DatabaseHealth, `{}`, adminUser).Code)
}
