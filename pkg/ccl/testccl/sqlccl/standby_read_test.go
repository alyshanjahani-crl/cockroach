// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sqlccl

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestStandbyRead(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderDuress(t, "slow test")

	testcases := []struct {
		standby  bool
		stmt     string
		expected [][]string
	}{
		{stmt: `CREATE TABLE abc (a INT PRIMARY KEY, b INT, c JSONB)`},
		{stmt: `INSERT INTO abc VALUES (1, 10, '[100]'), (3, 30, '[300]'), (5, 50, '[500]')`},
		{stmt: `ALTER TABLE abc SPLIT AT VALUES (2), (4)`},
		{stmt: `SELECT count(*) FROM [SHOW TABLES]`, expected: [][]string{{"1"}}},
		{stmt: `SELECT count(*) FROM abc`, expected: [][]string{{"3"}}},
		{standby: true, stmt: `SELECT count(*) FROM [SHOW TABLES]`, expected: [][]string{{"1"}}},
		{standby: true, stmt: `SELECT count(*) FROM abc`, expected: [][]string{{"3"}}},
	}

	ctx := context.Background()
	tc := serverutils.StartCluster(t, 3, /* numNodes */
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				DefaultTestTenant: base.TestControlsTenantsExplicitly,
			},
		})
	defer tc.Stopper().Stop(ctx)
	ts := tc.Server(0)

	_, srcDB, err := ts.TenantController().StartSharedProcessTenant(ctx,
		base.TestSharedProcessTenantArgs{
			TenantID:    serverutils.TestTenantID(),
			TenantName:  "src",
			UseDatabase: "defaultdb",
		},
	)
	require.NoError(t, err)
	dstTenant, dstDB, err := ts.TenantController().StartSharedProcessTenant(ctx,
		base.TestSharedProcessTenantArgs{
			TenantID:    serverutils.TestTenantID2(),
			TenantName:  "dst",
			UseDatabase: "defaultdb",
		},
	)
	require.NoError(t, err)

	srcRunner := sqlutils.MakeSQLRunner(srcDB)
	dstRunner := sqlutils.MakeSQLRunner(dstDB)

	dstRunner.Exec(t, `SET CLUSTER SETTING sql.defaults.distsql = always`)
	dstRunner.Exec(t, `SET distsql = always`)

	for _, tc := range testcases {
		var runner *sqlutils.SQLRunner
		if tc.standby {
			testcluster.WaitForStandbyTenantReplication(t, ctx, ts.Clock(), dstTenant)
			runner = dstRunner
		} else {
			runner = srcRunner
		}
		if tc.expected == nil {
			runner.Exec(t, tc.stmt)
		} else {
			runner.CheckQueryResultsRetry(t, tc.stmt, tc.expected)
		}
	}
}
