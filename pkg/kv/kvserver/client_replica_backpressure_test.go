// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver_test

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/pgurlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// Test that mitigations to backpressure when reducing the range size work.
func TestBackpressureNotAppliedWhenReducingRangeSize(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t, "takes >1m under race")

	rRand, _ := randutil.NewTestRand()
	ctx := context.Background()

	// Some arbitrary data sizes we'll load into a table and then use to derive
	// range size parameters. We want something not too tiny but also not too big
	// that it takes a while to load.
	const (
		rowSize             = 5 << 20   // 5 MiB
		dataSize            = 200 << 20 // 200 MiB
		numRows             = dataSize / rowSize
		min_range_max_bytes = 64 << 20 // 64 MiB
	)
	val := randutil.RandBytes(rRand, rowSize)

	// setup will set up a testcluster with a table filled with data. All splits
	// will be blocked until the returned closure is called.
	setup := func(t *testing.T, numServers int) (
		tc *testcluster.TestCluster,
		args base.TestClusterArgs,
		tdb *sqlutils.SQLRunner,
		tablePrefix roachpb.Key,
		unblockSplit func(),
		waitForBlockedRange func(id roachpb.RangeID),
	) {
		// Add a testing knob to block split transactions which we'll enable before
		// we return from setup.
		var allowSplits atomic.Value
		allowSplits.Store(true)
		unblockCh := make(chan struct{}, 1)
		var rangesBlocked syncutil.Set[roachpb.RangeID]
		args = base.TestClusterArgs{
			ServerArgs: base.TestServerArgs{
				Knobs: base.TestingKnobs{
					Store: &kvserver.StoreTestingKnobs{
						TestingRequestFilter: func(ctx context.Context, ba *kvpb.BatchRequest) *kvpb.Error {
							if ba.Header.Txn != nil && ba.Header.Txn.Name == "split" && !allowSplits.Load().(bool) {
								rangesBlocked.Add(ba.Header.RangeID)
								defer rangesBlocked.Remove(ba.Header.RangeID)
								select {
								case <-unblockCh:
									return kvpb.NewError(errors.Errorf("splits disabled"))
								case <-ctx.Done():
									<-ctx.Done()
								}
							}
							return nil
						},
					},
				},
			},
		}
		tc = testcluster.StartTestCluster(t, numServers, args)
		require.NoError(t, tc.WaitForFullReplication())

		// Create the table, split it off, and load it up with data.
		tdb = sqlutils.MakeSQLRunner(tc.ServerConn(0))

		// speeds up the test
		//		tdb.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '100ms'`)
		//		tdb.Exec(t, `SET CLUSTER SETTING kv.protectedts.poll_interval = '10ms'`)

		tdb.Exec(t, "CREATE TABLE foo (k INT PRIMARY KEY, v BYTES NOT NULL)")

		var tableID int
		tdb.QueryRow(t, "SELECT table_id FROM crdb_internal.tables WHERE name = 'foo'").Scan(&tableID)
		require.NotEqual(t, 0, tableID)
		tablePrefix = keys.SystemSQLCodec.TablePrefix(uint32(tableID))
		tc.SplitRangeOrFatal(t, tablePrefix)
		require.NoError(t, tc.WaitForSplitAndInitialization(tablePrefix))

		for i := 0; i < numRows; i++ {
			tdb.Exec(t, "UPSERT INTO foo VALUES ($1, $2)",
				rRand.Intn(numRows), val)
		}

		// Block splits and return.
		allowSplits.Store(false)
		var closeOnce sync.Once
		unblockSplit = func() {
			closeOnce.Do(func() {
				allowSplits.Store(true)
				close(unblockCh)
			})
		}
		waitForBlockedRange = func(id roachpb.RangeID) {
			testutils.SucceedsSoon(t, func() error {
				if !rangesBlocked.Contains(id) {
					return errors.Errorf("waiting for %v to be blocked", id)
				}
				return nil
			})
		}
		return tc, args, tdb, tablePrefix, unblockSplit, waitForBlockedRange
	}

	waitForSpanConfig := func(t *testing.T, tc *testcluster.TestCluster, tablePrefix roachpb.Key, exp int64) {
		testutils.SucceedsSoon(t, func() error {
			for i := 0; i < tc.NumServers(); i++ {
				s := tc.Server(i)
				_, r := getFirstStoreReplica(t, s, tablePrefix)
				conf, err := r.LoadSpanConfig(ctx)
				if err != nil {
					return err
				}
				if conf.RangeMaxBytes != exp {
					return fmt.Errorf("expected %d, got %d", exp, conf.RangeMaxBytes)
				}
			}
			return nil
		})
	}

	moveTableToNewStore := func(t *testing.T, tc *testcluster.TestCluster, args base.TestClusterArgs, tablePrefix roachpb.Key) {
		tc.AddAndStartServer(t, args.ServerArgs)
		testutils.SucceedsSoon(t, func() error {
			desc, err := tc.LookupRange(tablePrefix)
			require.NoError(t, err)
			// Temporarily turn off queues as we're about to make a manual
			// replication change. We don't want to turn it off throughout
			// these tests as sometimes we change zone configs and expect
			// replicas to move according to them.
			tc.ToggleReplicateQueues(false)
			defer tc.ToggleReplicateQueues(true)
			voters := desc.Replicas().VoterDescriptors()
			if len(voters) == 1 && voters[0].NodeID == tc.Server(1).NodeID() {
				return nil
			}
			if len(voters) == 1 {
				desc, err = tc.AddVoters(tablePrefix, tc.Target(1))
				if err != nil {
					return err
				}
			}
			if err = tc.TransferRangeLease(desc, tc.Target(1)); err != nil {
				return err
			}
			_, err = tc.RemoveVoters(tablePrefix, tc.Target(0))
			return err
		})
	}

	t.Run("no backpressure when much larger on existing node", func(t *testing.T) {
		tc, _, tdb, tablePrefix, unblockSplits, _ := setup(t, 1)
		defer tc.Stopper().Stop(ctx)
		defer unblockSplits()

		tdb.Exec(t, "ALTER TABLE foo CONFIGURE ZONE USING "+
			"range_max_bytes = $1, range_min_bytes = $2", min_range_max_bytes, dataSize/10)
		waitForSpanConfig(t, tc, tablePrefix, min_range_max_bytes)

		// Don't observe backpressure.
		tdb.Exec(t, "UPSERT INTO foo VALUES ($1, $2)",
			rRand.Intn(10000000), val)
	})

	t.Run("no backpressure when much larger on new node", func(t *testing.T) {
		tc, args, tdb, tablePrefix, unblockSplits, _ := setup(t, 1)
		defer tc.Stopper().Stop(ctx)
		defer unblockSplits()
		// We didn't want to have to load too much data into these ranges because
		// it makes the testing slower so let's lower the threshold at which we'll
		// consider the range to be way over the backpressure limit from megabytes
		// down to kilobytes.
		tdb.Exec(t, "SET CLUSTER SETTING kv.range.backpressure_byte_tolerance = '1 KiB'")

		tdb.Exec(t, "ALTER TABLE foo CONFIGURE ZONE USING "+
			"range_max_bytes = $1, range_min_bytes = $2", min_range_max_bytes, dataSize/10)
		waitForSpanConfig(t, tc, tablePrefix, min_range_max_bytes)

		// Then we'll add a new server and move the table there.
		moveTableToNewStore(t, tc, args, tablePrefix)

		// Don't observe backpressure.
		tdb.Exec(t, "UPSERT INTO foo VALUES ($1, $2)",
			rRand.Intn(10000000), val)
	})

	t.Run("no backpressure when near limit on existing node", func(t *testing.T) {
		tc, _, tdb, tablePrefix, unblockSplits, _ := setup(t, 1)
		defer tc.Stopper().Stop(ctx)
		defer unblockSplits()

		// We didn't want to have to load too much data into these ranges because
		// it makes the testing slower so let's lower the threshold at which we'll
		// consider the range to be way over the backpressure limit from megabytes
		// down to kilobytes.
		tdb.Exec(t, "SET CLUSTER SETTING kv.range.backpressure_byte_tolerance = '128 KiB'")

		// Now we'll change the range_max_bytes to be half the range size less a bit
		// so that the range size is above the backpressure threshold but within the
		// backpressureByteTolerance. We won't see backpressure because the range
		// will remember its previous zone config setting.
		s, repl := getFirstStoreReplica(t, tc.Server(0), tablePrefix.Next())
		newMax := repl.GetMVCCStats().Total()/2 - 32<<20
		newMin := newMax / 4
		tdb.Exec(t, "ALTER TABLE foo CONFIGURE ZONE USING "+
			"range_max_bytes = $1, range_min_bytes = $2", newMax, newMin)
		waitForSpanConfig(t, tc, tablePrefix, newMax)

		// Don't observe backpressure because we remember the previous max size on
		// this node.
		tdb.Exec(t, "UPSERT INTO foo VALUES ($1, $2)",
			rRand.Intn(10000000), val)

		// Allow one split to occur and make sure that the remembered value is
		// cleared.
		unblockSplits()

		testutils.SucceedsSoon(t, func() error {
			if size := repl.LargestPreviousMaxRangeSizeBytes(); size != 0 {
				_ = s.ForceSplitScanAndProcess()
				return errors.Errorf("expected LargestPreviousMaxRangeSizeBytes to be 0, got %d", size)
			}
			return nil
		})
	})

	// This case is very similar to the above case but differs in that the range
	// is moved to a new node after the range size is decreased. This new node
	// never knew about the old, larger range size, and thus will backpressure
	// writes. This is the one case that is not mitigated by either
	// backpressureByteTolerance or largestPreviousMaxRangeSizeBytes.
	t.Run("backpressure when near limit on new node", func(t *testing.T) {
		tc, args, tdb, tablePrefix, unblockSplits, waitForBlocked := setup(t, 1)
		defer tc.Stopper().Stop(ctx)
		defer unblockSplits()

		// Now we'll change the range_max_bytes to be half the range size less a
		// bit. This is the range where we expect to observe backpressure.
		_, repl := getFirstStoreReplica(t, tc.Server(0), tablePrefix.Next())
		newMax := repl.GetMVCCStats().Total()/2 - 32<<20
		newMin := newMax / 4
		tdb.Exec(t, "ALTER TABLE foo CONFIGURE ZONE USING "+
			"range_max_bytes = $1, range_min_bytes = $2", newMax, newMin)
		waitForSpanConfig(t, tc, tablePrefix, newMax)

		// Then we'll add a new server and move the table there.
		moveTableToNewStore(t, tc, args, tablePrefix)

		// Ensure that the new replica has applied the same config.
		testutils.SucceedsSoon(t, func() error {
			_, r := getFirstStoreReplica(t, tc.Server(1), tablePrefix)
			conf, err := r.LoadSpanConfig(ctx)
			if err != nil {
				return err
			}
			if conf.RangeMaxBytes != newMax {
				return fmt.Errorf("expected %d, got %d", newMax, conf.RangeMaxBytes)
			}
			return nil
		})

		s, repl := getFirstStoreReplica(t, tc.Server(1), tablePrefix)
		s.TestingSetReplicateQueueActive(false)
		require.Len(t, repl.Desc().Replicas().Descriptors(), 1)
		// We really need to make sure that the split queue has hit this range,
		// otherwise we'll fail to backpressure.
		_ = tc.Stopper().RunAsyncTask(ctx, "force-split", func(context.Context) {
			_ = s.ForceSplitScanAndProcess()
		})

		waitForBlocked(repl.RangeID)

		// Observe backpressure now that the range is just over the limit.
		// Use pgx so that cancellation does something reasonable.
		url, cleanup := pgurlutils.PGUrl(t, tc.Server(1).AdvSQLAddr(), "", url.User("root"))
		defer cleanup()
		conf, err := pgx.ParseConfig(url.String())
		require.NoError(t, err)
		c, err := pgx.ConnectConfig(ctx, conf)
		require.NoError(t, err)
		ctxWithCancel, cancel := context.WithCancel(ctx)
		defer cancel()
		upsertErrCh := make(chan error)
		_ = tc.Stopper().RunAsyncTask(ctx, "upsert", func(ctx context.Context) {
			_, err := c.Exec(ctxWithCancel, "UPSERT INTO foo VALUES ($1, $2)",
				rRand.Intn(numRows), randutil.RandBytes(rRand, rowSize))
			upsertErrCh <- err
		})

		select {
		case <-time.After(10 * time.Millisecond):
			cancel()
		case err := <-upsertErrCh:
			t.Fatalf("expected no error because the request should hang, got %v", err)
		}
		// Unfortunately we can't match on the error (context canceled) here since we can also
		// get random other errors such as:
		// "write failed: write tcp 127.0.0.1:37720->127.0.0.1:44313: i/o timeout"
		require.Error(t, <-upsertErrCh)
	})
}
