// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package workloadid

// This package defines CRDB-wide workload identifiers that we can
// define and use to attribute observability data.
//
// We will prefer using uint64 workload identifiers because those are
// cheaper to move around the system and can help us avoid allocation
// in hot paths.
//
// String-based identifiers are useful when we want to display the
// identifier to a human, or to label go profiles and execution traces,
// which only accept string values for tags.
//
// Currently, this definition is quite simple consisting only of a
// preset list of static identifiers below that can be used numerically
// or using static strings. Statement Fingerprint IDs are the only
// example here of a dynamic ID that can have high cardinality.
//
// It is expected that future work will expand the structure and
// expressivity of the workloadID. No ID stability is guaranteed.
// While we currently do persist statement fingerprint IDs in the SQL
// Activity tables, we advise users of these values to AVOID PERSISTING
// WORKLOAD IDs until they are stable.

const ProfileTag = "workload.id"

type WorkloadID uint64

// The IDs and Names below are currently not persisted anywhere and
// are subject to change.

const (
	WORKLOAD_ID_UNKNOWN               WorkloadID = iota // 0
	WORKLOAD_ID_LDR                                     // 1
	WORKLOAD_ID_RAFT                                    // 2
	WORKLOAD_ID_STORELIVENESS                           // 3
	WORKLOAD_ID_RPC_HEARTBEAT                           // 4
	WORKLOAD_ID_NODE_LIVENESS                           // 5
	WORKLOAD_ID_SQL_LIVENESS                            // 6
	WORKLOAD_ID_TIMESERIES                              // 7
	WORKLOAD_ID_RAFT_LOG_TRUNCATION                     // 8
	WORKLOAD_ID_TXN_HEARTBEAT                           // 9
	WORKLOAD_ID_INTENT_RESOLUTION                       // 10
	WORKLOAD_ID_LEASE_ACQUISITION                       // 11
	WORKLOAD_ID_MERGE_QUEUE                             // 12
	WORKLOAD_ID_CIRCUIT_BREAKER_PROBE                   // 13
	WORKLOAD_ID_GC                                      // 14
	WORKLOAD_ID_RANGEFEED                               // 15
)

const (
	WORKLOAD_NAME_UNKNOWN               = "UNKNOWN"
	WORKLOAD_NAME_LDR                   = "LDR"
	WORKLOAD_NAME_RAFT                  = "RAFT"
	WORKLOAD_NAME_STORELIVENESS         = "STORELIVENESS"
	WORKLOAD_NAME_RPC_HEARTBEAT         = "RPC_HEARTBEAT"
	WORKLOAD_NAME_NODE_LIVENESS         = "NODE_LIVENESS"
	WORKLOAD_NAME_SQL_LIVENESS          = "SQL_LIVENESS"
	WORKLOAD_NAME_TIMESERIES            = "TIMESERIES"
	WORKLOAD_NAME_RAFT_LOG_TRUNCATION   = "RAFT_LOG_TRUNCATION"
	WORKLOAD_NAME_TXN_HEARTBEAT         = "TXN_HEARTBEAT"
	WORKLOAD_NAME_INTENT_RESOLUTION     = "INTENT_RESOLUTION"
	WORKLOAD_NAME_LEASE_ACQUISITION     = "LEASE_ACQUISITION"
	WORKLOAD_NAME_MERGE_QUEUE           = "MERGE_QUEUE"
	WORKLOAD_NAME_CIRCUIT_BREAKER_PROBE = "CIRCUIT_BREAKER_PROBE"
	WORKLOAD_NAME_GC                    = "GC"
	WORKLOAD_NAME_RANGEFEED             = "RANGEFEED"
)
