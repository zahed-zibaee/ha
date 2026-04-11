package api

import (
	"math/rand"
	"sort"
	"strings"
	"sync"

	"ha/config"
)

type lbSelector struct {
	strategy string
	mu       sync.Mutex
	counter  map[string]int
	targets  map[string]map[string]config.Target
	ordered  map[string][]string
	perGroup map[string]string
}

func newSelector(strategy string, targets map[string]map[string]config.Target, perGroup map[string]string) *lbSelector {
	s := normalizeStrategy(strategy)
	if s == "" {
		s = "random"
	}
	ordered := make(map[string][]string, len(targets))
	for group, tmap := range targets {
		names := make([]string, 0, len(tmap))
		for name := range tmap {
			names = append(names, name)
		}
		sort.Strings(names)
		ordered[group] = names
	}
	normalized := make(map[string]string, len(perGroup))
	for group, st := range perGroup {
		if ns := normalizeStrategy(st); ns != "" {
			normalized[group] = ns
		}
	}
	return &lbSelector{strategy: s, counter: make(map[string]int), targets: targets, ordered: ordered, perGroup: normalized}
}

func normalizeStrategy(strategy string) string {
	s := strings.ToLower(strategy)
	switch s {
	case "round-robin", "roundrobin", "rr":
		return "round-robin"
	case "random":
		return "random"
	case "weighted", "weighted-latency", "latency":
		return "weighted"
	case "weighted-rr", "weighted-round-robin", "weighted_round_robin", "weightedrr":
		return "weighted-rr"
	default:
		return ""
	}
}

func (s *lbSelector) strategyForGroup(group string) string {
	if st, ok := s.perGroup[group]; ok && st != "" {
		return st
	}
	return s.strategy
}

func (s *lbSelector) isWeighted(group string) bool {
	st := s.strategyForGroup(group)
	return st == "weighted" || st == "weighted-rr"
}

func (s *lbSelector) pickHydrated(group string, reachable, all []probeResult) probeResult {
	if len(reachable) == 0 {
		return all[0]
	}
	switch s.strategyForGroup(group) {
	case "random":
		return reachable[rand.Intn(len(reachable))]
	case "weighted":
		return pickWeightedLatency(reachable)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.counter[group] % len(reachable)
	s.counter[group]++
	return reachable[idx]
}

func (s *lbSelector) pickFromPool(group string, pool []probeResult) probeResult {
	if len(pool) == 0 {
		return probeResult{}
	}
	switch s.strategyForGroup(group) {
	case "weighted-rr":
		s.mu.Lock()
		idx := s.counter[group] % len(pool)
		s.counter[group]++
		s.mu.Unlock()
		return pool[idx]
	default:
		return pool[rand.Intn(len(pool))]
	}
}

func pickWeightedLatency(reachable []probeResult) probeResult {
	if len(reachable) == 1 {
		return reachable[0]
	}
	total := 0.0
	weights := make([]float64, len(reachable))
	for i, r := range reachable {
		w := 1.0
		if r.LatencyMs > 0 {
			w = 1.0 / float64(r.LatencyMs)
		}
		weights[i] = w
		total += w
	}
	if total <= 0 {
		return reachable[rand.Intn(len(reachable))]
	}
	roll := rand.Float64() * total
	acc := 0.0
	for i, w := range weights {
		acc += w
		if roll <= acc {
			return reachable[i]
		}
	}
	return reachable[len(reachable)-1]
}

func latencyWeight(latencyMs int64) int {
	switch {
	case latencyMs >= 1000:
		return 1
	case latencyMs >= 900:
		return 2
	case latencyMs >= 800:
		return 3
	case latencyMs >= 700:
		return 4
	case latencyMs >= 600:
		return 5
	case latencyMs >= 500:
		return 6
	case latencyMs >= 400:
		return 7
	case latencyMs >= 300:
		return 8
	case latencyMs >= 200:
		return 9
	default:
		return 10
	}
}

func buildWeightedPool(up []probeResult) []probeResult {
	if len(up) == 0 {
		return nil
	}
	totalWeight := 0
	weights := make([]int, len(up))
	for i, pr := range up {
		w := latencyWeight(pr.LatencyMs)
		weights[i] = w
		totalWeight += w
	}
	if totalWeight == 0 {
		return nil
	}
	targetTotal := totalWeight
	if targetTotal > weightedPoolMax {
		targetTotal = weightedPoolMax
	}
	pool := make([]probeResult, 0, targetTotal)
	assigned := 0
	for i, pr := range up {
		w := weights[i]
		portion := (w * targetTotal) / totalWeight
		if portion == 0 {
			portion = 1
		}
		if assigned+portion > targetTotal {
			portion = targetTotal - assigned
		}
		for j := 0; j < portion; j++ {
			pool = append(pool, pr)
		}
		assigned += portion
		if assigned >= targetTotal {
			break
		}
	}
	for assigned < targetTotal {
		pool = append(pool, up[assigned%len(up)])
		assigned++
	}
	return pool
}

func pickFromCacheGroup(group string, selector *lbSelector, cg *cachedGroup) probeResult {
	if cg == nil {
		return probeResult{}
	}
	if selector.isWeighted(group) {
		pool := cg.weightedUp
		if len(pool) == 0 {
			pool = cg.hydratedUp
			if len(pool) == 0 {
				pool = cg.hydratedAll
			}
		}
		return selector.pickFromPool(group, pool)
	}
	return selector.pickHydrated(group, cg.hydratedUp, cg.hydratedAll)
}
