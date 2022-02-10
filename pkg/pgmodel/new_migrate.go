// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package pgmodel

import (
	"context"
	"fmt"
	"sync"

	"github.com/blang/semver/v4"
	"github.com/jackc/pgx/v4"
	"github.com/timescale/promscale/pkg/log"
	"github.com/timescale/promscale/pkg/migrations"
	"github.com/timescale/promscale/pkg/pgmodel/common/errors"
	"github.com/timescale/promscale/pkg/pgmodel/common/extension"
	"github.com/timescale/promscale/pkg/util"
	"github.com/timescale/promscale/pkg/version"
)

var (
	MigrationLockError = fmt.Errorf("Could not acquire migration lock. Ensure there are no other connectors running and try again.")
	migrateMutex       = &sync.Mutex{}
)

func doesSchemaMigrationTableExist(db *pgx.Conn) (exists bool, err error) {
	const stmt = "SELECT count(*) FILTER (WHERE tablename = 'prom_schema_migrations') > 0 FROM pg_tables"
	err = db.QueryRow(
		context.Background(),
		stmt,
	).Scan(&exists)
	return
}

func removeOldExtensionIfExists(db *pgx.Conn) error {
	const (
		// transition is the first version of the extension that does the
		// migrations the new way (i.e. in the extension rather than from promscale connector)
		transition = "0.5.0"
		stmt       = "DROP EXTENSION IF EXISTS promscale" // TODO to cascade or not to cascade?
	)

	installedVersion, installed, err := extension.FetchInstalledExtensionVersion(db, "promscale")
	if err != nil {
		return fmt.Errorf("error fetching extension version while dropping old extension: %w", err)
	}

	if installed && installedVersion.LT(semver.MustParse(transition)) {
		_, err := db.Exec(
			context.Background(),
			stmt,
		)
		if err != nil {
			return fmt.Errorf("error dropping old extension: %w", err)
		}
	}

	return nil
}

func installExtensionAllBalls(db *pgx.Conn) error {
	const stmt = "CREATE EXTENSION promscale SCHEMA _prom_ext VERSION '0.0.0'"
	_, err := db.Exec(
		context.Background(),
		stmt,
	)
	return fmt.Errorf("error installing Promscale extension at version 0.0.0: %w", err)
}

func Migrate(conn *pgx.Conn, appVersion VersionInfo, leaseLock *util.PgAdvisoryLock, extOptions extension.ExtensionMigrateOptions) error {
	// At startup migrators attempt to grab the schema-version lock. If this
	// fails that means some other connector is running. All is not lost: some
	// other connector may have migrated the DB to the correct version. We warn,
	// then start the connector as normal. If we are on the wrong version, the
	// normal version-check code will prevent us from running.

	if leaseLock != nil {
		locked, err := leaseLock.GetAdvisoryLock()
		if err != nil {
			return fmt.Errorf("error while acquiring migration lock %w", err)
		}
		if !locked {
			return MigrationLockError
		}
		defer func() {
			_, err := leaseLock.Unlock()
			if err != nil {
				log.Error("msg", "error while releasing migration lock", "err", err)
			}
		}()
	} else {
		log.Warn("msg", "skipping migration lock")
	}

	migrateMutex.Lock()
	defer migrateMutex.Unlock()

	// if the old prom_schema_migrations table exists, then we need to apply any outstanding
	// migrations from the old way of doing migrations, then transition to the new way
	// the prom_schema_migrations table will be dropped as a part of the transition
	schemaMigrationTableExists, err := doesSchemaMigrationTableExist(conn)
	if err != nil {
		return err
	}
	if schemaMigrationTableExists {
		if err = oldMigrate(conn, appVersion); err != nil {
			return fmt.Errorf("error while trying to migrate DB: %w", err)
		}
		if err = removeOldExtensionIfExists(conn); err != nil {
			return fmt.Errorf("error while dropping old promscale extension: %w", err)
		}
		if err = installExtensionAllBalls(conn); err != nil {
			return fmt.Errorf("error while installing promscale extension version 0.0.0: %w", err)
		}
	}

	_, err = extension.InstallUpgradePromscaleExtensions(conn, extOptions)
	if err != nil {
		return err
	}

	return nil
}

func oldMigrate(db *pgx.Conn, versionInfo VersionInfo) (err error) {
	appVersion, err := semver.Make(versionInfo.Version)
	if err != nil {
		return errors.ErrInvalidSemverFormat
	}

	mig := NewMigrator(db, migrations.MigrationFiles, tableOfContents)

	err = mig.Migrate(appVersion)
	if err != nil {
		return fmt.Errorf("Error encountered during migration: %w", err)
	}

	return nil
}

// CheckDependencies makes sure the Promscale and TimescaleDB extensions are set up correctly. This will set
// the ExtensionIsInstalled flag and thus should only be called once, at initialization.
func CheckDependencies(db *pgx.Conn, migrationFailedDueToLockError bool, extOptions extension.ExtensionMigrateOptions) (err error) {
	return extension.CheckVersions(db, migrationFailedDueToLockError, extOptions)
}

// CheckPromscaleExtInstalledVersion checks the promscale extension version installed
func CheckPromscaleExtInstalledVersion(conn *pgx.Conn) error {
	installedVersion, isInstalled, err := extension.FetchInstalledExtensionVersion(conn, "promscale")
	if err != nil {
		return fmt.Errorf("failed to fetch the installed version of the promscale extension: %s", err)
	}
	if !isInstalled {
		return fmt.Errorf("promscale extension is required but is not installed")
	}
	if !version.ExtVersionRange(installedVersion) {
		return fmt.Errorf("the promscale connector requires the promscale extension to be in version range %s, but the installed version of the extension is %s", version.ExtVersionRangeString, installedVersion)
	}
	return nil
}
