// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package config

import (
	"time"

	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
)

const (
	defaultTickInteval             = 500 * time.Millisecond
	defaultMetricsInterval         = 10 * time.Second
	defaultReplicaChangeBaseDelay  = 100 * time.Millisecond
	defaultRebalancingSnapshotRate = 32 << 20 // 32MiB/s
	defaultSplitQueueDelay         = 100 * time.Millisecond
	defaultRangeSizeSplitThreshold = 512 << 20 // 512MiB
	defaultRangeRebalanceThreshold = 0.05
	defaultPacerLoopInterval       = 10 * time.Minute
	defaultPacerMinIterInterval    = 10 * time.Millisecond
	defaultPacerMaxIterIterval     = 1 * time.Second
	defaultStateExchangeInterval   = 10 * time.Second
	defaultStateExchangeDelay      = 500 * time.Millisecond
	defaultSplitQPSThreshold       = 2500
	defaultSplitStatRetention      = 10 * time.Minute
	defaultSeed                    = 42
	defaultLBRebalancingInterval   = time.Minute
	defaultLBRebalanceQPSThreshold = 0.1
	defaultLBRebalancingObjective  = 0 // QPS
)

var (
	// defaultStartTime is used as the default beginning time for simulation
	// runs. It isn't necessarily meaningful other than for logging and having
	// "some" start time for components taking a time.Time.
	defaultStartTime = time.Date(2022, 03, 21, 11, 0, 0, 0, time.UTC)
)

// SimulationSettings controls
// WIP: Thread these settings through to each of the sim parts.
type SimulationSettings struct {
	// StartTime is the time to start the simulation at. This is also used to
	// init the shared state simulation clock.
	StartTime time.Time
	// TickInterval is the duration between simulator ticks. The lower this
	// setting, the higher resolution the simulation will be. A lower
	// TickInterval will also take longer to execute so a tradeoff exists.
	TickInterval time.Duration
	// MetricsInterval is the interval at which metrics are recorded.
	MetricsInterval time.Duration
	// Seed is the random source that will be used for any simulator components
	// that accept a seed.
	Seed int64
	// ReplicaChangeBaseDelay is the base delay for all replica movements
	// (add,remove). It accounts for a fixed overhead of initiating a replica
	// movement.
	ReplicaChangeBaseDelay time.Duration
	// RebalancingSnapshotRate is rate at which newly added replicas will be
	// added based on the range size. e.g., When the range size is 32MiB, and the
	// RebalancingSnapshotRate is 32 << 20, the delay for adding a replica wil be
	// 1 second + ReplicaChangeBaseDelay.
	RebalancingSnapshotRate int64
	// SplitQueueDelay is the delay that range splits take to complete.
	SplitQueueDelay time.Duration
	// RangeSizeSplitThreshold is the threshold in MB, below which ranges will
	// not attempted to be split due to size.
	RangeSizeSplitThreshold int64
	// RangeRebalanceThreshold is the minimum ratio of a store's range count to
	// the mean range count at which that store is considered overfull or underfull
	// of ranges.
	RangeRebalanceThreshold float64
	// PacerLoopInterval is the period over which the pacer will visit every
	// replica e.g. If the period is 10 minutes, the pacer will attempt to
	// visit every replica on the store 10 minute window, so if there are 1000
	// replicas, 100 replicas per minute.
	PacerLoopInterval time.Duration
	// PacerMinIterInterval is the minimum amount of time the pacer may wait
	// between visiting replicas.
	PacerMinIterInterval time.Duration
	// PacerMaxIterIterval is the maximum amount of time the pacer may wait
	// between visiting replicas.
	PacerMaxIterIterval time.Duration
	// StateExchangeInterval is the interval at which state updates will be
	// broadcast to other stores.
	StateExchangeInterval time.Duration
	// StateExchangeDelay is the delay between sending a state update and all
	// other stores receiving the update.
	StateExchangeDelay time.Duration
	// SplitQPSThreshold is the threshold above which a range will be a
	// candidate for load based splitting.
	SplitQPSThreshold float64
	// SplitStatRetention is the duration which recorded load will be retained
	// and factored into load based splitting decisions.
	SplitStatRetention time.Duration
	// LBRebalancingObjective is the load objective to balance.
	LBRebalancingObjective int64
	// LBRebalancingInterval controls how often the store rebalancer will
	// consider opportunities for rebalancing.
	LBRebalancingInterval time.Duration
	// ReplicateQueueEnabled controls whether the replicate queue is enabled.
	ReplicateQueueEnabled bool
	// LeaseQueueEnabled controls whether the lease queue is enabled.
	LeaseQueueEnabled bool
	// SplitQueueEnabled controls whether the split queue is enabled.
	SplitQueueEnabled bool
	// st is used to update cockroach cluster settings.
	//
	// TODO(wenyihu6): Remove any non-simulation settings from this struct and
	// instead override the settings below.
	ST *cluster.Settings
}

// DefaultSimulationSettings returns a set of default settings for simulation.
func DefaultSimulationSettings() *SimulationSettings {
	return &SimulationSettings{
		StartTime:               defaultStartTime,
		TickInterval:            defaultTickInteval,
		MetricsInterval:         defaultMetricsInterval,
		Seed:                    defaultSeed,
		ReplicaChangeBaseDelay:  defaultReplicaChangeBaseDelay,
		RebalancingSnapshotRate: defaultRebalancingSnapshotRate,
		SplitQueueDelay:         defaultSplitQueueDelay,
		RangeSizeSplitThreshold: defaultRangeSizeSplitThreshold,
		RangeRebalanceThreshold: defaultRangeRebalanceThreshold,
		PacerLoopInterval:       defaultPacerLoopInterval,
		PacerMinIterInterval:    defaultPacerMinIterInterval,
		PacerMaxIterIterval:     defaultPacerMaxIterIterval,
		StateExchangeInterval:   defaultStateExchangeInterval,
		StateExchangeDelay:      defaultStateExchangeDelay,
		SplitQPSThreshold:       defaultSplitQPSThreshold,
		SplitStatRetention:      defaultSplitStatRetention,
		LBRebalancingObjective:  defaultLBRebalancingObjective,
		LBRebalancingInterval:   defaultLBRebalancingInterval,
		ReplicateQueueEnabled:   true,
		LeaseQueueEnabled:       true,
		SplitQueueEnabled:       true,
		ST:                      cluster.MakeClusterSettings(),
	}
}

// ReplicaChangeDelayFn returns a function which calculates the delay for
// adding a replica based on the range size.
func (s *SimulationSettings) ReplicaChangeDelayFn() func(rangeSize int64, add bool) time.Duration {
	return func(rangeSize int64, add bool) time.Duration {
		delay := s.ReplicaChangeBaseDelay
		if add {
			estimatedTimeToAddReplica := float64(rangeSize) / float64(s.RebalancingSnapshotRate)
			// To avoid truncating the decimal part of the result, cast to Duration
			// after float multiplication.
			delay += time.Duration(estimatedTimeToAddReplica * float64(time.Second))
		}
		return delay
	}
}

// RangeSplitDelayFn returns a function which calculates the delay for
// splitting a range.
func (s *SimulationSettings) RangeSplitDelayFn() func() time.Duration {
	return func() time.Duration {
		return s.SplitQueueDelay
	}
}

// SplitQPSThresholdFn returns a function that returns the current QPS split
// threshold for load based splitting of a range.
func (s *SimulationSettings) SplitQPSThresholdFn() func() float64 {
	return func() float64 {
		return s.SplitQPSThreshold
	}
}

// SplitQPSRetentionFn returns a function that returns the current QPS
// retention duration for load recorded against a range, used in load based
// split decisions.
func (s *SimulationSettings) SplitQPSRetentionFn() func() time.Duration {
	return func() time.Duration {
		return s.SplitStatRetention
	}
}
