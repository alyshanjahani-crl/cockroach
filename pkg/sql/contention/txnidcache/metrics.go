// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package txnidcache

import "github.com/cockroachdb/cockroach/pkg/util/metric"

// Metrics is a struct that include all metrics related to txn id cache.
type Metrics struct {
	CacheMissCounter   *metric.Counter
	CacheReadCounter   *metric.Counter
	CacheIngestCounter *metric.Counter
	CacheRecordCounter *metric.Counter
}

var _ metric.Struct = Metrics{}

// MetricStruct implements the metric.Struct interface.
func (Metrics) MetricStruct() {}

// NewMetrics returns a new instance of Metrics.
func NewMetrics() Metrics {
	return Metrics{
		CacheMissCounter: metric.NewCounter(metric.Metadata{
			Name:        "sql.contention.txn_id_cache.miss",
			Help:        "Number of cache misses",
			Measurement: "Cache miss",
			Unit:        metric.Unit_COUNT,
		}),
		CacheReadCounter: metric.NewCounter(metric.Metadata{
			Name:        "sql.contention.txn_id_cache.read",
			Help:        "Number of cache read",
			Measurement: "Cache read",
			Unit:        metric.Unit_COUNT,
		}),
		CacheIngestCounter: metric.NewCounter(metric.Metadata{
			Name:        "sql.contention.txn_id_cache.ingest",
			Help:        "Number of times store.add is called",
			Measurement: "Add to store",
			Unit:        metric.Unit_COUNT,
		}),
		CacheRecordCounter: metric.NewCounter(metric.Metadata{
			Name:        "sql.contention.txn_id_cache.record",
			Help:        "Number of times Record is called",
			Measurement: "Cache record",
			Unit:        metric.Unit_COUNT,
		}),
	}
}
