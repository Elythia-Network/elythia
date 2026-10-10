package backup

import "time"

// MetaFormatVersion is the version of the Meta JSON. Bump it when a field
// changes meaning; readers must reject versions they do not know.
const MetaFormatVersion = 1

// Meta describes one generation. It is written as meta.json after the dump
// has been uploaded and read back, so a generation without meta.json is
// incomplete and must be ignored.
type Meta struct {
	FormatVersion int       `json:"formatVersion"`
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"createdAt"`

	ElythiaVersion string `json:"elythiaVersion"`
	ElythiaCommit  string `json:"elythiaCommit"`
	// PostgresVersion is server_version and PostgresVersionNum is
	// server_version_num of the server the dump was taken from.
	PostgresVersion    string `json:"postgresVersion"`
	PostgresVersionNum int    `json:"postgresVersionNum"`
	Database           string `json:"database"`

	// Migrations is the state of each migration bookkeeping table
	// (schema_migrations, schema_migrations_local) at the snapshot.
	Migrations []MigrationState `json:"migrations"`
	// RowCounts maps "schema.table" to the row count, counted in the same
	// snapshot as the dump (pg_export_snapshot + pg_dump --snapshot).
	RowCounts map[string]int64 `json:"rowCounts"`
	// DatabaseSettings are the per-database settings (ALTER DATABASE ... SET)
	// that pg_dump does not include, as "name=value" (#3461).
	DatabaseSettings []string `json:"databaseSettings"`
	// DatabaseLocale is the encoding and locale the database was created
	// with. A restore target must be created with the same values (#3459,
	// #3461).
	DatabaseLocale DatabaseLocale `json:"databaseLocale"`
	// Extensions are the installed extensions (pg_extension). A restore
	// target needs each of them available, or pg_restore fails at
	// CREATE EXTENSION.
	Extensions []ExtensionInfo `json:"extensions"`
	// PgDumpVersion is the first line of `pg_dump --version` of the program
	// that took the dump.
	PgDumpVersion string `json:"pgDumpVersion"`

	// DumpFile is the key of the dump relative to the generation directory.
	DumpFile string `json:"dumpFile"`
	// DumpSize and DumpSHA256 describe the stored bytes (encrypted when
	// Encrypted). PlainSHA256 is the hash of the pg_dump output itself.
	DumpSize    int64  `json:"dumpSize"`
	DumpSHA256  string `json:"dumpSha256"`
	PlainSHA256 string `json:"plainSha256"`
	Encrypted   bool   `json:"encrypted"`
	// Encryption names the scheme ("age") when Encrypted.
	Encryption string `json:"encryption,omitempty"`
}

// DatabaseLocale is from pg_database for the dumped database.
type DatabaseLocale struct {
	// Encoding is the server encoding name, such as "UTF8".
	Encoding string `json:"encoding"`
	Collate  string `json:"collate"`
	Ctype    string `json:"ctype"`
	// Provider is "libc", "icu" or "builtin" (datlocprovider). Empty on
	// servers older than PostgreSQL 15.
	Provider string `json:"provider,omitempty"`
	// Locale is the ICU or builtin locale (datlocale, daticulocale before
	// PostgreSQL 17). Empty for libc.
	Locale string `json:"locale,omitempty"`
}

// ExtensionInfo is one row of pg_extension.
type ExtensionInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
}

// MigrationState is one row of a migration bookkeeping table.
type MigrationState struct {
	Table   string `json:"table"`
	Version int64  `json:"version"`
	Dirty   bool   `json:"dirty"`
	// Missing is true when the table does not exist in the database.
	Missing bool `json:"missing,omitempty"`
}

// VerifyStage names the stages of `backup verify` (#3459).
type VerifyStage string

// Verify stages, in the order they run.
const (
	StageReadable   VerifyStage = "readable"
	StageRestorable VerifyStage = "restorable"
	StageUsable     VerifyStage = "usable"
)

// VerifyResult is written as verify.json next to meta.json (#3459).
type VerifyResult struct {
	ID             string        `json:"id"`
	VerifiedAt     time.Time     `json:"verifiedAt"`
	OK             bool          `json:"ok"`
	Stages         []StageResult `json:"stages"`
	Mismatches     []RowMismatch `json:"mismatches,omitempty"`
	ElythiaVersion string        `json:"elythiaVersion"`
}

// StageResult is the outcome of one verify stage. Skipped stages (because an
// earlier one failed) are recorded with OK false and Skipped true.
type StageResult struct {
	Stage   VerifyStage `json:"stage"`
	OK      bool        `json:"ok"`
	Skipped bool        `json:"skipped,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// RowMismatch is a table whose restored row count differs from Meta.
type RowMismatch struct {
	Table    string `json:"table"`
	Expected int64  `json:"expected"`
	Actual   int64  `json:"actual"`
}
