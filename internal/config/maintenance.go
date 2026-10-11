package config

import (
	"fmt"
	"time"
)

// DefaultMaintenanceDB is the database the maintenance judge connects to
// when counting sessions (#3463).
const DefaultMaintenanceDB = "postgres"

// DefaultMaintenanceDrainTimeout is how long the maintenance judge waits
// before terminating the Elythia sessions that remain (#3463).
const DefaultMaintenanceDrainTimeout = 30 * time.Second

// MaintenanceSource is the `maintenance:` section of the config file.
type MaintenanceSource struct {
	// MaintenanceDB is the database the judge connects to while it counts
	// the sessions on the application database in pg_stat_activity. It must
	// not be the application database.
	MaintenanceDB string `mapstructure:"maintenanceDb"`
	// DrainTimeout is a Go duration (e.g. "30s").
	DrainTimeout string `mapstructure:"drainTimeout"`
}

// MaintenanceOptions is the resolved `maintenance:` section.
type MaintenanceOptions struct {
	MaintenanceDB string
	DrainTimeout  time.Duration
}

// resolveMaintenance fills the defaults of the `maintenance:` section. The
// section is optional; every field has a default.
func resolveMaintenance(src *MaintenanceSource, appDB string) (MaintenanceOptions, error) {
	out := MaintenanceOptions{MaintenanceDB: DefaultMaintenanceDB, DrainTimeout: DefaultMaintenanceDrainTimeout}
	if src == nil {
		return out, nil
	}
	if src.MaintenanceDB != "" {
		out.MaintenanceDB = src.MaintenanceDB
	}
	// 対象の DB に繋いで数えると、判定役の接続が数に入って 0 にならない。
	if out.MaintenanceDB == appDB {
		return out, fmt.Errorf("maintenance.maintenanceDb must not be the application database %q", appDB)
	}
	if src.DrainTimeout != "" {
		d, err := time.ParseDuration(src.DrainTimeout)
		if err != nil {
			return out, fmt.Errorf("maintenance.drainTimeout: %w", err)
		}
		if d <= 0 {
			return out, fmt.Errorf("maintenance.drainTimeout must be positive, got %s", src.DrainTimeout)
		}
		out.DrainTimeout = d
	}
	return out, nil
}
