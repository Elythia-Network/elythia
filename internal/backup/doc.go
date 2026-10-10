// Package backup takes, verifies and restores database backups (#3457).
//
// A backup is a "generation": a directory in the storage that holds the
// pg_dump custom-format file, its metadata and, once verified, the result of
// the verification. See Layout for the key names.
//
// Elythia の本体の image は distroless で pg_dump を持たないので、このパッケージの
// 処理はバックアップ用の image (postgres:18-alpine + elythia) の中で動かす前提で
// 書く。PostgreSQL のクライアント (pg_dump など) は Tools で外から差し替えられる。
package backup
