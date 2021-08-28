// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package persistedsqlstats_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobstest"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/scheduledjobs"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlstats/persistedsqlstats"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/sql/tests"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

type testHelper struct {
	server           serverutils.TestServerInterface
	sqlDB            *sqlutils.SQLRunner
	env              *jobstest.JobSchedulerTestEnv
	cfg              *scheduledjobs.JobExecutionConfig
	executeSchedules func() error
}

func (h *testHelper) waitForSuccessfulScheduledJob(t *testing.T, sj *jobs.ScheduledJob) {
	query := fmt.Sprintf(`
SELECT id
FROM %s
WHERE
  status=$1
  AND created_by_type=$2
  AND created_by_id=$3
`, h.env.SystemJobsTableName())

	testutils.SucceedsSoon(t, func() error {
		// Force the job created by the schedule to actually run.
		h.server.JobRegistry().(*jobs.Registry).TestingNudgeAdoptionQueue()
		var unused int64
		return h.sqlDB.DB.QueryRowContext(context.Background(),
			query, jobs.StatusSucceeded, jobs.CreatedByScheduledJobs, sj.ScheduleID()).Scan(&unused)
	})
}

func newTestHelper(t *testing.T) (helper *testHelper, cleanup func()) {
	helper = &testHelper{
		env: jobstest.NewJobSchedulerTestEnv(jobstest.UseSystemTables, timeutil.Now()),
	}

	knobs := jobs.NewTestingKnobsWithShortIntervals()
	knobs.JobSchedulerEnv = helper.env
	knobs.TakeOverJobsScheduling = func(fn func(ctx context.Context, maxSchedules int64, txn *kv.Txn) error) {
		helper.executeSchedules = func() error {
			defer helper.server.JobRegistry().(*jobs.Registry).TestingNudgeAdoptionQueue()
			return helper.cfg.DB.Txn(context.Background(), func(ctx context.Context, txn *kv.Txn) error {
				// maxSchedules = 0 means there's no limit.
				return fn(ctx, 0 /* maxSchedules */, txn)
			})
		}
	}
	knobs.CaptureJobExecutionConfig = func(config *scheduledjobs.JobExecutionConfig) {
		helper.cfg = config
	}

	params, _ := tests.CreateTestServerParams()
	params.Knobs.JobsTestingKnobs = knobs
	server, db, _ := serverutils.StartServer(t, params)
	require.NotNil(t, helper.cfg)

	helper.sqlDB = sqlutils.MakeSQLRunner(db)
	helper.server = server

	return helper, func() {
		server.Stopper().Stop(context.Background())
	}
}

func verifySQLStatsCompactionScheduleCreatedOnStartup(t *testing.T, helper *testHelper) {
	var compactionScheduleCount int
	row := helper.sqlDB.QueryRow(t, `SELECT count(*) FROM system.scheduled_jobs WHERE schedule_name = 'sql-stats-compaction'`)
	row.Scan(&compactionScheduleCount)
	require.Equal(t, 1 /* expected */, compactionScheduleCount)
}

func getSQLStatsCompactionSchedule(t *testing.T, helper *testHelper) *jobs.ScheduledJob {
	var jobID int64
	helper.sqlDB.
		QueryRow(t, `SELECT schedule_id FROM system.scheduled_jobs WHERE schedule_name = 'sql-stats-compaction'`).
		Scan(&jobID)
	sj, err :=
		jobs.LoadScheduledJob(
			context.Background(),
			helper.env,
			jobID,
			helper.server.InternalExecutor().(sqlutil.InternalExecutor),
			nil, /* txn */
		)
	require.NoError(t, err)
	require.NotNil(t, sj)
	return sj
}

func TestScheduledSQLStatsCompaction(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	helper, helperCleanup := newTestHelper(t)
	defer helperCleanup()

	// We run some queries then flush so that we ensure that are some stats in
	// the system table.
	helper.sqlDB.Exec(t, "SELECT 1; SELECT 1, 1")
	helper.server.SQLServer().(*sql.Server).GetSQLStatsProvider().(*persistedsqlstats.PersistedSQLStats).Flush(ctx)
	helper.sqlDB.Exec(t, "SET CLUSTER SETTING sql.stats.persisted_rows.max = 1")

	stmtStatsCnt, txnStatsCnt := getPersistedStatsEntry(t, helper.sqlDB)
	require.True(t, stmtStatsCnt >= 2,
		"expecting at least 2 persisted stmt fingerprints, but found: %d", stmtStatsCnt)
	require.True(t, txnStatsCnt >= 2,
		"expecting at least 2 persisted txn fingerprints, but found: %d", txnStatsCnt)

	verifySQLStatsCompactionScheduleCreatedOnStartup(t, helper)
	schedule := getSQLStatsCompactionSchedule(t, helper)
	require.Equal(t, string(jobs.StatusPending), schedule.ScheduleStatus())

	// Force the schedule to execute.
	helper.env.SetTime(schedule.NextRun().Add(time.Minute))
	require.NoError(t, helper.executeSchedules())
	helper.waitForSuccessfulScheduledJob(t, schedule)

	// Read the system.scheduled_job table again.
	schedule = getSQLStatsCompactionSchedule(t, helper)
	require.Equal(t, string(jobs.StatusSucceeded), schedule.ScheduleStatus())

	stmtStatsCnt, txnStatsCnt = getPersistedStatsEntry(t, helper.sqlDB)
	require.Equal(t, 1 /* expected */, stmtStatsCnt,
		"expecting exactly 1 persisted stmt fingerprints, but found: %d", stmtStatsCnt)
	require.Equal(t, 1 /* expected */, txnStatsCnt,
		"expecting exactly 1 persisted txn fingerprints, but found: %d", txnStatsCnt)
}

func TestSQLStatsScheduleOperations(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	helper, helperCleanup := newTestHelper(t)
	defer helperCleanup()

	sj := getSQLStatsCompactionSchedule(t, helper)

	t.Run("schedule_cannot_be_dropped", func(t *testing.T) {
		_, err := helper.sqlDB.DB.ExecContext(ctx, "DROP SCHEDULE $1", sj.ScheduleID())
		require.True(t,
			strings.Contains(err.Error(), persistedsqlstats.ErrScheduleUndroppable.Error()),
			"expected to found ErrScheduleUndroppable, but found %+v", err)
	})

	t.Run("warn_schedule_paused", func(t *testing.T) {
		helper.sqlDB.Exec(t, "PAUSE SCHEDULE $1", sj.ScheduleID())
		defer helper.sqlDB.Exec(t, "RESUME SCHEDULE $1", sj.ScheduleID())

		helper.sqlDB.CheckQueryResults(
			t,
			fmt.Sprintf("SELECT schedule_status FROM [SHOW SCHEDULE %d]", sj.ScheduleID()),
			[][]string{{"PAUSED"}},
		)

		// Reload schedule from DB.
		sj = getSQLStatsCompactionSchedule(t, helper)
		err := persistedsqlstats.CheckScheduleAnomaly(sj)
		require.True(t, errors.Is(err, persistedsqlstats.ErrSchedulePaused),
			"expected ErrSchedulePaused, but found %+v", err)
	})

	t.Run("warn_schedule_long_run_interval", func(t *testing.T) {
		t.Run("via cluster setting", func(t *testing.T) {
			helper.sqlDB.Exec(t, "SET CLUSTER SETTING sql.stats.cleanup.recurrence = '0 59 23 24 12 ? 2099'")

			var err error
			testutils.SucceedsSoon(t, func() error {
				// Reload schedule from DB.
				sj := getSQLStatsCompactionSchedule(t, helper)
				err = persistedsqlstats.CheckScheduleAnomaly(sj)
				if err == nil {
					return errors.Newf("retry: next_run=%s, schedule_expr=%s", sj.NextRun(), sj.ScheduleExpr())
				}
				require.Equal(t, "0 59 23 24 12 ? 2099", sj.ScheduleExpr())
				return nil
			})
			require.True(t, errors.Is(
				errors.Unwrap(err), persistedsqlstats.ErrScheduleIntervalTooLong),
				"expected ErrScheduleIntervalTooLong, but found %+v", err)

			helper.sqlDB.Exec(t, "RESET CLUSTER SETTING sql.stats.cleanup.recurrence")
			helper.sqlDB.CheckQueryResultsRetry(t,
				fmt.Sprintf(`
SELECT schedule_expr
FROM system.scheduled_jobs WHERE schedule_id = %d`, sj.ScheduleID()),
				[][]string{{"@hourly"}},
			)
		})

		t.Run("via directly updating system table", func(t *testing.T) {
			sj := getSQLStatsCompactionSchedule(t, helper)
			rowsAffected, err := helper.sqlDB.Exec(t, `
			 UPDATE system.scheduled_jobs
			 SET (schedule_expr, next_run) = ('@weekly', $1)
			 WHERE schedule_id = $2`, timeutil.Now().Add(time.Hour*24*7), sj.ScheduleID()).RowsAffected()
			require.NoError(t, err)
			require.Equal(t, int64(1) /* expected */, rowsAffected)

			// Sanity check.
			helper.sqlDB.CheckQueryResults(t,
				fmt.Sprintf(`
SELECT schedule_expr
FROM system.scheduled_jobs WHERE schedule_id = %d`, sj.ScheduleID()),
				[][]string{{"@weekly"}},
			)

			sj = getSQLStatsCompactionSchedule(t, helper)
			require.Equal(t, "@weekly", sj.ScheduleExpr())
			err = persistedsqlstats.CheckScheduleAnomaly(sj)
			require.NotNil(t, err)
			require.True(t, errors.Is(
				errors.Unwrap(err), persistedsqlstats.ErrScheduleIntervalTooLong),
				"expected ErrScheduleIntervalTooLong, but found %+v", err)
		})
	})
}
