package chart

import "gorm.io/gorm"

// MigratedTestDB returns the migrated test database opened by TestMain, or
// nil when PostgreSQL is not available. It lets the external test package
// check the chart schemas in internal/core/chart/charts against the real
// tables (charts imports chart, so the check cannot live in this package).
func MigratedTestDB() *gorm.DB { return testDB }

// ColumnName exposes toColumnName to the external test package.
var ColumnName = toColumnName
