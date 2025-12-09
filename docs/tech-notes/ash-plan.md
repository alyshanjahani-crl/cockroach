---
name: ASH Feature Design
overview: Design an Active Session History (ASH) feature for CockroachDB that samples active SQL sessions at 1-second intervals to capture wait event data for performance analysis and troubleshooting.
todos:
  - id: phase1-package
    content: Create pkg/sql/ash/ package with sampler, classifier, buffer types
    status: pending
  - id: phase1-labels
    content: Add pprof label tagging in connExecutor for session/txn/stmt IDs
    status: pending
  - id: phase1-sampler
    content: Implement goroutine stack sampler using allstacks.Get()
    status: pending
  - id: phase1-classifier
    content: Basic wait event classifier (CPU, LOCK, KV, IO, OTHER)
    status: pending
  - id: phase1-buffer
    content: In-memory ring buffer for ASH samples
    status: pending
  - id: phase1-vtable
    content: Add crdb_internal.node_active_session_history virtual table
    status: pending
  - id: phase1-setting
    content: Add sql.ash.enabled cluster setting
    status: pending
  - id: phase2-taxonomy
    content: Complete wait event taxonomy with all 15+ categories
    status: pending
  - id: phase2-schema
    content: Create system.active_session_history table with TTL
    status: pending
  - id: phase2-writer
    content: Implement batch writer with backpressure
    status: pending
  - id: phase2-cluster
    content: Cluster-wide virtual table via status RPC
    status: pending
  - id: phase2-markers
    content: Add explicit wait markers at lock_table, admission_control
    status: pending
  - id: phase3-tracing
    content: Integrate with existing tracing spans
    status: pending
  - id: phase3-raft-storage
    content: Add wait markers for Raft propose and Pebble commits
    status: pending
  - id: phase3-ui
    content: DB Console visualizations for ASH data
    status: pending
---

# Active Session History (ASH) Design for CockroachDB

## Executive Summary

This plan outlines a comprehensive ASH implementation for CockroachDB, leveraging existing infrastructure while adding targeted instrumentation. The design uses a hybrid approach: goroutine stack sampling for broad coverage plus explicit wait markers for critical paths.

---

## Part 1: Key Code Locations for Instrumentation

### 1.1 Session/Transaction/Statement Identity

| Component | Location | Key Types |

|-----------|----------|-----------|

| Session ID | [`pkg/sql/conn_executor.go`](pkg/sql/conn_executor.go) | `connExecutor`, `extendedEvalCtx.SessionID` (clusterunique.ID) |

| Session Registry | [`pkg/sql/exec_util.go`](pkg/sql/exec_util.go):2744 | `SessionRegistry` - maps sessionsByID |

| Transaction ID | [`pkg/kv/kvclient/kvcoord/txn_coord_sender.go`](pkg/kv/kvclient/kvcoord/txn_coord_sender.go):538 | `tc.mu.txn.ID` (uuid.UUID) |

| Statement/Query ID | [`pkg/sql/statement.go`](pkg/sql/statement.go):28 | `Statement.QueryID` (clusterunique.ID) |

| Statement Fingerprint | [`pkg/sql/sqlstats/ssprovider.go`](pkg/sql/sqlstats/ssprovider.go):69 | `RecordedStmtStats.FingerprintID` |

| Gateway Node | [`pkg/sql/conn_executor.go`](pkg/sql/conn_executor.go):1213 | `nodeIDOrZero` from NodeInfo |

**Key Insight**: The `connExecutor` already tracks `goroutineID` (line 4597) which maps sessions to goroutines.

### 1.2 Request Execution Paths

**SQL Layer**:

- Entry: [`pkg/sql/conn_executor_exec.go`](pkg/sql/conn_executor_exec.go):115 `execStmt()`
- Statement execution: [`pkg/sql/conn_executor_exec.go`](pkg/sql/conn_executor_exec.go):356 `execStmtInOpenState()`
- Creates tracing span at line 366: `tracing.ChildSpan(ctx, "sql query")`

**KV Client Layer**:

- DistSender entry: [`pkg/kv/kvclient/kvcoord/dist_sender.go`](pkg/kv/kvclient/kvcoord/dist_sender.go):1259 `Send()`
- Creates span at line 1271: `tracing.ChildSpan(ctx, "dist sender send")`
- TxnCoordSender: [`pkg/kv/kvclient/kvcoord/txn_coord_sender.go`](pkg/kv/kvclient/kvcoord/txn_coord_sender.go):510

**KV Server / Concurrency**:

- Lock table wait: [`pkg/kv/kvserver/concurrency/lock_table_waiter.go`](pkg/kv/kvserver/concurrency/lock_table_waiter.go):391 `WaitOn()`
- Latch acquisition: [`pkg/kv/kvserver/concurrency/concurrency_manager.go`](pkg/kv/kvserver/concurrency/concurrency_manager.go):357

**Raft Layer**:

- Proposal: [`pkg/kv/kvserver/replica_raft.go`](pkg/kv/kvserver/replica_raft.go):404 `propose()`
- Application: [`pkg/kv/kvserver/replica_application_decoder.go`](pkg/kv/kvserver/replica_application_decoder.go):136

**Storage Layer**:

- Batch commit: [`pkg/storage/pebble_batch.go`](pkg/storage/pebble_batch.go):354 `Commit()`
- Sync wait: [`pkg/storage/pebble_batch.go`](pkg/storage/pebble_batch.go):398 `SyncWait()`

**Admission Control**:

- Work queue wait: [`pkg/util/admission/work_queue.go`](pkg/util/admission/work_queue.go):567 `Admit()`
- Creates span at line 769: `tracing.ChildSpan(ctx, "admissionWorkQueueWait")`

### 1.3 Existing Infrastructure to Leverage

| Infrastructure | Location | Use for ASH |

|---------------|----------|-------------|

| Tracing | [`pkg/util/tracing/`](pkg/util/tracing/) | Context propagation, span hierarchy |

| pprof labels | [`pkg/util/pprofutil/labels.go`](pkg/util/pprofutil/labels.go) | Tag goroutines with session/txn IDs |

| allstacks | [`pkg/util/allstacks/allstacks.go`](pkg/util/allstacks/allstacks.go) | Goroutine stack capture |

| Session registry | [`pkg/sql/exec_util.go`](pkg/sql/exec_util.go):2746 | Enumerate active sessions |

| crdb_internal tables | [`pkg/sql/crdb_internal.go`](pkg/sql/crdb_internal.go) | Pattern for virtual tables |

| Statement stats | [`pkg/sql/sqlstats/`](pkg/sql/sqlstats/) | Stats collection pattern |

| Env sampler | [`pkg/server/env_sampler.go`](pkg/server/env_sampler.go):171 | Background sampling pattern |

---

## Part 2: Goroutine-Based Sampling Design

### 2.1 Tagging Goroutines with Session Context

**Recommended Approach**: Use `pprof.SetGoroutineLabels()` to tag goroutines at key entry points.

```go
// In pkg/sql/conn_executor.go, at session start
import "runtime/pprof"

func (ex *connExecutor) run(ctx context.Context, ...) error {
    labels := pprof.Labels(
        "session_id", ex.planner.extendedEvalCtx.SessionID.String(),
        "app", ex.applicationName.Load().(string),
    )
    ctx = pprof.WithLabels(ctx, labels)
    pprof.SetGoroutineLabels(ctx)
    // ... existing code
}
```

**Update labels when transaction/statement starts** in `execStmtInOpenState()`:

```go
labels := pprof.Labels(
    "session_id", ex.planner.extendedEvalCtx.SessionID.String(),
    "txn_id", ex.state.mu.txn.ID().String(),
    "stmt_id", stmt.QueryID.String(),
)
```

### 2.2 Sampler Architecture

```go
// pkg/sql/ash/sampler.go (new package)
type ASHSampler struct {
    nodeID        roachpb.NodeID
    sessionReg    *SessionRegistry
    interval      time.Duration
    classifier    *WaitEventClassifier
    buffer        *RingBuffer[ASHSample]
    stopper       *stop.Stopper
}

func (s *ASHSampler) Run(ctx context.Context) {
    ticker := time.NewTicker(s.interval)
    for {
        select {
        case <-ticker.C:
            s.takeSample(ctx)
        case <-s.stopper.ShouldQuiesce():
            return
        }
    }
}

func (s *ASHSampler) takeSample(ctx context.Context) {
    // 1. Get goroutine stacks (uses runtime.Stack)
    stacks := allstacks.Get()
    
    // 2. Parse stacks and extract pprof labels
    goroutines := parseGoroutineStacks(stacks)
    
    // 3. For each goroutine with session labels, classify wait event
    for _, g := range goroutines {
        if sessionID := g.Labels["session_id"]; sessionID != "" {
            sample := ASHSample{
                SampleTime:   timeutil.Now(),
                NodeID:       s.nodeID,
                SessionID:    sessionID,
                TxnID:        g.Labels["txn_id"],
                StmtID:       g.Labels["stmt_id"],
                WaitEvent:    s.classifier.Classify(g.Stack),
                GoroutineID:  g.ID,
            }
            s.buffer.Add(sample)
        }
    }
}
```

### 2.3 Performance Considerations

**Cost of `runtime.Stack(all=true)`**:

- Stops the world briefly (typically <1ms for <10K goroutines)
- Memory: ~1MB for typical workload, up to 512MB cap
- At 1s intervals: <0.1% CPU overhead in typical scenarios

**Mitigations**:

- Make sampling interval configurable (default 1s, min 100ms)
- Add jitter to prevent thundering herd across nodes
- Cap sample buffer size to limit memory
- Feature flag to disable entirely

---

## Part 3: Wait Event Taxonomy

### 3.1 Top-Level Categories

| Category | Description |

|----------|-------------|

| `CPU` | On-CPU execution (no blocking call in stack) |

| `SQL_PARSE` | SQL parsing |

| `SQL_PLAN` | Query optimization/planning |

| `SQL_EXEC` | SQL execution (row processing, result buffering) |

| `KV_CLIENT` | DistSender batch send, range lookup, retry |

| `KV_LOCK` | Lock table wait, latch acquisition |

| `ADMISSION` | Admission control queuing |

| `RAFT_PROPOSE` | Raft proposal submission |

| `RAFT_APPLY` | Raft log application |

| `STORAGE_WRITE` | Pebble write/commit |

| `STORAGE_READ` | Pebble read/iteration |

| `NETWORK_RPC` | gRPC send/receive |

| `NETWORK_STREAM` | DistSQL flow streaming |

| `BACKGROUND` | Background jobs, GC, maintenance |

| `SYSTEM` | Range operations, descriptor lease |

| `OTHER` | Unclassified |

### 3.2 Stack Pattern Mapping

```go
// pkg/sql/ash/classifier.go
type WaitEventClassifier struct {
    rules []ClassificationRule
}

type ClassificationRule struct {
    Category    WaitEventType
    SubEvent    string
    Patterns    []string  // function name patterns
    Priority    int       // higher = checked first
}

var defaultRules = []ClassificationRule{
    // Lock waits
    {KV_LOCK, "lock_table_wait", []string{
        "concurrency.(*lockTableWaiterImpl).WaitOn",
        "concurrency.(*lockTableGuardImpl).wait",
    }, 100},
    
    // Latch waits
    {KV_LOCK, "latch_wait", []string{
        "concurrency.(*managerImpl).lm.Acquire",
        "spanlatch.(*Manager).Acquire",
    }, 99},
    
    // Admission control
    {ADMISSION, "work_queue_wait", []string{
        "admission.(*WorkQueue).Admit",
        "admissionWorkQueueWait",
    }, 98},
    
    // Raft
    {RAFT_PROPOSE, "raft_propose", []string{
        "replica.(*Replica).propose",
        "proposalBuf.FlushLockedWithRaftGroup",
    }, 95},
    
    // Storage
    {STORAGE_WRITE, "pebble_commit", []string{
        "pebble.(*Batch).Commit",
        "pebble.(*DB).Apply",
    }, 90},
    
    // Network
    {NETWORK_RPC, "grpc_call", []string{
        "grpc.(*clientStream).SendMsg",
        "grpc.(*clientStream).RecvMsg",
    }, 85},
    
    // SQL planning
    {SQL_PLAN, "optimize", []string{
        "opt.(*Builder).Build",
        "optbuilder",
    }, 80},
}
```

---

## Part 4: Complementary Instrumentation Approaches

### 4.1 Explicit Wait Markers

Add lightweight markers at critical blocking points:

```go
// pkg/sql/ash/wait_state.go
type WaitState struct {
    Event     WaitEventType
    SubEvent  string
    StartTime time.Time
    Details   string  // e.g., lock key, range ID
}

// Thread-local wait state using goroutine-local storage pattern
var currentWaitState sync.Map  // goroutineID -> *WaitState

func SetWaitState(event WaitEventType, subEvent, details string) func() {
    gid := goid.Get()
    ws := &WaitState{
        Event: event, SubEvent: subEvent,
        StartTime: timeutil.Now(), Details: details,
    }
    currentWaitState.Store(gid, ws)
    return func() { currentWaitState.Delete(gid) }
}
```

**Instrumentation points**:

- [`pkg/kv/kvserver/concurrency/lock_table_waiter.go`](pkg/kv/kvserver/concurrency/lock_table_waiter.go):413
- [`pkg/util/admission/work_queue.go`](pkg/util/admission/work_queue.go):769
- [`pkg/kv/kvserver/replica_raft.go`](pkg/kv/kvserver/replica_raft.go):404

### 4.2 Tracing Integration

Leverage existing tracing spans:

```go
// During sampling, also check active spans
func (s *ASHSampler) enrichWithTracing(sample *ASHSample, sessionID string) {
    if session, ok := s.sessionReg.GetSessionByID(sessionID); ok {
        if span := session.CurrentSpan(); span != nil {
            sample.SpanName = span.OperationName()
            sample.TraceID = span.TraceID()
        }
    }
}
```

---

## Part 5: Storage Schema and SQL Surface

### 5.1 Internal Table Schema

```sql
-- system.active_session_history
CREATE TABLE system.active_session_history (
    sample_time      TIMESTAMPTZ NOT NULL,
    node_id          INT8 NOT NULL,
    session_id       STRING NOT NULL,
    txn_id           UUID,
    stmt_id          STRING,
    stmt_fingerprint BYTES,  -- statement fingerprint ID
    wait_event_type  STRING NOT NULL,
    wait_event       STRING NOT NULL,
    wait_details     STRING,  -- lock key, range ID, etc.
    application_name STRING,
    database_name    STRING,
    user_name        STRING,
    client_address   STRING,
    is_on_cpu        BOOL NOT NULL DEFAULT false,
    goroutine_id     INT8,
    
    PRIMARY KEY (sample_time, node_id, session_id)
        USING HASH WITH (bucket_count = 8),
    INDEX ash_time_idx (sample_time DESC),
    INDEX ash_session_idx (session_id, sample_time DESC),
    INDEX ash_wait_event_idx (wait_event_type, wait_event, sample_time DESC)
) WITH (
    ttl = 'on',
    ttl_expiration_expression = '((sample_time) + INTERVAL ''24 hours'')',
    ttl_job_cron = '@hourly'
);
```

### 5.2 Virtual Tables

```sql
-- crdb_internal.active_session_history (current node, in-memory buffer)
-- crdb_internal.cluster_active_session_history (all nodes via RPC)
-- crdb_internal.ash_wait_events (wait event taxonomy reference)
```

### 5.3 Useful Views

```sql
-- Top wait events in last 10 minutes
CREATE VIEW crdb_internal.ash_top_waits AS
SELECT 
    wait_event_type,
    wait_event,
    count(*) as sample_count,
    count(DISTINCT session_id) as session_count
FROM system.active_session_history
WHERE sample_time > now() - INTERVAL '10 minutes'
GROUP BY wait_event_type, wait_event
ORDER BY sample_count DESC;
```

### 5.4 Write Strategy

- Buffer samples in-memory ring buffer (configurable size, default 3600 = 1 hour at 1s)
- Batch flush to system table every 10s
- Use async writer goroutine to avoid blocking sampler
- Implement backpressure if writes fall behind

---

## Part 6: Implementation Phases

### Phase 1: Minimal Prototype (2-3 weeks)

**Tasks**:

1. Create `pkg/sql/ash/` package structure
2. Add pprof label tagging in `connExecutor.run()` and `execStmtInOpenState()`
3. Implement basic stack sampler using `allstacks.Get()`
4. Simple classifier with 5 categories: CPU, LOCK, KV, IO, OTHER
5. In-memory ring buffer only
6. Add `crdb_internal.node_active_session_history` virtual table
7. Cluster setting `sql.ash.enabled` (default false)

**Files to modify**:

- [`pkg/sql/conn_executor.go`](pkg/sql/conn_executor.go) - add pprof labels
- [`pkg/sql/conn_executor_exec.go`](pkg/sql/conn_executor_exec.go) - update labels per statement
- New: `pkg/sql/ash/sampler.go`, `classifier.go`, `buffer.go`
- [`pkg/sql/crdb_internal.go`](pkg/sql/crdb_internal.go) - add virtual table
- [`pkg/server/server.go`](pkg/server/server.go) - start sampler

### Phase 2: Full Taxonomy and Persistence (3-4 weeks)

**Tasks**:

1. Complete wait event taxonomy (all 15+ categories)
2. Implement system table with TTL
3. Add batch writer with backpressure
4. Cluster-wide virtual table via RPC
5. Add explicit wait markers at 3-5 critical paths
6. Dashboard integration (basic)

**Files to modify**:

- [`pkg/sql/catalog/systemschema/system.go`](pkg/sql/catalog/systemschema/system.go) - add schema
- [`pkg/kv/kvserver/concurrency/lock_table_waiter.go`](pkg/kv/kvserver/concurrency/lock_table_waiter.go) - wait markers
- [`pkg/util/admission/work_queue.go`](pkg/util/admission/work_queue.go) - wait markers
- [`pkg/server/serverpb/status.proto`](pkg/server/serverpb/status.proto) - RPC for cluster view

### Phase 3: Production Hardening (2-3 weeks)

**Tasks**:

1. Tracing span integration
2. Additional explicit wait markers (Raft, Pebble)
3. DB Console visualizations
4. Performance testing and tuning
5. Documentation

**Files to modify**:

- [`pkg/kv/kvserver/replica_raft.go`](pkg/kv/kvserver/replica_raft.go) - Raft wait markers
- [`pkg/storage/pebble_batch.go`](pkg/storage/pebble_batch.go) - storage wait markers
- UI components for visualization

---

## Risks and Mitigations

| Risk | Mitigation |

|------|------------|

| Stack sampling STW pause | Configurable interval, feature flag |

| Memory pressure from buffer | Capped ring buffer, configurable size |

| Write amplification from persistence | Batch writes, async flush, TTL |

| ASH queries impacting production | Rate limiting, query timeout hints |

| Incomplete coverage | Hybrid approach (sampling + explicit markers) |