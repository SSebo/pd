// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/pd/server/cache"
	"github.com/pingcap/pd/server/core"
	"github.com/pingcap/pd/server/schedule"
	log "github.com/sirupsen/logrus"
)

func init() {
	schedule.RegisterScheduler("balance-region", func(limiter *schedule.Limiter, args []string) (schedule.Scheduler, error) {
		return newBalanceRegionScheduler(limiter), nil
	})
}

const storeCacheInterval = 30 * time.Second

type balanceRegionScheduler struct {
	*baseScheduler
	cache    *cache.TTLUint64
	limit    uint64
	selector schedule.Selector
}

// newBalanceRegionScheduler creates a scheduler that tends to keep regions on
// each store balanced.
func newBalanceRegionScheduler(limiter *schedule.Limiter) schedule.Scheduler {
	ttlCache := cache.NewIDTTL(storeCacheInterval, 4*storeCacheInterval)
	filters := []schedule.Filter{
		schedule.NewCacheFilter(ttlCache),
		schedule.NewStateFilter(),
		schedule.NewHealthFilter(),
		schedule.NewSnapshotCountFilter(),
		schedule.NewStorageThresholdFilter(),
		schedule.NewPendingPeerCountFilter(),
	}
	base := newBaseScheduler(limiter)
	return &balanceRegionScheduler{
		baseScheduler: base,
		cache:         ttlCache,
		limit:         1,
		selector:      schedule.NewBalanceSelector(core.RegionKind, filters),
	}
}

func (s *balanceRegionScheduler) GetName() string {
	return "balance-region-scheduler"
}

func (s *balanceRegionScheduler) GetType() string {
	return "balance-region"
}

func (s *balanceRegionScheduler) IsScheduleAllowed(cluster schedule.Cluster) bool {
	limit := minUint64(s.limit, cluster.GetRegionScheduleLimit())
	return s.limiter.OperatorCount(schedule.OpRegion) < limit
}

func (s *balanceRegionScheduler) Schedule(cluster schedule.Cluster, opInfluence schedule.OpInfluence) *schedule.Operator {
	schedulerCounter.WithLabelValues(s.GetName(), "schedule").Inc()
	// Select a peer from the store with most regions.
	region, oldPeer := scheduleRemovePeer(cluster, s.GetName(), s.selector)
	if region == nil {
		return nil
	}

	// We don't schedule region with abnormal number of replicas.
	if len(region.GetPeers()) != cluster.GetMaxReplicas() {
		schedulerCounter.WithLabelValues(s.GetName(), "abnormal_replica").Inc()
		return nil
	}

	// Skip hot regions.
	if cluster.IsRegionHot(region.GetId()) {
		schedulerCounter.WithLabelValues(s.GetName(), "region_hot").Inc()
		return nil
	}

	op := s.transferPeer(cluster, region, oldPeer, opInfluence)
	if op == nil {
		// We can't transfer peer from this store now, so we add it to the cache
		// and skip it for a while.
		s.cache.Put(oldPeer.GetStoreId())
		return nil
	}
	schedulerCounter.WithLabelValues(s.GetName(), "new_operator").Inc()
	return op
}

func (s *balanceRegionScheduler) transferPeer(cluster schedule.Cluster, region *core.RegionInfo, oldPeer *metapb.Peer, opInfluence schedule.OpInfluence) *schedule.Operator {
	// scoreGuard guarantees that the distinct score will not decrease.
	stores := cluster.GetRegionStores(region)
	source := cluster.GetStore(oldPeer.GetStoreId())
	scoreGuard := schedule.NewDistinctScoreFilter(cluster.GetLocationLabels(), stores, source)

	checker := schedule.NewReplicaChecker(cluster, nil)
	newPeer := checker.SelectBestReplacedPeerToAddReplica(region, oldPeer, scoreGuard)
	if newPeer == nil {
		schedulerCounter.WithLabelValues(s.GetName(), "no_peer").Inc()
		return nil
	}

	target := cluster.GetStore(newPeer.GetStoreId())
	log.Debugf("[region %d] source store id is %v, target store id is %v", region.GetId(), source.GetId(), target.GetId())

	sourceSize := source.RegionSize + int64(opInfluence.GetStoreInfluence(source.GetId()).RegionSize)
	targetSize := target.RegionSize + int64(opInfluence.GetStoreInfluence(target.GetId()).RegionSize)
	regionSize := float64(region.ApproximateSize) * cluster.GetTolerantSizeRatio()
	if !shouldBalance(sourceSize, source.RegionWeight, targetSize, target.RegionWeight, regionSize) {
		log.Debugf("[%s] skip balance region%d, source size: %v, source weight: %v, target size: %v, target weight: %v, region size: %v", s.GetName(), region.GetId(), sourceSize, source.RegionWeight, targetSize, target.RegionWeight, region.ApproximateSize)
		schedulerCounter.WithLabelValues(s.GetName(), "skip").Inc()
		return nil
	}
	s.limit = adjustBalanceLimit(cluster, core.RegionKind)

	return schedule.CreateMovePeerOperator("balance-region", cluster, region, schedule.OpBalance, oldPeer.GetStoreId(), newPeer.GetStoreId(), newPeer.GetId())
}