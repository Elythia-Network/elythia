package config

// BackupOptions is the `backup:` section of the config file (#3457).
//
// 保存先の認証情報は DB ではなく設定ファイルに置く。DB が失われたときにも
// バックアップを取り出せる必要があるため (ドライブの保存先は DB の meta にある
// ので流用しない)。同じホストのディスクはバックアップに数えないので、保存先は
// S3 互換か、別の機器を mount したディレクトリに限る。
type BackupOptions struct {
	// Storage is where generations are written. Required.
	Storage BackupStorageOptions `mapstructure:"storage"`
	// Encryption encrypts the dump with age when Enabled.
	Encryption BackupEncryptionOptions `mapstructure:"encryption"`
	// Schedule drives `elythia backup daemon` (#3460).
	Schedule BackupScheduleOptions `mapstructure:"schedule"`
	// Notify is where failures, mismatches and delays are reported (#3460).
	Notify BackupNotifyOptions `mapstructure:"notify"`
	// Server holds what the main server (not the backup service) uses to
	// list generations and issue download URLs (#3462).
	Server BackupServerOptions `mapstructure:"server"`
	// Tools overrides the paths of the PostgreSQL client programs. Empty
	// fields are looked up in PATH.
	Tools BackupToolsOptions `mapstructure:"tools"`
}

// BackupStorageOptions selects and configures the storage backend.
type BackupStorageOptions struct {
	// Type is "s3" or "dir".
	Type string                 `mapstructure:"type"`
	S3   BackupS3Options        `mapstructure:"s3"`
	Dir  BackupDirectoryOptions `mapstructure:"dir"`
}

// BackupS3Options configures an S3-compatible bucket.
type BackupS3Options struct {
	// Endpoint is the base URL (e.g. https://<account>.r2.cloudflarestorage.com).
	// Empty means AWS S3.
	Endpoint       string `mapstructure:"endpoint"`
	Region         string `mapstructure:"region"`
	Bucket         string `mapstructure:"bucket"`
	Prefix         string `mapstructure:"prefix"`
	AccessKey      string `mapstructure:"accessKey"`
	SecretKey      string `mapstructure:"secretKey"`
	ForcePathStyle bool   `mapstructure:"forcePathStyle"`
}

// BackupDirectoryOptions configures a directory on a mounted remote device.
type BackupDirectoryOptions struct {
	Path string `mapstructure:"path"`
}

// BackupEncryptionOptions configures age encryption of the dump.
type BackupEncryptionOptions struct {
	Enabled bool `mapstructure:"enabled"`
	// Recipients are age public keys (age1...). Taking a backup needs only
	// these, so the private key does not have to be on the backup host.
	Recipients []string `mapstructure:"recipients"`
	// IdentityFile is the path of the age private key file, needed by
	// verify and restore.
	IdentityFile string `mapstructure:"identityFile"`
}

// BackupScheduleOptions configures periodic backups (#3460).
type BackupScheduleOptions struct {
	// Interval between backups, as a Go duration (e.g. "24h"). Empty disables
	// the daemon.
	Interval string `mapstructure:"interval"`
	// At is the local time of day ("HH:MM") the first backup of a cycle starts.
	At string `mapstructure:"at"`
	// Keep is the number of verified generations to keep.
	Keep int `mapstructure:"keep"`
	// Verify runs `backup verify` after every backup.
	Verify bool `mapstructure:"verify"`
	// Listen is the address the daemon's control API listens on (e.g.
	// ":3010"). Empty disables the control API.
	Listen string `mapstructure:"listen"`
}

// BackupNotifyOptions configures notifications (#3460).
type BackupNotifyOptions struct {
	WebhookURL string `mapstructure:"webhookUrl"`
	// Format is "generic", "discord" or "slack".
	Format string `mapstructure:"format"`
}

// BackupServerOptions configures what the main server uses (#3462).
type BackupServerOptions struct {
	// Storage overrides Storage for the main server, so that it can be given
	// a key limited to listing and presigning. Empty Type means Storage.
	Storage BackupStorageOptions `mapstructure:"storage"`
	// PricePerGBMonth, when set, shows a monthly estimate in the admin page.
	PricePerGBMonth float64 `mapstructure:"pricePerGbMonth"`
	// ServiceURL is the URL of the control API of `elythia backup daemon`
	// that the main server asks to take or verify a backup.
	ServiceURL string `mapstructure:"serviceUrl"`
	// ServiceToken authenticates the main server to that control API. The
	// daemon reads the same value to check requests.
	ServiceToken string `mapstructure:"serviceToken"`
}

// BackupToolsOptions overrides PostgreSQL client program paths.
type BackupToolsOptions struct {
	PgDump    string `mapstructure:"pgDump"`
	PgRestore string `mapstructure:"pgRestore"`
	Initdb    string `mapstructure:"initdb"`
	PgCtl     string `mapstructure:"pgCtl"`
	Psql      string `mapstructure:"psql"`
}
