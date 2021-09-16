// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/integration/querier_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.
// +build requires_docker

package integration

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/mimir/integration/e2e"
	e2ecache "github.com/grafana/mimir/integration/e2e/cache"
	e2edb "github.com/grafana/mimir/integration/e2e/db"
	"github.com/grafana/mimir/integration/e2emimir"
	"github.com/grafana/mimir/pkg/storage/tsdb"
	"github.com/grafana/mimir/pkg/util"
)

func TestQuerierWithBlocksStorageRunningInMicroservicesMode(t *testing.T) {
	tests := map[string]struct {
		blocksShardingStrategy   string // Empty means sharding is disabled.
		tenantShardSize          int
		ingesterStreamingEnabled bool
		indexCacheBackend        string
		bucketIndexEnabled       bool
		queryShardingEnabled     bool
	}{
		"blocks sharding disabled, ingester gRPC streaming disabled, memcached index cache": {
			blocksShardingStrategy:   "",
			ingesterStreamingEnabled: false,
			// Memcached index cache is required to avoid flaky tests when the blocks sharding is disabled
			// because two different requests may hit two different store-gateways, so if the cache is not
			// shared there's no guarantee we'll have a cache hit.
			indexCacheBackend: tsdb.IndexCacheBackendMemcached,
		},
		"blocks default sharding, ingester gRPC streaming disabled, inmemory index cache": {
			blocksShardingStrategy:   "default",
			ingesterStreamingEnabled: false,
			indexCacheBackend:        tsdb.IndexCacheBackendInMemory,
		},
		"blocks default sharding, ingester gRPC streaming enabled, inmemory index cache": {
			blocksShardingStrategy:   "default",
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendInMemory,
		},
		"blocks default sharding, ingester gRPC streaming enabled, memcached index cache": {
			blocksShardingStrategy:   "default",
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendMemcached,
		},
		"blocks shuffle sharding, ingester gRPC streaming enabled, memcached index cache": {
			blocksShardingStrategy:   "shuffle-sharding",
			tenantShardSize:          1,
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendMemcached,
		},
		"blocks default sharding, ingester gRPC streaming enabled, inmemory index cache, bucket index enabled": {
			blocksShardingStrategy:   "default",
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendInMemory,
			bucketIndexEnabled:       true,
		},
		"blocks shuffle sharding, ingester gRPC streaming enabled, memcached index cache, bucket index enabled": {
			blocksShardingStrategy:   "shuffle-sharding",
			tenantShardSize:          1,
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendMemcached,
			bucketIndexEnabled:       true,
		},
		"blocks shuffle sharding, ingester gRPC streaming enabled, memcached index cache, bucket index enabled, query sharding enabled": {
			blocksShardingStrategy:   "shuffle-sharding",
			tenantShardSize:          1,
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendMemcached,
			bucketIndexEnabled:       true,
			queryShardingEnabled:     true,
		},
	}

	for testName, testCfg := range tests {
		t.Run(testName, func(t *testing.T) {
			const blockRangePeriod = 5 * time.Second

			s, err := e2e.NewScenario(networkName)
			require.NoError(t, err)
			defer s.Close()

			// Configure the blocks storage to frequently compact TSDB head
			// and ship blocks to the storage.
			flags := mergeFlags(BlocksStorageFlags(), map[string]string{
				"-blocks-storage.tsdb.block-ranges-period":          blockRangePeriod.String(),
				"-blocks-storage.tsdb.ship-interval":                "1s",
				"-blocks-storage.bucket-store.sync-interval":        "1s",
				"-blocks-storage.tsdb.retention-period":             ((blockRangePeriod * 2) - 1).String(),
				"-blocks-storage.bucket-store.index-cache.backend":  testCfg.indexCacheBackend,
				"-store-gateway.sharding-enabled":                   strconv.FormatBool(testCfg.blocksShardingStrategy != ""),
				"-store-gateway.sharding-strategy":                  testCfg.blocksShardingStrategy,
				"-store-gateway.tenant-shard-size":                  fmt.Sprintf("%d", testCfg.tenantShardSize),
				"-querier.ingester-streaming":                       strconv.FormatBool(testCfg.ingesterStreamingEnabled),
				"-querier.query-store-for-labels-enabled":           "true",
				"-blocks-storage.bucket-store.bucket-index.enabled": strconv.FormatBool(testCfg.bucketIndexEnabled),
				"-frontend.query-stats-enabled":                     "true",
				"-querier.parallelise-shardable-queries":            strconv.FormatBool(testCfg.queryShardingEnabled),
			})

			// Start dependencies.
			consul := e2edb.NewConsul()
			minio := e2edb.NewMinio(9000, flags["-blocks-storage.s3.bucket-name"])
			memcached := e2ecache.NewMemcached()
			require.NoError(t, s.StartAndWaitReady(consul, minio, memcached))

			// Add the memcached address to the flags.
			flags["-blocks-storage.bucket-store.index-cache.memcached.addresses"] = "dns+" + memcached.NetworkEndpoint(e2ecache.MemcachedPort)

			// Start Mimir components.
			distributor := e2emimir.NewDistributor("distributor", consul.NetworkHTTPEndpoint(), flags, "")
			ingester := e2emimir.NewIngester("ingester", consul.NetworkHTTPEndpoint(), flags, "")
			storeGateway1 := e2emimir.NewStoreGateway("store-gateway-1", consul.NetworkHTTPEndpoint(), flags, "")
			storeGateway2 := e2emimir.NewStoreGateway("store-gateway-2", consul.NetworkHTTPEndpoint(), flags, "")
			storeGateways := e2emimir.NewCompositeMimirService(storeGateway1, storeGateway2)
			require.NoError(t, s.StartAndWaitReady(distributor, ingester, storeGateway1, storeGateway2))

			// Start the query-frontend but do not check for readiness yet.
			queryFrontend := e2emimir.NewQueryFrontend("query-frontend", flags, "")
			require.NoError(t, s.Start(queryFrontend))

			// Configure the querier to connect to the query-frontend.
			flags["-querier.frontend-address"] = queryFrontend.NetworkGRPCEndpoint()

			// Start the querier with configuring store-gateway addresses if sharding is disabled.
			if testCfg.blocksShardingStrategy == "" {
				flags = mergeFlags(flags, map[string]string{
					"-querier.store-gateway-addresses": strings.Join([]string{storeGateway1.NetworkGRPCEndpoint(), storeGateway2.NetworkGRPCEndpoint()}, ","),
				})
			}
			querier := e2emimir.NewQuerier("querier", consul.NetworkHTTPEndpoint(), flags, "")
			require.NoError(t, s.StartAndWaitReady(querier))
			require.NoError(t, s.WaitReady(queryFrontend))

			// Wait until both the distributor and querier have updated the ring. The querier will also watch
			// the store-gateway ring if blocks sharding is enabled.
			require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))
			if testCfg.blocksShardingStrategy != "" {
				require.NoError(t, querier.WaitSumMetrics(e2e.Equals(float64(512+(512*storeGateways.NumInstances()))), "cortex_ring_tokens_total"))
			} else {
				require.NoError(t, querier.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))
			}

			c, err := e2emimir.NewClient(distributor.HTTPEndpoint(), queryFrontend.HTTPEndpoint(), "", "", "user-1")
			require.NoError(t, err)

			// Push some series to Mimir.
			series1Timestamp := time.Now()
			series2Timestamp := series1Timestamp.Add(blockRangePeriod * 2)
			series1, expectedVector1 := generateSeries("series_1", series1Timestamp, prompb.Label{Name: "series_1", Value: "series_1"})
			series2, expectedVector2 := generateSeries("series_2", series2Timestamp, prompb.Label{Name: "series_2", Value: "series_2"})

			res, err := c.Push(series1)
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode)

			res, err = c.Push(series2)
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode)

			// Wait until the TSDB head is compacted and shipped to the storage.
			// The shipped block contains the 1st series, while the 2ns series in in the head.
			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_shipper_uploads_total"))
			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_memory_series"))
			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(2), "cortex_ingester_memory_series_created_total"))
			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_memory_series_removed_total"))

			// Push another series to further compact another block and delete the first block
			// due to expired retention.
			series3Timestamp := series2Timestamp.Add(blockRangePeriod * 2)
			series3, expectedVector3 := generateSeries("series_3", series3Timestamp, prompb.Label{Name: "series_3", Value: "series_3"})

			res, err = c.Push(series3)
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode)

			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(2), "cortex_ingester_shipper_uploads_total"))
			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_memory_series"))
			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(3), "cortex_ingester_memory_series_created_total"))
			require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(2), "cortex_ingester_memory_series_removed_total"))

			if testCfg.bucketIndexEnabled {
				// Start the compactor to have the bucket index created before querying.
				compactor := e2emimir.NewCompactor("compactor", consul.NetworkHTTPEndpoint(), flags, "")
				require.NoError(t, s.StartAndWaitReady(compactor))
			} else {
				// Wait until the querier has discovered the uploaded blocks.
				require.NoError(t, querier.WaitSumMetrics(e2e.Equals(2), "cortex_blocks_meta_synced"))
			}

			// Wait until the store-gateway has synched the new uploaded blocks. When sharding is enabled
			// we don't known which store-gateway instance will synch the blocks, so we need to wait on
			// metrics extracted from all instances.
			if testCfg.blocksShardingStrategy != "" {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(2), "cortex_bucket_store_blocks_loaded"))
			} else {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(float64(2*storeGateways.NumInstances())), "cortex_bucket_store_blocks_loaded"))
			}

			// Check how many tenants have been discovered and synced by store-gateways.
			require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(float64(1*storeGateways.NumInstances())), "cortex_bucket_stores_tenants_discovered"))
			if testCfg.blocksShardingStrategy == "shuffle-sharding" {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(float64(1)), "cortex_bucket_stores_tenants_synced"))
			} else {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(float64(1*storeGateways.NumInstances())), "cortex_bucket_stores_tenants_synced"))
			}

			// Query back the series (1 only in the storage, 1 only in the ingesters, 1 on both).
			expectedFetchedSeries := 0

			result, err := c.Query("series_1", series1Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector1, result.(model.Vector))
			expectedFetchedSeries++ // Storage only.

			result, err = c.Query("series_2", series2Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector2, result.(model.Vector))
			expectedFetchedSeries += 2 // Ingester + storage.

			result, err = c.Query("series_3", series3Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector3, result.(model.Vector))
			expectedFetchedSeries++ // Ingester only.

			// Check the in-memory index cache metrics (in the store-gateway).
			require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(7), "thanos_store_index_cache_requests_total"))
			require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(0), "thanos_store_index_cache_hits_total")) // no cache hit cause the cache was empty

			if testCfg.indexCacheBackend == tsdb.IndexCacheBackendInMemory {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(2*2), "thanos_store_index_cache_items"))             // 2 series both for postings and series cache
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(2*2), "thanos_store_index_cache_items_added_total")) // 2 series both for postings and series cache
			} else if testCfg.indexCacheBackend == tsdb.IndexCacheBackendMemcached {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(11), "thanos_memcached_operations_total")) // 7 gets + 4 sets
			}

			// Query back again the 1st series from storage. This time it should use the index cache.
			result, err = c.Query("series_1", series1Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector1, result.(model.Vector))
			expectedFetchedSeries++ // Storage only.

			require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(7+2), "thanos_store_index_cache_requests_total"))
			require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(2), "thanos_store_index_cache_hits_total")) // this time has used the index cache

			if testCfg.indexCacheBackend == tsdb.IndexCacheBackendInMemory {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(2*2), "thanos_store_index_cache_items"))             // as before
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(2*2), "thanos_store_index_cache_items_added_total")) // as before
			} else if testCfg.indexCacheBackend == tsdb.IndexCacheBackendMemcached {
				require.NoError(t, storeGateways.WaitSumMetrics(e2e.Equals(11+2), "thanos_memcached_operations_total")) // as before + 2 gets
			}

			// Query range. We expect 1 data point with a value of 3 (number of series).
			result, err = c.QueryRange(`count({__name__=~"series.*"})`, series3Timestamp, series3Timestamp, time.Minute)
			require.NoError(t, err)
			require.Equal(t, model.ValMatrix, result.Type())

			matrix := result.(model.Matrix)
			require.Equal(t, 1, len(matrix))
			require.Equal(t, 1, len(matrix[0].Values))
			assert.Equal(t, model.SampleValue(3), matrix[0].Values[0].Value)
			expectedFetchedSeries += 4 // series_2 is fetched both from ingester and storage, while other series are fetched either from ingester or storage.

			// When query sharding is enabled, we expect the range query above to be sharded.
			if testCfg.queryShardingEnabled {
				require.NoError(t, queryFrontend.WaitSumMetrics(e2e.Equals(1), "cortex_frontend_query_sharding_rewrites_attempted_total"))
				require.NoError(t, queryFrontend.WaitSumMetrics(e2e.Equals(1), "cortex_frontend_query_sharding_rewrites_succeeded_total"))
				require.NoError(t, queryFrontend.WaitSumMetrics(e2e.Equals(16), "cortex_frontend_sharded_queries_total"))
			}

			// Check query stats (supported only when gRPC streaming is enabled).
			if testCfg.ingesterStreamingEnabled {
				require.NoError(t, queryFrontend.WaitSumMetrics(e2e.Equals(float64(expectedFetchedSeries)), "cortex_query_fetched_series_total"))
			}

			// Query metadata.
			testMetadataQueriesWithBlocksStorage(t, c, series1[0], series2[0], series3[0], blockRangePeriod)

			// Ensure no service-specific metrics prefix is used by the wrong service.
			assertServiceMetricsPrefixes(t, Distributor, distributor)
			assertServiceMetricsPrefixes(t, Ingester, ingester)
			assertServiceMetricsPrefixes(t, Querier, querier)
			assertServiceMetricsPrefixes(t, StoreGateway, storeGateway1)
			assertServiceMetricsPrefixes(t, StoreGateway, storeGateway2)
		})
	}
}

func TestQuerierWithBlocksStorageRunningInSingleBinaryMode(t *testing.T) {
	tests := map[string]struct {
		blocksShardingEnabled    bool
		ingesterStreamingEnabled bool
		indexCacheBackend        string
		bucketIndexEnabled       bool
	}{
		"blocks sharding enabled, ingester gRPC streaming disabled, inmemory index cache": {
			blocksShardingEnabled:    true,
			ingesterStreamingEnabled: false,
			indexCacheBackend:        tsdb.IndexCacheBackendInMemory,
		},
		"blocks sharding enabled, ingester gRPC streaming enabled, inmemory index cache": {
			blocksShardingEnabled:    true,
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendInMemory,
		},
		"blocks sharding disabled, ingester gRPC streaming disabled, memcached index cache": {
			blocksShardingEnabled:    false,
			ingesterStreamingEnabled: false,
			// Memcached index cache is required to avoid flaky tests when the blocks sharding is disabled
			// because two different requests may hit two different store-gateways, so if the cache is not
			// shared there's no guarantee we'll have a cache hit.
			indexCacheBackend: tsdb.IndexCacheBackendMemcached,
		},
		"blocks sharding enabled, ingester gRPC streaming enabled, memcached index cache": {
			blocksShardingEnabled:    true,
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendMemcached,
		},
		"blocks sharding enabled, ingester gRPC streaming enabled, memcached index cache, bucket index enabled": {
			blocksShardingEnabled:    true,
			ingesterStreamingEnabled: true,
			indexCacheBackend:        tsdb.IndexCacheBackendMemcached,
			bucketIndexEnabled:       true,
		},
	}

	for testName, testCfg := range tests {
		t.Run(testName, func(t *testing.T) {
			const blockRangePeriod = 5 * time.Second

			s, err := e2e.NewScenario(networkName)
			require.NoError(t, err)
			defer s.Close()

			// Start dependencies.
			consul := e2edb.NewConsul()
			minio := e2edb.NewMinio(9000, bucketName)
			memcached := e2ecache.NewMemcached()
			require.NoError(t, s.StartAndWaitReady(consul, minio, memcached))

			// Setting the replication factor equal to the number of Mimir replicas
			// make sure each replica creates the same blocks, so the total number of
			// blocks is stable and easy to assert on.
			const seriesReplicationFactor = 2

			// Configure the blocks storage to frequently compact TSDB head
			// and ship blocks to the storage.
			flags := mergeFlags(BlocksStorageFlags(), map[string]string{
				"-blocks-storage.tsdb.block-ranges-period":                     blockRangePeriod.String(),
				"-blocks-storage.tsdb.ship-interval":                           "1s",
				"-blocks-storage.bucket-store.sync-interval":                   "1s",
				"-blocks-storage.tsdb.retention-period":                        ((blockRangePeriod * 2) - 1).String(),
				"-blocks-storage.bucket-store.index-cache.backend":             testCfg.indexCacheBackend,
				"-blocks-storage.bucket-store.index-cache.memcached.addresses": "dns+" + memcached.NetworkEndpoint(e2ecache.MemcachedPort),
				"-blocks-storage.bucket-store.bucket-index.enabled":            strconv.FormatBool(testCfg.bucketIndexEnabled),
				"-querier.ingester-streaming":                                  strconv.FormatBool(testCfg.ingesterStreamingEnabled),
				"-querier.query-store-for-labels-enabled":                      "true",
				// Ingester.
				"-ring.store":      "consul",
				"-consul.hostname": consul.NetworkHTTPEndpoint(),
				// Distributor.
				"-distributor.replication-factor": strconv.FormatInt(seriesReplicationFactor, 10),
				// Store-gateway.
				"-store-gateway.sharding-enabled":                 strconv.FormatBool(testCfg.blocksShardingEnabled),
				"-store-gateway.sharding-ring.store":              "consul",
				"-store-gateway.sharding-ring.consul.hostname":    consul.NetworkHTTPEndpoint(),
				"-store-gateway.sharding-ring.replication-factor": "1",
			})

			// Start Mimir replicas.
			mimir1 := e2emimir.NewSingleBinary("mimir-1", flags, "")
			mimir2 := e2emimir.NewSingleBinary("mimir-2", flags, "")
			cluster := e2emimir.NewCompositeMimirService(mimir1, mimir2)
			require.NoError(t, s.StartAndWaitReady(mimir1, mimir2))

			// Wait until Mimir replicas have updated the ring state.
			for _, replica := range cluster.Instances() {
				numTokensPerInstance := 512 // Ingesters ring.
				if testCfg.blocksShardingEnabled {
					numTokensPerInstance += 512 * 2 // Store-gateway ring (read both by the querier and store-gateway).
				}

				require.NoError(t, replica.WaitSumMetrics(e2e.Equals(float64(numTokensPerInstance*cluster.NumInstances())), "cortex_ring_tokens_total"))
			}

			c, err := e2emimir.NewClient(mimir1.HTTPEndpoint(), mimir2.HTTPEndpoint(), "", "", "user-1")
			require.NoError(t, err)

			// Push some series to Mimir.
			series1Timestamp := time.Now()
			series2Timestamp := series1Timestamp.Add(blockRangePeriod * 2)
			series1, expectedVector1 := generateSeries("series_1", series1Timestamp, prompb.Label{Name: "series_1", Value: "series_1"})
			series2, expectedVector2 := generateSeries("series_2", series2Timestamp, prompb.Label{Name: "series_2", Value: "series_2"})

			res, err := c.Push(series1)
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode)

			res, err = c.Push(series2)
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode)

			// Wait until the TSDB head is compacted and shipped to the storage.
			// The shipped block contains the 1st series, while the 2ns series in in the head.
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(1*cluster.NumInstances())), "cortex_ingester_shipper_uploads_total"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(1*cluster.NumInstances())), "cortex_ingester_memory_series"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*cluster.NumInstances())), "cortex_ingester_memory_series_created_total"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(1*cluster.NumInstances())), "cortex_ingester_memory_series_removed_total"))

			// Push another series to further compact another block and delete the first block
			// due to expired retention.
			series3Timestamp := series2Timestamp.Add(blockRangePeriod * 2)
			series3, expectedVector3 := generateSeries("series_3", series3Timestamp, prompb.Label{Name: "series_3", Value: "series_3"})

			res, err = c.Push(series3)
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode)

			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*cluster.NumInstances())), "cortex_ingester_shipper_uploads_total"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(1*cluster.NumInstances())), "cortex_ingester_memory_series"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(3*cluster.NumInstances())), "cortex_ingester_memory_series_created_total"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*cluster.NumInstances())), "cortex_ingester_memory_series_removed_total"))

			if testCfg.bucketIndexEnabled {
				// Start the compactor to have the bucket index created before querying. We need to run the compactor
				// as a separate service because it's currently not part of the single binary.
				compactor := e2emimir.NewCompactor("compactor", consul.NetworkHTTPEndpoint(), flags, "")
				require.NoError(t, s.StartAndWaitReady(compactor))
			} else {
				// Wait until the querier has discovered the uploaded blocks (discovered both by the querier and store-gateway).
				require.NoError(t, cluster.WaitSumMetricsWithOptions(e2e.Equals(float64(2*cluster.NumInstances()*2)), []string{"cortex_blocks_meta_synced"}, e2e.WithLabelMatchers(
					labels.MustNewMatcher(labels.MatchEqual, "component", "querier"))))
			}

			// Wait until the store-gateway has synched the new uploaded blocks. The number of blocks loaded
			// may be greater than expected if the compactor is running (there may have been compacted).
			const shippedBlocks = 2
			if testCfg.blocksShardingEnabled {
				require.NoError(t, cluster.WaitSumMetrics(e2e.GreaterOrEqual(float64(shippedBlocks*seriesReplicationFactor)), "cortex_bucket_store_blocks_loaded"))
			} else {
				require.NoError(t, cluster.WaitSumMetrics(e2e.GreaterOrEqual(float64(shippedBlocks*seriesReplicationFactor*cluster.NumInstances())), "cortex_bucket_store_blocks_loaded"))
			}

			// Query back the series (1 only in the storage, 1 only in the ingesters, 1 on both).
			result, err := c.Query("series_1", series1Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector1, result.(model.Vector))

			result, err = c.Query("series_2", series2Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector2, result.(model.Vector))

			result, err = c.Query("series_3", series3Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector3, result.(model.Vector))

			// Check the in-memory index cache metrics (in the store-gateway).
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(7*seriesReplicationFactor)), "thanos_store_index_cache_requests_total"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(0), "thanos_store_index_cache_hits_total")) // no cache hit cause the cache was empty

			if testCfg.indexCacheBackend == tsdb.IndexCacheBackendInMemory {
				require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*2*seriesReplicationFactor)), "thanos_store_index_cache_items"))             // 2 series both for postings and series cache
				require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*2*seriesReplicationFactor)), "thanos_store_index_cache_items_added_total")) // 2 series both for postings and series cache
			} else if testCfg.indexCacheBackend == tsdb.IndexCacheBackendMemcached {
				require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(11*seriesReplicationFactor)), "thanos_memcached_operations_total")) // 7 gets + 4 sets
			}

			// Query back again the 1st series from storage. This time it should use the index cache.
			result, err = c.Query("series_1", series1Timestamp)
			require.NoError(t, err)
			require.Equal(t, model.ValVector, result.Type())
			assert.Equal(t, expectedVector1, result.(model.Vector))

			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64((7+2)*seriesReplicationFactor)), "thanos_store_index_cache_requests_total"))
			require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*seriesReplicationFactor)), "thanos_store_index_cache_hits_total")) // this time has used the index cache

			if testCfg.indexCacheBackend == tsdb.IndexCacheBackendInMemory {
				require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*2*seriesReplicationFactor)), "thanos_store_index_cache_items"))             // as before
				require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64(2*2*seriesReplicationFactor)), "thanos_store_index_cache_items_added_total")) // as before
			} else if testCfg.indexCacheBackend == tsdb.IndexCacheBackendMemcached {
				require.NoError(t, cluster.WaitSumMetrics(e2e.Equals(float64((11+2)*seriesReplicationFactor)), "thanos_memcached_operations_total")) // as before + 2 gets
			}

			// Query metadata.
			testMetadataQueriesWithBlocksStorage(t, c, series1[0], series2[0], series3[0], blockRangePeriod)
		})
	}
}

func testMetadataQueriesWithBlocksStorage(
	t *testing.T,
	c *e2emimir.Client,
	lastSeriesInStorage prompb.TimeSeries,
	lastSeriesInIngesterBlocks prompb.TimeSeries,
	firstSeriesInIngesterHead prompb.TimeSeries,
	blockRangePeriod time.Duration,
) {
	var (
		lastSeriesInIngesterBlocksName = getMetricName(lastSeriesInIngesterBlocks.Labels)
		firstSeriesInIngesterHeadName  = getMetricName(firstSeriesInIngesterHead.Labels)
		lastSeriesInStorageName        = getMetricName(lastSeriesInStorage.Labels)

		lastSeriesInStorageTs        = util.TimeFromMillis(lastSeriesInStorage.Samples[0].Timestamp)
		lastSeriesInIngesterBlocksTs = util.TimeFromMillis(lastSeriesInIngesterBlocks.Samples[0].Timestamp)
		firstSeriesInIngesterHeadTs  = util.TimeFromMillis(firstSeriesInIngesterHead.Samples[0].Timestamp)
	)

	type seriesTest struct {
		lookup string
		ok     bool
		resp   []prompb.Label
	}
	type labelValuesTest struct {
		label   string
		matches []string
		resp    []string
	}

	testCases := map[string]struct {
		from time.Time
		to   time.Time

		seriesTests []seriesTest

		labelValuesTests []labelValuesTest

		labelNames []string
	}{
		"query metadata entirely inside the head range": {
			from: firstSeriesInIngesterHeadTs,
			to:   firstSeriesInIngesterHeadTs.Add(blockRangePeriod),
			seriesTests: []seriesTest{
				{
					lookup: firstSeriesInIngesterHeadName,
					ok:     true,
					resp:   firstSeriesInIngesterHead.Labels,
				},
				{
					lookup: lastSeriesInIngesterBlocksName,
					ok:     false,
				},
				{
					lookup: lastSeriesInStorageName,
					ok:     false,
				},
			},
			labelValuesTests: []labelValuesTest{
				{
					label: labels.MetricName,
					resp:  []string{firstSeriesInIngesterHeadName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{firstSeriesInIngesterHeadName},
					matches: []string{firstSeriesInIngesterHeadName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{},
					matches: []string{lastSeriesInStorageName},
				},
			},
			labelNames: []string{labels.MetricName, firstSeriesInIngesterHeadName},
		},
		"query metadata entirely inside the ingester range but outside the head range": {
			from: lastSeriesInIngesterBlocksTs,
			to:   lastSeriesInIngesterBlocksTs.Add(blockRangePeriod / 2),
			seriesTests: []seriesTest{
				{
					lookup: firstSeriesInIngesterHeadName,
					ok:     false,
				},
				{
					lookup: lastSeriesInIngesterBlocksName,
					ok:     true,
					resp:   lastSeriesInIngesterBlocks.Labels,
				},
				{
					lookup: lastSeriesInStorageName,
					ok:     false,
				},
			},
			labelValuesTests: []labelValuesTest{
				{
					label: labels.MetricName,
					resp:  []string{lastSeriesInIngesterBlocksName},
				},

				{
					label:   labels.MetricName,
					resp:    []string{lastSeriesInIngesterBlocksName},
					matches: []string{lastSeriesInIngesterBlocksName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{},
					matches: []string{firstSeriesInIngesterHeadName},
				},
			},
			labelNames: []string{labels.MetricName, lastSeriesInIngesterBlocksName},
		},
		"query metadata partially inside the ingester range": {
			from: lastSeriesInStorageTs.Add(-blockRangePeriod),
			to:   firstSeriesInIngesterHeadTs.Add(blockRangePeriod),
			seriesTests: []seriesTest{
				{
					lookup: firstSeriesInIngesterHeadName,
					ok:     true,
					resp:   firstSeriesInIngesterHead.Labels,
				},
				{
					lookup: lastSeriesInIngesterBlocksName,
					ok:     true,
					resp:   lastSeriesInIngesterBlocks.Labels,
				},
				{
					lookup: lastSeriesInStorageName,
					ok:     true,
					resp:   lastSeriesInStorage.Labels,
				},
			},
			labelValuesTests: []labelValuesTest{
				{
					label: labels.MetricName,
					resp:  []string{lastSeriesInStorageName, lastSeriesInIngesterBlocksName, firstSeriesInIngesterHeadName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{lastSeriesInStorageName},
					matches: []string{lastSeriesInStorageName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{lastSeriesInIngesterBlocksName},
					matches: []string{lastSeriesInIngesterBlocksName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{lastSeriesInStorageName, lastSeriesInIngesterBlocksName},
					matches: []string{lastSeriesInStorageName, lastSeriesInIngesterBlocksName},
				},
			},
			labelNames: []string{labels.MetricName, lastSeriesInStorageName, lastSeriesInIngesterBlocksName, firstSeriesInIngesterHeadName},
		},
		"query metadata entirely outside the ingester range should return the head data as well": {
			from: lastSeriesInStorageTs.Add(-2 * blockRangePeriod),
			to:   lastSeriesInStorageTs,
			seriesTests: []seriesTest{
				{
					lookup: firstSeriesInIngesterHeadName,
					ok:     true,
					resp:   firstSeriesInIngesterHead.Labels,
				},
				{
					lookup: lastSeriesInIngesterBlocksName,
					ok:     false,
				},
				{
					lookup: lastSeriesInStorageName,
					ok:     true,
					resp:   lastSeriesInStorage.Labels,
				},
			},
			labelValuesTests: []labelValuesTest{
				{
					label: labels.MetricName,
					resp:  []string{lastSeriesInStorageName, firstSeriesInIngesterHeadName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{lastSeriesInStorageName},
					matches: []string{lastSeriesInStorageName},
				},
				{
					label:   labels.MetricName,
					resp:    []string{firstSeriesInIngesterHeadName},
					matches: []string{firstSeriesInIngesterHeadName},
				},
			},
			labelNames: []string{labels.MetricName, lastSeriesInStorageName, firstSeriesInIngesterHeadName},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			for _, st := range tc.seriesTests {
				seriesRes, err := c.Series([]string{st.lookup}, tc.from, tc.to)
				require.NoError(t, err)
				if st.ok {
					require.Equal(t, 1, len(seriesRes))
					require.Equal(t, model.LabelSet(prompbLabelsToModelMetric(st.resp)), seriesRes[0])
				} else {
					require.Equal(t, 0, len(seriesRes))
				}
			}

			for _, lvt := range tc.labelValuesTests {
				labelsRes, err := c.LabelValues(lvt.label, tc.from, tc.to, lvt.matches)
				require.NoError(t, err)
				exp := model.LabelValues{}
				for _, val := range lvt.resp {
					exp = append(exp, model.LabelValue(val))
				}
				require.Equal(t, exp, labelsRes)
			}

			labelNames, err := c.LabelNames(tc.from, tc.to)
			require.NoError(t, err)
			require.Equal(t, tc.labelNames, labelNames)
		})
	}
}

func TestQuerierWithBlocksStorageOnMissingBlocksFromStorage(t *testing.T) {
	const blockRangePeriod = 5 * time.Second

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Configure the blocks storage to frequently compact TSDB head
	// and ship blocks to the storage.
	flags := mergeFlags(BlocksStorageFlags(), map[string]string{
		"-blocks-storage.tsdb.block-ranges-period": blockRangePeriod.String(),
		"-blocks-storage.tsdb.ship-interval":       "1s",
		"-blocks-storage.tsdb.retention-period":    ((blockRangePeriod * 2) - 1).String(),
	})

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, flags["-blocks-storage.s3.bucket-name"])
	require.NoError(t, s.StartAndWaitReady(consul, minio))

	// Start Mimir components for the write path.
	distributor := e2emimir.NewDistributor("distributor", consul.NetworkHTTPEndpoint(), flags, "")
	ingester := e2emimir.NewIngester("ingester", consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester))

	// Wait until the distributor has updated the ring.
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))

	// Push some series to Mimir.
	c, err := e2emimir.NewClient(distributor.HTTPEndpoint(), "", "", "", "user-1")
	require.NoError(t, err)

	series1Timestamp := time.Now()
	series2Timestamp := series1Timestamp.Add(blockRangePeriod * 2)
	series1, expectedVector1 := generateSeries("series_1", series1Timestamp)
	series2, _ := generateSeries("series_2", series2Timestamp)

	res, err := c.Push(series1)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	res, err = c.Push(series2)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	// Wait until the TSDB head is compacted and shipped to the storage.
	// The shipped block contains the 1st series, while the 2ns series in in the head.
	require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_shipper_uploads_total"))
	require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(2), "cortex_ingester_memory_series_created_total"))
	require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_memory_series_removed_total"))
	require.NoError(t, ingester.WaitSumMetrics(e2e.Equals(1), "cortex_ingester_memory_series"))

	// Start the querier and store-gateway, and configure them to not frequently sync blocks.
	storeGateway := e2emimir.NewStoreGateway("store-gateway", consul.NetworkHTTPEndpoint(), mergeFlags(flags, map[string]string{
		"-blocks-storage.bucket-store.sync-interval": "1m",
	}), "")
	querier := e2emimir.NewQuerier("querier", consul.NetworkHTTPEndpoint(), mergeFlags(flags, map[string]string{
		"-blocks-storage.bucket-store.sync-interval": "1m",
	}), "")
	require.NoError(t, s.StartAndWaitReady(querier, storeGateway))

	// Wait until the querier and store-gateway have updated the ring.
	require.NoError(t, querier.WaitSumMetrics(e2e.Equals(512*2), "cortex_ring_tokens_total"))
	require.NoError(t, storeGateway.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))

	// Query back the series.
	c, err = e2emimir.NewClient("", querier.HTTPEndpoint(), "", "", "user-1")
	require.NoError(t, err)

	result, err := c.Query("series_1", series1Timestamp)
	require.NoError(t, err)
	require.Equal(t, model.ValVector, result.Type())
	assert.Equal(t, expectedVector1, result.(model.Vector))

	// Delete all blocks from the storage.
	storage, err := e2emimir.NewS3ClientForMinio(minio, flags["-blocks-storage.s3.bucket-name"])
	require.NoError(t, err)
	require.NoError(t, storage.DeleteBlocks("user-1"))

	// Query back again the series. Now we do expect a 500 error because the blocks are
	// missing from the storage.
	_, err = c.Query("series_1", series1Timestamp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestQueryLimitsWithBlocksStorageRunningInMicroServices(t *testing.T) {
	const blockRangePeriod = 5 * time.Second

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	// Configure the blocks storage to frequently compact TSDB head
	// and ship blocks to the storage.
	flags := mergeFlags(BlocksStorageFlags(), map[string]string{
		"-blocks-storage.tsdb.block-ranges-period":   blockRangePeriod.String(),
		"-blocks-storage.tsdb.ship-interval":         "1s",
		"-blocks-storage.bucket-store.sync-interval": "1s",
		"-blocks-storage.tsdb.retention-period":      ((blockRangePeriod * 2) - 1).String(),
		"-querier.ingester-streaming":                "true",
		"-querier.query-store-for-labels-enabled":    "true",
		"-querier.max-fetched-series-per-query":      "3",
	})

	// Start dependencies.
	consul := e2edb.NewConsul()
	minio := e2edb.NewMinio(9000, flags["-blocks-storage.s3.bucket-name"])
	memcached := e2ecache.NewMemcached()
	require.NoError(t, s.StartAndWaitReady(consul, minio, memcached))

	// Add the memcached address to the flags.
	flags["-blocks-storage.bucket-store.index-cache.memcached.addresses"] = "dns+" + memcached.NetworkEndpoint(e2ecache.MemcachedPort)

	// Start Mimir components.
	distributor := e2emimir.NewDistributor("distributor", consul.NetworkHTTPEndpoint(), flags, "")
	ingester := e2emimir.NewIngester("ingester", consul.NetworkHTTPEndpoint(), flags, "")
	storeGateway := e2emimir.NewStoreGateway("store-gateway", consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester, storeGateway))

	// Start the querier with configuring store-gateway addresses if sharding is disabled.
	flags = mergeFlags(flags, map[string]string{
		"-querier.store-gateway-addresses": strings.Join([]string{storeGateway.NetworkGRPCEndpoint()}, ","),
	})

	querier := e2emimir.NewQuerier("querier", consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(querier))

	c, err := e2emimir.NewClient(distributor.HTTPEndpoint(), querier.HTTPEndpoint(), "", "", "user-1")
	require.NoError(t, err)

	// Push some series to Mimir.
	series1Timestamp := time.Now()
	series2Timestamp := series1Timestamp.Add(blockRangePeriod * 2)
	series3Timestamp := series1Timestamp.Add(blockRangePeriod * 2)
	series4Timestamp := series1Timestamp.Add(blockRangePeriod * 3)

	series1, _ := generateSeries("series_1", series1Timestamp, prompb.Label{Name: "series_1", Value: "series_1"})
	series2, _ := generateSeries("series_2", series2Timestamp, prompb.Label{Name: "series_2", Value: "series_2"})
	series3, _ := generateSeries("series_3", series3Timestamp, prompb.Label{Name: "series_3", Value: "series_3"})
	series4, _ := generateSeries("series_4", series4Timestamp, prompb.Label{Name: "series_4", Value: "series_4"})

	res, err := c.Push(series1)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	res, err = c.Push(series2)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	result, err := c.QueryRange("{__name__=~\"series_.+\"}", series1Timestamp, series2Timestamp.Add(1*time.Hour), blockRangePeriod)
	require.NoError(t, err)
	require.Equal(t, model.ValMatrix, result.Type())

	res, err = c.Push(series3)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	res, err = c.Push(series4)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	_, err = c.QueryRange("{__name__=~\"series_.+\"}", series1Timestamp, series4Timestamp.Add(1*time.Hour), blockRangePeriod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max number of series limit")
}

func TestHashCollisionHandling(t *testing.T) {
	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	require.NoError(t, writeFileToSharedDir(s, mimirSchemaConfigFile, []byte(mimirSchemaConfigYaml)))
	flags := ChunksStorageFlags()

	// Start dependencies.
	dynamo := e2edb.NewDynamoDB()

	consul := e2edb.NewConsul()
	require.NoError(t, s.StartAndWaitReady(consul, dynamo))

	tableManager := e2emimir.NewTableManager("table-manager", ChunksStorageFlags(), "")
	require.NoError(t, s.StartAndWaitReady(tableManager))

	// Wait until the first table-manager sync has completed, so that we're
	// sure the tables have been created.
	require.NoError(t, tableManager.WaitSumMetrics(e2e.Greater(0), "cortex_table_manager_sync_success_timestamp_seconds"))

	// Start Mimir components for the write path.
	distributor := e2emimir.NewDistributor("distributor", consul.NetworkHTTPEndpoint(), flags, "")
	ingester := e2emimir.NewIngester("ingester", consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(distributor, ingester))

	// Wait until the distributor has updated the ring.
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))

	// Push a series for each user to Mimir.
	now := time.Now()

	c, err := e2emimir.NewClient(distributor.HTTPEndpoint(), "", "", "", "user-0")
	require.NoError(t, err)

	var series []prompb.TimeSeries
	var expectedVector model.Vector
	// Generate two series which collide on fingerprints and fast fingerprints.
	tsMillis := e2e.TimeToMilliseconds(now)
	metric1 := []prompb.Label{
		{Name: "A", Value: "K6sjsNNczPl"},
		{Name: labels.MetricName, Value: "fingerprint_collision"},
	}
	metric2 := []prompb.Label{
		{Name: "A", Value: "cswpLMIZpwt"},
		{Name: labels.MetricName, Value: "fingerprint_collision"},
	}

	series = append(series, prompb.TimeSeries{
		Labels: metric1,
		Samples: []prompb.Sample{
			{Value: float64(0), Timestamp: tsMillis},
		},
	})
	expectedVector = append(expectedVector, &model.Sample{
		Metric:    prompbLabelsToModelMetric(metric1),
		Value:     model.SampleValue(float64(0)),
		Timestamp: model.Time(tsMillis),
	})
	series = append(series, prompb.TimeSeries{
		Labels: metric2,
		Samples: []prompb.Sample{
			{Value: float64(1), Timestamp: tsMillis},
		},
	})
	expectedVector = append(expectedVector, &model.Sample{
		Metric:    prompbLabelsToModelMetric(metric2),
		Value:     model.SampleValue(float64(1)),
		Timestamp: model.Time(tsMillis),
	})

	res, err := c.Push(series)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	querier := e2emimir.NewQuerier("querier", consul.NetworkHTTPEndpoint(), flags, "")
	require.NoError(t, s.StartAndWaitReady(querier))

	// Wait until the querier has updated the ring.
	require.NoError(t, querier.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))

	// Query the series.
	c, err = e2emimir.NewClient("", querier.HTTPEndpoint(), "", "", "user-0")
	require.NoError(t, err)

	result, err := c.Query("fingerprint_collision", now)
	require.NoError(t, err)
	require.Equal(t, model.ValVector, result.Type())
	require.Equal(t, expectedVector, result.(model.Vector))
}

func getMetricName(lbls []prompb.Label) string {
	for _, lbl := range lbls {
		if lbl.Name == labels.MetricName {
			return lbl.Value
		}
	}

	panic(fmt.Sprintf("series %v has no metric name", lbls))
}

func prompbLabelsToModelMetric(pbLabels []prompb.Label) model.Metric {
	metric := model.Metric{}

	for _, l := range pbLabels {
		metric[model.LabelName(l.Name)] = model.LabelValue(l.Value)
	}

	return metric
}
