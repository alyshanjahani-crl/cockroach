// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package localtestcluster

import (
	"context"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/config/zonepb"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/rangefeed"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/allocator/mmaprototype"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/allocator/storepool"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/closedts/sidetransport"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/load"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/storeliveness"
	"github.com/cockroachdb/cockroach/pkg/multitenant/tenantcapabilities/tenantcapabilitiesauthorizer"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/server/systemconfigwatcher"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/spanconfig/spanconfigkvsubscriber"
	"github.com/cockroachdb/cockroach/pkg/spanconfig/spanconfigstore"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/bootstrap"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/hlc/logger"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
)

// A LocalTestCluster encapsulates an in-memory instantiation of a
// cockroach node with a single store using a local sender. Example
// usage of a LocalTestCluster follows:
//
//	s := &LocalTestCluster{}
//	s.Start(t, kv.InitFactoryForLocalTestCluster)
//	defer s.Stop()
//
// Note that the LocalTestCluster is different from server.TestCluster
// in that although it uses a distributed sender, there is no RPC traffic.
type LocalTestCluster struct {
	AmbientCtx        log.AmbientContext
	Cfg               kvserver.StoreConfig
	Manual            *timeutil.ManualTime
	Clock             *hlc.Clock
	Gossip            *gossip.Gossip
	Eng               storage.Engine
	Store             *kvserver.Store
	StoreTestingKnobs *kvserver.StoreTestingKnobs
	dbContext         *kv.DBContext
	DB                *kv.DB
	Stores            *kvserver.Stores
	stopper           *stop.Stopper
	Latency           time.Duration // sleep for each RPC sent
	tester            testing.TB

	// DisableLivenessHeartbeat, if set, inhibits the heartbeat loop. Some tests
	// need this because, for example, the heartbeat loop increments some
	// transaction metrics.
	// However, note that without heartbeats, ranges with epoch-based leases
	// cannot be accessed because the leases cannot be granted.
	// See also DontCreateSystemRanges.
	DisableLivenessHeartbeat bool

	// DontCreateSystemRanges, if set, makes the cluster start with a single
	// range, not with all the system ranges (as regular cluster start).
	// If DisableLivenessHeartbeat is set, you probably want to also set this so
	// that ranges requiring epoch-based leases are not created automatically.
	DontCreateSystemRanges bool
}

// InitFactoryFn is a callback used to initiate the txn coordinator
// sender factory (we don't do it directly from this package to avoid
// a dependency on kv).
type InitFactoryFn func(
	ctx context.Context,
	st *cluster.Settings,
	nodeDesc *roachpb.NodeDescriptor,
	tracer *tracing.Tracer,
	clock *hlc.Clock,
	latency time.Duration,
	stores kv.Sender,
	stopper *stop.Stopper,
	gossip *gossip.Gossip,
) kv.TxnSenderFactory

// Stopper returns the Stopper.
func (ltc *LocalTestCluster) Stopper() *stop.Stopper {
	return ltc.stopper
}

// Start starts the test cluster by bootstrapping an in-memory store
// (defaults to maximum of 50M). The server is started, launching the
// node RPC server and all HTTP endpoints. Use the value of
// TestServer.Addr after Start() for client connections. Use Stop()
// to shutdown the server after the test completes.
func (ltc *LocalTestCluster) Start(t testing.TB, initFactory InitFactoryFn) {
	manualClock := timeutil.NewManualTime(timeutil.Unix(0, 123))
	clock := hlc.NewClock(manualClock,
		50*time.Millisecond /* maxOffset */, 50*time.Millisecond /* toleratedOffset */, logger.CRDBLogger)
	var cfg kvserver.StoreConfig
	if ltc.StoreTestingKnobs != nil && ltc.StoreTestingKnobs.InitialReplicaVersionOverride != nil {
		cfg = kvserver.TestStoreConfigWithVersion(clock, *ltc.StoreTestingKnobs.InitialReplicaVersionOverride)
	} else {
		cfg = kvserver.TestStoreConfig(clock)
	}
	tr := cfg.AmbientCtx.Tracer
	ltc.stopper = stop.NewStopper(stop.WithTracer(tr))
	ltc.Manual = manualClock
	ltc.Clock = clock
	ambient := cfg.AmbientCtx

	nc := &base.NodeIDContainer{}
	ambient.AddLogTag("n", nc)
	ltc.AmbientCtx = ambient
	ctx := ambient.AnnotateCtx(context.Background())

	nodeID := roachpb.NodeID(1)
	nodeDesc := &roachpb.NodeDescriptor{
		NodeID:  nodeID,
		Address: util.MakeUnresolvedAddr("tcp", "invalid.invalid:26257"),
	}

	ltc.tester = t

	opts := rpc.DefaultContextOptions()
	opts.Clock = ltc.Clock.WallClock()
	opts.ToleratedOffset = ltc.Clock.ToleratedOffset()
	opts.Stopper = ltc.stopper
	opts.Settings = cfg.Settings
	opts.NodeID = nc
	opts.TenantRPCAuthorizer = tenantcapabilitiesauthorizer.NewAllowEverythingAuthorizer()

	cfg.RPCContext = rpc.NewContext(ctx, opts)

	cfg.RPCContext.NodeID.Set(ctx, nodeID)
	clusterID := cfg.RPCContext.StorageClusterID
	ltc.Gossip = gossip.New(ambient, clusterID, nc, ltc.stopper, metric.NewRegistry(), roachpb.Locality{})
	var err error
	ltc.Eng, err = storage.Open(
		ctx,
		storage.InMemory(),
		cfg.Settings,
		storage.CacheSize(1<<20 /* 1 MiB */),
		storage.MaxSizeBytes(50<<20 /* 50 MiB */),
	)
	if err != nil {
		t.Fatal(err)
	}
	ltc.stopper.AddCloser(ltc.Eng)

	ltc.Stores = kvserver.NewStores(ambient, ltc.Clock)
	// Faster refresh intervals for testing.
	cfg.NodeCapacityProvider = load.NewNodeCapacityProvider(ltc.stopper, ltc.Stores, load.NodeCapacityProviderConfig{
		CPUUsageRefreshInterval:    10 * time.Millisecond,
		CPUCapacityRefreshInterval: 10 * time.Millisecond,
		CPUUsageMovingAverageAge:   20,
	})

	factory := initFactory(ctx, cfg.Settings, nodeDesc, ltc.stopper.Tracer(), ltc.Clock, ltc.Latency, ltc.Stores, ltc.stopper, ltc.Gossip)

	var nodeIDContainer base.NodeIDContainer
	nodeIDContainer.Set(context.Background(), nodeID)

	ltc.dbContext = &kv.DBContext{
		UserPriority: roachpb.NormalUserPriority,
		NodeID:       base.NewSQLIDContainerForNode(&nodeIDContainer),
		Settings:     cfg.Settings,
		Stopper:      ltc.stopper,
	}
	ltc.DB = kv.NewDBWithContext(cfg.AmbientCtx, factory, ltc.Clock, *ltc.dbContext)

	// By default, disable the replica scanner and split queue, which
	// confuse tests using LocalTestCluster.
	if ltc.StoreTestingKnobs == nil {
		cfg.TestingKnobs.DisableScanner = true
		cfg.TestingKnobs.DisableSplitQueue = true
	} else {
		cfg.TestingKnobs = *ltc.StoreTestingKnobs
	}
	cfg.DB = ltc.DB
	cfg.Gossip = ltc.Gossip
	cfg.HistogramWindowInterval = metric.TestSampleInterval
	active, renewal := cfg.NodeLivenessDurations()
	cfg.NodeLiveness = liveness.NewNodeLiveness(liveness.NodeLivenessOptions{
		AmbientCtx:              cfg.AmbientCtx,
		Stopper:                 ltc.stopper,
		Clock:                   cfg.Clock,
		Storage:                 liveness.NewKVStorage(cfg.DB),
		Cache:                   liveness.NewCache(cfg.Gossip, cfg.Clock, cfg.Settings, cfg.NodeDialer),
		LivenessThreshold:       active,
		RenewalDuration:         renewal,
		HistogramWindowInterval: cfg.HistogramWindowInterval,
		Engines:                 []storage.Engine{ltc.Eng},
	})
	liveness.TimeUntilNodeDead.Override(ctx, &cfg.Settings.SV, liveness.TestTimeUntilNodeDead)
	{
		livenessInterval, heartbeatInterval := cfg.StoreLivenessDurations()
		supportGracePeriod := cfg.RPCContext.StoreLivenessWithdrawalGracePeriod()
		options := storeliveness.NewOptions(livenessInterval, heartbeatInterval, supportGracePeriod)
		transport, err := storeliveness.NewTransport(
			cfg.AmbientCtx, ltc.stopper, ltc.Clock,
			nil /* dialer */, nil /* grpcServer */, nil /* drpcServer */, nil, /* knobs */
		)
		if err != nil {
			t.Fatal(err)
		}
		knobs := cfg.TestingKnobs.StoreLivenessKnobs
		cfg.StoreLiveness = storeliveness.NewNodeContainer(ltc.stopper, options, transport, knobs)
	}
	nodeCountFn := func() int {
		var count int
		for _, nv := range cfg.NodeLiveness.ScanNodeVitalityFromCache() {
			if !nv.IsDecommissioning() && !nv.IsDecommissioned() {
				count++
			}
		}
		return count
	}
	cfg.StorePool = storepool.NewStorePool(
		cfg.AmbientCtx,
		cfg.Settings,
		cfg.Gossip,
		cfg.Clock,
		nodeCountFn,
		storepool.MakeStorePoolNodeLivenessFunc(cfg.NodeLiveness),
		/* deterministic */ false,
	)
	cfg.MMAllocator = mmaprototype.NewAllocatorState(timeutil.DefaultTimeSource{},
		rand.New(rand.NewSource(timeutil.Now().UnixNano())))

	cfg.Transport = kvserver.NewDummyRaftTransport(cfg.AmbientCtx, cfg.Settings, ltc.Clock)
	cfg.ClosedTimestampReceiver = sidetransport.NewReceiver(nc, ltc.stopper, ltc.Stores, nil /* testingKnobs */)

	if err := kvstorage.WriteClusterVersion(ctx, ltc.Eng, clusterversion.TestingClusterVersion); err != nil {
		t.Fatalf("unable to write cluster version: %s", err)
	}
	if err := kvstorage.InitEngine(
		ctx, ltc.Eng, roachpb.StoreIdent{NodeID: nodeID, StoreID: 1},
	); err != nil {
		t.Fatalf("unable to start local test cluster: %s", err)
	}

	rangeFeedFactory, err := rangefeed.NewFactory(ltc.stopper, ltc.DB, cfg.Settings, nil /* knobs */)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SpanConfigSubscriber = spanconfigkvsubscriber.New(
		clock,
		rangeFeedFactory,
		keys.SpanConfigurationsTableID,
		1<<20, /* 1 MB */
		cfg.DefaultSpanConfig,
		cfg.Settings,
		spanconfigstore.NewEmptyBoundsReader(),
		nil, /* knobs */
		nil, /* registry */
	)
	cfg.SystemConfigProvider = systemconfigwatcher.New(
		keys.SystemSQLCodec,
		cfg.Clock,
		rangeFeedFactory,
		zonepb.DefaultZoneConfigRef(),
	)

	ltc.Store = kvserver.NewStore(ctx, cfg, ltc.Eng, nodeDesc)

	var initialValues []roachpb.KeyValue
	var splits []roachpb.RKey
	if !ltc.DontCreateSystemRanges {
		schema := bootstrap.MakeMetadataSchema(
			keys.SystemSQLCodec, zonepb.DefaultZoneConfigRef(), zonepb.DefaultSystemZoneConfigRef(),
		)
		var tableSplits []roachpb.RKey
		initialValues, tableSplits = schema.GetInitialValues()
		splits = append(config.StaticSplits(), tableSplits...)
		sort.Slice(splits, func(i, j int) bool {
			return splits[i].Less(splits[j])
		})
	}

	if err := kvserver.WriteInitialClusterData(
		ctx,
		ltc.Eng,
		initialValues,
		clusterversion.Latest.Version(),
		1, /* numStores */
		splits,
		ltc.Clock.PhysicalNow(),
		cfg.TestingKnobs,
	); err != nil {
		t.Fatalf("unable to start local test cluster: %s", err)
	}

	// The heartbeat loop depends on gossip to retrieve the node ID, so we're
	// sure to set it first.
	nc.Set(ctx, nodeDesc.NodeID)
	if err := ltc.Gossip.SetNodeDescriptor(nodeDesc); err != nil {
		t.Fatalf("unable to set node descriptor: %s", err)
	}

	if err := ltc.Store.Start(ctx, ltc.stopper); err != nil {
		t.Fatalf("unable to start local test cluster: %s", err)
	}

	ltc.Stores.AddStore(ltc.Store)

	if !ltc.DisableLivenessHeartbeat {
		cfg.NodeLiveness.Start(ctx)
	}

	ltc.Cfg = cfg
}

// Stop stops the cluster.
func (ltc *LocalTestCluster) Stop() {
	// If the test has failed, we don't attempt to clean up: This often hangs,
	// and leaktest will disable itself for the remaining tests so that no
	// unrelated errors occur from a dirty shutdown.
	if ltc.tester.Failed() {
		return
	}
	if r := recover(); r != nil {
		panic(r)
	}
	ltc.stopper.Stop(context.TODO())
}
