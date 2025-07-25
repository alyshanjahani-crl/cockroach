// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package backup

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/cloud/nodelocal"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/securitytest"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/fingerprintutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/jobutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/errors"
	"github.com/kr/pretty"
	"github.com/stretchr/testify/require"
)

var orParams = base.TestClusterArgs{
	// Online restore is not supported in a secondary tenant yet.
	ServerArgs: base.TestServerArgs{
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
	},
}

var latestDownloadJobIDQuery = `SELECT id FROM system.jobs WHERE description LIKE '%Background Data Download%' ORDER BY created DESC LIMIT 1`

func onlineImpl(rng *rand.Rand) string {
	opt := "EXPERIMENTAL DEFERRED COPY"
	if rng.Intn(2) == 0 {
		opt = "EXPERIMENTAL COPY"
	}
	return opt
}

func TestOnlineRestoreBasic(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	ctx := context.Background()

	const numAccounts = 1000

	tc, sqlDB, dir, cleanupFn := backupRestoreTestSetupWithParams(t, singleNode, numAccounts, InitManualReplication, orParams)
	defer cleanupFn()

	rtc, rSQLDB, cleanupFnRestored := backupRestoreTestSetupEmpty(t, 1, dir, InitManualReplication, orParams)
	defer cleanupFnRestored()

	externalStorage := "nodelocal://1/backup"

	createStmt := `SELECT create_statement FROM [SHOW CREATE TABLE data.bank]`
	createStmtRes := sqlDB.QueryStr(t, createStmt)

	testutils.RunTrueAndFalse(t, "incremental", func(t *testing.T, incremental bool) {
		testutils.RunTrueAndFalse(t, "blocking download", func(t *testing.T, blockingDownload bool) {
			sqlDB.Exec(t, fmt.Sprintf("BACKUP DATABASE data INTO '%s'", externalStorage))

			if incremental {
				sqlDB.Exec(t, "UPDATE data.bank SET balance = balance+123 where true;")
				sqlDB.Exec(t, fmt.Sprintf("BACKUP DATABASE data INTO LATEST IN '%s'", externalStorage))
			}

			var preRestoreTs float64
			sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&preRestoreTs)

			bankOnlineRestore(t, rSQLDB, numAccounts, externalStorage, blockingDownload)

			fpSrc, err := fingerprintutils.FingerprintDatabase(ctx, tc.Conns[0], "data", fingerprintutils.Stripped())
			require.NoError(t, err)
			fpDst, err := fingerprintutils.FingerprintDatabase(ctx, rtc.Conns[0], "data", fingerprintutils.Stripped())
			require.NoError(t, err)
			require.NoError(t, fingerprintutils.CompareDatabaseFingerprints(fpSrc, fpDst))

			assertMVCCOnlineRestore(t, rSQLDB, preRestoreTs)
			assertOnlineRestoreWithRekeying(t, sqlDB, rSQLDB)

			if !blockingDownload {
				waitForLatestDownloadJobToSucceed(t, rSQLDB)
			}

			rSQLDB.CheckQueryResults(t, createStmt, createStmtRes)
			sqlDB.CheckQueryResults(t, jobutils.GetExternalBytesForConnectedTenant, [][]string{{"0"}})

			rSQLDB.Exec(t, "DROP DATABASE data CASCADE")
		})
	})

}

func TestOnlineRestoreRecovery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tmpDir := t.TempDir()

	defer nodelocal.ReplaceNodeLocalForTesting(tmpDir)()

	const numAccounts = 1000

	externalStorage := "nodelocal://1/backup"

	_, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(t, singleNode, numAccounts, InitManualReplication, orParams)
	defer cleanupFn()

	restoreToPausedDownloadJob := func(t *testing.T, newDBName string) int {
		defer func() {
			sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = ''")
		}()
		sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = 'restore.before_download'")
		sqlDB.Exec(t, fmt.Sprintf("BACKUP DATABASE data INTO '%s'", externalStorage))
		var linkJobID int
		sqlDB.QueryRow(t, fmt.Sprintf("RESTORE DATABASE data FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY, new_db_name=%s, detached", externalStorage, newDBName)).Scan(&linkJobID)
		jobutils.WaitForJobToSucceed(t, sqlDB, jobspb.JobID(linkJobID))
		var downloadJobID int
		sqlDB.QueryRow(t, latestDownloadJobIDQuery).Scan(&downloadJobID)
		jobutils.WaitForJobToPause(t, sqlDB, jobspb.JobID(downloadJobID))

		var dbExists bool
		sqlDB.QueryRow(t, fmt.Sprintf("SELECT count(*) > 0 FROM system.namespace WHERE name = '%s'", newDBName)).Scan(&dbExists)
		require.True(t, dbExists, "database should exist")

		var externalBytes int64
		sqlDB.QueryRow(t, jobutils.GetExternalBytesForConnectedTenant).Scan(&externalBytes)
		require.Greater(t, externalBytes, int64(0), "external bytes should be greater than 0")
		return downloadJobID
	}

	checkRecovery := func(t *testing.T, dbName string) {
		sqlDB.CheckQueryResults(t, jobutils.GetExternalBytesForConnectedTenant, [][]string{{"0"}})
		var dbExists bool
		sqlDB.QueryRow(t, fmt.Sprintf("SELECT count(*) > 0 FROM system.namespace WHERE name = '%s'", dbName)).Scan(&dbExists)
		require.False(t, dbExists, "database %s should not exist", dbName)
	}

	t.Run("cancel download job", func(t *testing.T) {
		dbName := "data_cancel"
		downloadJobID := restoreToPausedDownloadJob(t, dbName)
		sqlDB.Exec(t, fmt.Sprintf("CANCEL JOB %d", downloadJobID))
		jobutils.WaitForJobToCancel(t, sqlDB, jobspb.JobID(downloadJobID))
		checkRecovery(t, dbName)
	})
	t.Run("delete file", func(t *testing.T) {
		dbName := "data_delete"
		downloadJobID := restoreToPausedDownloadJob(t, dbName)
		corruptBackup(t, sqlDB, tmpDir, externalStorage)
		sqlDB.ExpectErr(t, "no such file or directory", "SELECT count(*) FROM data_delete.bank")
		sqlDB.Exec(t, fmt.Sprintf("RESUME JOB %d", downloadJobID))
		jobutils.WaitForJobToFail(t, sqlDB, jobspb.JobID(downloadJobID))
		checkRecovery(t, dbName)
	})
	t.Run("cancel link job", func(t *testing.T) {
		dbName := "data_link"
		defer func() {
			sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = ''")
		}()
		sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = 'restore.before_publishing_descriptors'")
		sqlDB.Exec(t, fmt.Sprintf("BACKUP DATABASE data INTO '%s'", externalStorage))
		var linkJobID int
		sqlDB.QueryRow(t, fmt.Sprintf("RESTORE DATABASE data FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY, new_db_name=%s, detached", externalStorage, dbName)).Scan(&linkJobID)
		jobutils.WaitForJobToPause(t, sqlDB, jobspb.JobID(linkJobID))
		sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = ''")
		sqlDB.Exec(t, fmt.Sprintf("CANCEL JOB %d", linkJobID))
		jobutils.WaitForJobToCancel(t, sqlDB, jobspb.JobID(linkJobID))
		checkRecovery(t, dbName)
	})
	t.Run("delete file block job", func(t *testing.T) {
		dbName := "data_block"
		defer func() {
			sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = ''")
		}()
		sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = 'restore.before_download'")
		sqlDB.Exec(t, fmt.Sprintf("BACKUP DATABASE data INTO '%s'", externalStorage))
		var blockingJobID int
		sqlDB.QueryRow(t, fmt.Sprintf("RESTORE DATABASE data FROM LATEST IN '%s' WITH EXPERIMENTAL COPY, new_db_name=%s, detached", externalStorage, dbName)).Scan(&blockingJobID)
		jobutils.WaitForJobToPause(t, sqlDB, jobspb.JobID(blockingJobID))
		corruptBackup(t, sqlDB, tmpDir, externalStorage)
		sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = ''")
		sqlDB.Exec(t, fmt.Sprintf("RESUME JOB %d", blockingJobID))
		jobutils.WaitForJobToFail(t, sqlDB, jobspb.JobID(blockingJobID))
		checkRecovery(t, dbName)
	})
}

// We run full cluster online restore recovery in a separate environment since
// it requires dropping all databases and will impact other tests.
func TestFullClusterOnlineRestoreRecovery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tmpDir := t.TempDir()

	defer nodelocal.ReplaceNodeLocalForTesting(tmpDir)()

	const numAccounts = 1000

	externalStorage := "nodelocal://1/backup"

	_, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(t, singleNode, numAccounts, InitManualReplication, orParams)
	defer cleanupFn()

	sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = 'restore.before_download'")
	sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO '%s'", externalStorage))

	// Reset cluster for full cluster restore.
	dbs := sqlDB.QueryStr(
		t, "SELECT database_name FROM [SHOW DATABASES] WHERE database_name != 'system'",
	)
	sqlDB.Exec(t, "USE system")
	for _, db := range dbs {
		sqlDB.Exec(t, fmt.Sprintf("DROP DATABASE %s CASCADE", db[0]))
	}

	var linkJobID jobspb.JobID
	sqlDB.QueryRow(
		t,
		fmt.Sprintf(
			"RESTORE FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY, detached", externalStorage,
		),
	).Scan(&linkJobID)
	jobutils.WaitForJobToSucceed(t, sqlDB, linkJobID)
	var downloadJobID jobspb.JobID
	sqlDB.QueryRow(t, latestDownloadJobIDQuery).Scan(&downloadJobID)
	jobutils.WaitForJobToPause(t, sqlDB, downloadJobID)
	corruptBackup(t, sqlDB, tmpDir, externalStorage)
	sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints = ''")
	sqlDB.Exec(t, fmt.Sprintf("RESUME JOB %d", downloadJobID))
	jobutils.WaitForJobToFail(t, sqlDB, downloadJobID)
}

func corruptBackup(t *testing.T, sqlDB *sqlutils.SQLRunner, ioDir string, uri string) {
	var filePath, spanStart, spanEnd string
	// We delete the last SST file in SHOW BACKUP to ensure the deletion of an SST
	// file that backs a user-table.
	// https://github.com/cockroachdb/cockroach/issues/148408 illustrates how
	// deleting the backing SST file of a system table will not necessarily cause
	// a download job to fail.
	filePathQuery := fmt.Sprintf(
		`SELECT path, start_pretty, end_pretty FROM
		(
			SELECT row_number() OVER (), *
			FROM [SHOW BACKUP FILES FROM LATEST IN '%s']
		)
		ORDER BY row_number DESC
		LIMIT 1`,
		uri,
	)
	parsedURI, err := url.Parse(strings.ReplaceAll(uri, "'", ""))
	sqlDB.QueryRow(t, filePathQuery).Scan(&filePath, &spanStart, &spanEnd)
	fullPath := filepath.Join(ioDir, parsedURI.Path, filePath)
	t.Logf("deleting backup file %s covering span [%s, %s)", fullPath, spanStart, spanEnd)
	require.NoError(t, err)
	require.NoError(t, os.Remove(fullPath))
}

func TestOnlineRestorePartitioned(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	srv, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(t, 3, 100,
		InitManualReplication,
		base.TestClusterArgs{
			ServerArgs: base.TestServerArgs{DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant},
		},
	)
	defer cleanupFn()

	sqlDB.Exec(t, `BACKUP DATABASE data INTO ('nodelocal://1/a?COCKROACH_LOCALITY=default',
		'nodelocal://1/b?COCKROACH_LOCALITY=dc%3Ddc2',
		'nodelocal://1/c?COCKROACH_LOCALITY=dc%3Ddc3')`)

	j := sqlDB.QueryStr(t, `RESTORE DATABASE data FROM LATEST IN ('nodelocal://1/a?COCKROACH_LOCALITY=default',
		'nodelocal://1/b?COCKROACH_LOCALITY=dc%3Ddc2',
		'nodelocal://1/c?COCKROACH_LOCALITY=dc%3Ddc3') WITH new_db_name='d2', EXPERIMENTAL DEFERRED COPY`)

	srv.Servers[0].JobRegistry().(*jobs.Registry).TestingNudgeAdoptionQueue()

	sqlDB.Exec(t, fmt.Sprintf(`SHOW JOB WHEN COMPLETE %s`, j[0][4]))
}

func TestOnlineRestoreLinkCheckpoint(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	rng, _ := randutil.NewTestRand()

	const numAccounts = 10
	_, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(
		t,
		singleNode,
		numAccounts,
		InitManualReplication,
		orParams,
	)
	defer cleanupFn()
	sqlDB.Exec(t, "BACKUP DATABASE data INTO $1", localFoo)
	sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints  = 'restore.before_publishing_descriptors'")
	var jobID jobspb.JobID
	stmt := fmt.Sprintf("RESTORE DATABASE data FROM LATEST IN $1 WITH OPTIONS (new_db_name='data2', %s, detached)", onlineImpl(rng))
	sqlDB.QueryRow(t, stmt, localFoo).Scan(&jobID)
	jobutils.WaitForJobToPause(t, sqlDB, jobID)

	// Set a pauspoint during the link phase which should not get hit because of
	// checkpointing.
	sqlDB.Exec(t, "SET CLUSTER SETTING jobs.debug.pausepoints  = 'restore.before_link'")
	sqlDB.Exec(t, "RESUME JOB $1", jobID)
	jobutils.WaitForJobToSucceed(t, sqlDB, jobID)
}

func TestOnlineRestoreStatementResult(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	const numAccounts = 1
	_, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(
		t,
		singleNode,
		numAccounts,
		InitManualReplication,
		base.TestClusterArgs{
			ServerArgs: base.TestServerArgs{
				DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
			},
		},
	)
	defer cleanupFn()

	sqlDB.ExecMultiple(
		t,
		"USE data",
		"CREATE TABLE foo (x INT PRIMARY KEY, y INT)",
		"INSERT INTO foo VALUES (1, 2)",
	)
	sqlDB.Exec(t, "BACKUP DATABASE data INTO $1", localFoo)

	rows := sqlDB.Query(
		t,
		"RESTORE DATABASE data FROM LATEST IN $1 WITH OPTIONS (new_db_name='data2', experimental deferred copy)",
		localFoo,
	)
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if a, e := columns, []string{
		"job_id", "tables", "approx_rows", "approx_bytes", "background_download_job_id",
	}; !reflect.DeepEqual(e, a) {
		t.Fatalf("unexpected columns:\n%s", strings.Join(pretty.Diff(e, a), "\n"))
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("zero rows in result")
	}

	var id, expectedID, tables, approxRows, approxBytes, downloadJobID int64
	require.NoError(t, rows.Scan(
		&id, &tables, &approxRows, &approxBytes, &downloadJobID,
	))
	sqlDB.QueryRow(t,
		`SELECT job_id FROM crdb_internal.jobs WHERE job_id = $1`, id,
	).Scan(
		&expectedID,
	)

	require.Equal(t, expectedID, id, "result does not match system.jobs")
	require.Equal(t, id+1, downloadJobID, "download job id should be one greater than restore job id")
	require.Equal(t, int64(2), tables, "expected 2 tables to be in result")
	require.Greater(t, approxRows, int64(0), "no rows estimated in result")
	require.Greater(t, approxBytes, int64(0), "no bytes estimated in result")

	if rows.Next() {
		t.Fatal("more than one row in result")
	}
}

// TestOnlineRestoreWaitForDownload checks that the download job succeeeds even
// if no queries are run on the restoring key space.
func TestOnlineRestoreWaitForDownload(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	const numAccounts = 1000
	_, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(t, singleNode, numAccounts, InitManualReplication, base.TestClusterArgs{
		// Online restore is not supported in a secondary tenant yet.
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
		},
	})
	defer cleanupFn()
	externalStorage := "nodelocal://1/backup"

	sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO '%s'", externalStorage))

	sqlDB.Exec(t, fmt.Sprintf("RESTORE DATABASE data FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY, new_db_name=data2", externalStorage))
	waitForLatestDownloadJobToSucceed(t, sqlDB)
	sqlDB.CheckQueryResults(t, jobutils.GetExternalBytesForConnectedTenant, [][]string{{"0"}})

}

func waitForLatestDownloadJobToSucceed(t *testing.T, sqlDB *sqlutils.SQLRunner) {
	var downloadJobID jobspb.JobID
	sqlDB.QueryRow(t,
		latestDownloadJobIDQuery,
	).Scan(&downloadJobID)
	jobutils.WaitForJobToSucceed(t, sqlDB, downloadJobID)
}

// TestOnlineRestoreTenant runs an online restore of a tenant and ensures the
// restore is not MVCC compliant.
func TestOnlineRestoreTenant(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	externalStorage := "nodelocal://1/backup"

	params := base.TestClusterArgs{ServerArgs: base.TestServerArgs{
		Knobs: base.TestingKnobs{
			JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
			TenantTestingKnobs: &sql.TenantTestingKnobs{
				// The tests expect specific tenant IDs to show up.
				EnableTenantIDReuse: true,
			},
		},

		DefaultTestTenant: base.TestControlsTenantsExplicitly},
	}
	const numAccounts = 1

	tc, systemDB, dir, cleanupFn := backupRestoreTestSetupWithParams(
		t, singleNode, numAccounts, InitManualReplication, params,
	)
	_, _ = tc, systemDB
	defer cleanupFn()
	srv := tc.Server(0)

	_ = securitytest.EmbeddedTenantIDs()

	_, conn10 := serverutils.StartTenant(t, srv, base.TestTenantArgs{TenantID: roachpb.MustMakeTenantID(10)})
	defer conn10.Close()
	tenant10 := sqlutils.MakeSQLRunner(conn10)
	tenant10.Exec(t, `CREATE DATABASE foo; CREATE TABLE foo.bar(i int primary key); INSERT INTO foo.bar VALUES (110), (210)`)

	testutils.RunTrueAndFalse(t, "incremental", func(t *testing.T, incremental bool) {

		restoreTC, rSQLDB, cleanupFnRestored := backupRestoreTestSetupEmpty(t, 1, dir, InitManualReplication, params)
		defer cleanupFnRestored()

		systemDB.Exec(t, fmt.Sprintf(`BACKUP TENANT 10 INTO '%s'`, externalStorage))

		if incremental {
			tenant10.Exec(t, "INSERT INTO foo.bar VALUES (111), (211)")
			systemDB.Exec(t, fmt.Sprintf(`BACKUP TENANT 10 INTO LATEST IN '%s'`, externalStorage))
		}

		var preRestoreTs float64
		tenant10.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&preRestoreTs)

		belowID := uint64(2)
		aboveID := uint64(20)

		// Restore the tenant twice: once below and once above the old ID, to show
		// that we can rewrite it in either direction.
		rSQLDB.Exec(t, fmt.Sprintf("RESTORE TENANT 10 FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY, TENANT_NAME = 'below', TENANT = '%d'", externalStorage, belowID))
		rSQLDB.Exec(t, fmt.Sprintf("RESTORE TENANT 10 FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY, TENANT_NAME = 'above', TENANT = '%d'", externalStorage, aboveID))
		rSQLDB.Exec(t, "ALTER TENANT below STOP SERVICE")
		rSQLDB.Exec(t, "ALTER TENANT above STOP SERVICE")
		rSQLDB.CheckQueryResults(t, "SELECT fingerprint FROM [SHOW EXPERIMENTAL_FINGERPRINTS FROM TENANT below]",
			rSQLDB.QueryStr(t, `SELECT fingerprint FROM [SHOW EXPERIMENTAL_FINGERPRINTS FROM TENANT above]`))

		secondaryStopper := stop.NewStopper()
		_, cBelow := serverutils.StartTenant(
			t, restoreTC.Server(0), base.TestTenantArgs{
				TenantName: "below",
				TenantID:   roachpb.MustMakeTenantID(belowID),
				Stopper:    secondaryStopper,
			})
		_, cAbove := serverutils.StartTenant(
			t, restoreTC.Server(0), base.TestTenantArgs{
				TenantName: "above",
				TenantID:   roachpb.MustMakeTenantID(aboveID),
				Stopper:    secondaryStopper,
			})

		defer func() {
			cBelow.Close()
			cAbove.Close()
			secondaryStopper.Stop(context.Background())
		}()
		dbBelow, dbAbove := sqlutils.MakeSQLRunner(cBelow), sqlutils.MakeSQLRunner(cAbove)
		dbBelow.CheckQueryResults(t, `select * from foo.bar`, tenant10.QueryStr(t, `select * from foo.bar`))
		dbAbove.CheckQueryResults(t, `select * from foo.bar`, tenant10.QueryStr(t, `select * from foo.bar`))

		// Ensure the restore of a tenant was not mvcc
		var maxRestoreMVCCTimestamp float64
		dbBelow.QueryRow(t, "SELECT max(crdb_internal_mvcc_timestamp) FROM foo.bar").Scan(&maxRestoreMVCCTimestamp)
		require.Greater(t, preRestoreTs, maxRestoreMVCCTimestamp)
		dbAbove.QueryRow(t, "SELECT max(crdb_internal_mvcc_timestamp) FROM foo.bar").Scan(&maxRestoreMVCCTimestamp)
		require.Greater(t, preRestoreTs, maxRestoreMVCCTimestamp)

		dbAbove.CheckQueryResults(t, jobutils.GetExternalBytesForConnectedTenant, [][]string{{"0"}})
		dbBelow.CheckQueryResults(t, jobutils.GetExternalBytesForConnectedTenant, [][]string{{"0"}})
		rSQLDB.CheckQueryResults(t, jobutils.GetExternalBytesTenantKeySpace, [][]string{{"0"}})
	})
}

func TestOnlineRestoreErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	_, sqlDB, dir, cleanupFn := backupRestoreTestSetup(t, singleNode, 1, InitManualReplication)
	defer cleanupFn()
	params := base.TestClusterArgs{
		// Online restore is not supported in a secondary tenant yet.
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
		},
	}
	_, rSQLDB, cleanupFnRestored := backupRestoreTestSetupEmpty(t, 1, dir, InitManualReplication, params)
	defer cleanupFnRestored()
	rSQLDB.Exec(t, "CREATE DATABASE data")
	var (
		fullBackup                = "nodelocal://1/full-backup"
		fullBackupWithRevs        = "nodelocal://1/full-backup-with-revs"
		incrementalBackupWithRevs = "nodelocal://1/incremental-backup-with-revs"
	)
	t.Run("full backups with revision history are unsupported", func(t *testing.T) {
		var systemTime string
		sqlDB.QueryRow(t, "SELECT cluster_logical_timestamp()").Scan(&systemTime)
		sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO '%s' AS OF SYSTEM TIME '%s' WITH revision_history", fullBackupWithRevs, systemTime))
		rSQLDB.ExpectErr(t, "revision history backup not supported",
			fmt.Sprintf("RESTORE TABLE data.bank FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY", fullBackupWithRevs))
	})
	t.Run("incremental backups with revision history are unsupported", func(t *testing.T) {
		sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO '%s' WITH revision_history", incrementalBackupWithRevs))
		sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO LATEST IN '%s' WITH revision_history", incrementalBackupWithRevs))
		rSQLDB.ExpectErr(t, "revision history backup not supported",
			fmt.Sprintf("RESTORE TABLE data.bank FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY", incrementalBackupWithRevs))
	})
	t.Run("external storage locations that don't support early boot are unsupported", func(t *testing.T) {
		rSQLDB.Exec(t, "CREATE DATABASE bank")
		rSQLDB.Exec(t, "BACKUP INTO 'userfile:///my_backups'")
		rSQLDB.ExpectErr(t, "scheme userfile is not accessible during node startup",
			"RESTORE DATABASE bank FROM LATEST IN 'userfile:///my_backups' WITH EXPERIMENTAL DEFERRED COPY")
	})
	t.Run("verify_backup_table_data not supported", func(t *testing.T) {
		sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO '%s'", fullBackup))
		sqlDB.ExpectErr(t, "cannot run online restore with verify_backup_table_data",
			fmt.Sprintf("RESTORE data FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY, schema_only, verify_backup_table_data", fullBackup))
	})
}

func TestOnlineRestoreRetryingDownloadRequests(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	rng, seed := randutil.NewPseudoRand()
	t.Logf("random seed: %d", seed)

	alwaysFail := rng.Intn(2) == 0
	t.Logf("always fail download requests: %t", alwaysFail)
	totalFailures := int32(rng.Intn(maxDownloadAttempts-1) + 1)
	var currentFailures atomic.Int32

	clusterArgs := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
			Knobs: base.TestingKnobs{
				BackupRestore: &sql.BackupRestoreTestingKnobs{
					RunBeforeSendingDownloadSpan: func() error {
						if alwaysFail {
							return errors.Newf("always fail download request")
						}
						if currentFailures.Load() >= totalFailures {
							return nil
						}
						currentFailures.Add(1)
						return errors.Newf("injected download request failure")
					},
				},
			},
		},
	}

	const numAccounts = 1
	_, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(
		t, singleNode, numAccounts, InitManualReplication, clusterArgs,
	)
	defer cleanupFn()

	externalStorage := "nodelocal://1/backup"
	sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO '%s'", externalStorage))
	sqlDB.Exec(
		t,
		fmt.Sprintf(`
		RESTORE DATABASE data FROM LATEST IN '%s'
		WITH EXPERIMENTAL DEFERRED COPY, new_db_name=data2
		`, externalStorage),
	)

	var downloadJobID jobspb.JobID
	sqlDB.QueryRow(t, latestDownloadJobIDQuery).Scan(&downloadJobID)
	if alwaysFail {
		jobutils.WaitForJobToFail(t, sqlDB, downloadJobID)
	} else {
		jobutils.WaitForJobToSucceed(t, sqlDB, downloadJobID)
	}
}

func TestOnlineRestoreDownloadRetryReset(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()

	var attemptCount int
	clusterArgs := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
			Knobs: base.TestingKnobs{
				BackupRestore: &sql.BackupRestoreTestingKnobs{
					// We want the retry loop to fail until its final attempt, and then
					// succeed on the last attempt. This will allow the download job to
					// make progress, in which case the retry loop _should_ reset. Then
					// we continue allowing the retry loop to fail until its last
					// attempt, in which case it will succeed again.
					RunBeforeSendingDownloadSpan: func() error {
						attemptCount++
						if attemptCount < maxDownloadAttempts {
							return errors.Newf("injected download request failure")
						}
						return nil
					},
					RunBeforeDownloadCleanup: func() error {
						if attemptCount < maxDownloadAttempts*2 {
							return errors.Newf("injected download cleanup failure")
						}
						return nil
					},
				},
			},
		},
	}
	const numAccounts = 1
	_, sqlDB, _, cleanupFn := backupRestoreTestSetupWithParams(
		t, singleNode, numAccounts, InitManualReplication, clusterArgs,
	)
	defer cleanupFn()

	externalStorage := "nodelocal://1/backup"
	sqlDB.Exec(t, fmt.Sprintf("BACKUP INTO '%s'", externalStorage))
	sqlDB.Exec(
		t,
		fmt.Sprintf(`
		RESTORE DATABASE data FROM LATEST IN '%s'
		WITH EXPERIMENTAL DEFERRED COPY, new_db_name=data2
		`, externalStorage),
	)

	var downloadJobID jobspb.JobID
	sqlDB.QueryRow(t, latestDownloadJobIDQuery).Scan(&downloadJobID)
	jobutils.WaitForJobToSucceed(t, sqlDB, downloadJobID)
	require.Equal(t, maxDownloadAttempts*2, attemptCount)
}

func bankOnlineRestore(
	t *testing.T,
	sqlDB *sqlutils.SQLRunner,
	numAccounts int,
	externalStorage string,
	blockingDownload bool,
) {
	// Create a table in the default database to force table id rewriting.
	sqlDB.Exec(t, "CREATE TABLE IF NOT EXISTS foo (i INT PRIMARY KEY, s STRING);")

	sqlDB.Exec(t, "CREATE DATABASE data")
	stmt := fmt.Sprintf("RESTORE TABLE data.bank FROM LATEST IN '%s' WITH EXPERIMENTAL DEFERRED COPY", externalStorage)
	if blockingDownload {
		stmt = fmt.Sprintf("RESTORE TABLE data.bank FROM LATEST IN '%s' WITH EXPERIMENTAL COPY", externalStorage)
	}
	sqlDB.Exec(t, stmt)

	var restoreRowCount int
	sqlDB.QueryRow(t, "SELECT count(*) FROM data.bank").Scan(&restoreRowCount)
	require.Equal(t, numAccounts, restoreRowCount)
}

// assertMVCCOnlineRestore checks that online restore conducted mvcc compatible
// addsstable requests. Note that the restoring database is written to, so no
// fingerprinting can be done after this command.
func assertMVCCOnlineRestore(t *testing.T, sqlDB *sqlutils.SQLRunner, preRestoreTs float64) {
	// Check that Online Restore was MVCC
	var minRestoreMVCCTimestamp float64
	sqlDB.QueryRow(t, "SELECT min(crdb_internal_mvcc_timestamp) FROM data.bank").Scan(&minRestoreMVCCTimestamp)
	require.Greater(t, minRestoreMVCCTimestamp, preRestoreTs)

	// Check that we can write on top of OR data
	var maxRestoreMVCCTimestamp float64
	sqlDB.QueryRow(t, "SELECT max(crdb_internal_mvcc_timestamp) FROM data.bank").Scan(&maxRestoreMVCCTimestamp)

	// The where true conditional avoids the need to set sql_updates to true.
	sqlDB.Exec(t, "UPDATE data.bank SET balance = balance+1 where true;")

	var updateMVCCTimestamp float64
	sqlDB.QueryRow(t, "SELECT min(crdb_internal_mvcc_timestamp) FROM data.bank").Scan(&updateMVCCTimestamp)
	require.Greater(t, updateMVCCTimestamp, maxRestoreMVCCTimestamp)
}

func assertOnlineRestoreWithRekeying(
	t *testing.T, sqlDB *sqlutils.SQLRunner, rSQLDB *sqlutils.SQLRunner,
) {
	bankTableIDQuery := "SELECT id FROM system.namespace WHERE name = 'bank'"
	var (
		originalID int
		restoreID  int
	)
	sqlDB.QueryRow(t, bankTableIDQuery).Scan(&originalID)
	rSQLDB.QueryRow(t, bankTableIDQuery).Scan(&restoreID)
	require.NotEqual(t, originalID, restoreID)
}
