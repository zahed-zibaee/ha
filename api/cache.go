package api

import (
	"log/slog"
	"sync"
	"time"
)

const (
	lbCacheMaxAge   = 5 * time.Second
	redisLBTimeout  = 200 * time.Millisecond
	redisBackoffTTL = 1 * time.Second
	weightedPoolMax = 1000
)

var redisSem = make(chan struct{}, 256)

type cacheState struct {
	mu       sync.Mutex
	last     map[string][]probeResult
	lb       map[string]cachedGroup
	backoff  map[string]time.Time
	warnMu   sync.Mutex
	lastWarn map[string]time.Time
}

func newCacheState() *cacheState {
	return &cacheState{
		last:     make(map[string][]probeResult),
		lb:       make(map[string]cachedGroup),
		backoff:  make(map[string]time.Time),
		lastWarn: make(map[string]time.Time),
	}
}

type cachedGroup struct {
	data        groupData
	hydratedAll []probeResult
	hydratedUp  []probeResult
	weightedUp  []probeResult
	seenAt      time.Time
}

type groupData struct {
	all []probeResult
	up  []probeResult
}

func (c *cacheState) set(group string, res []probeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[group] = res
}

func (c *cacheState) get(group string) []probeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last[group]
}

func (c *cacheState) lbSet(group string, data groupData, hydratedAll, hydratedUp []probeResult) *cachedGroup {
	weightedUp := buildWeightedPool(hydratedUp)
	c.mu.Lock()
	defer c.mu.Unlock()
	cg := cachedGroup{data: data, hydratedAll: hydratedAll, hydratedUp: hydratedUp, weightedUp: weightedUp, seenAt: time.Now()}
	c.lb[group] = cg
	return &cg
}

func (c *cacheState) lbGetFresh(group string, maxAge time.Duration) *cachedGroup {
	c.mu.Lock()
	defer c.mu.Unlock()
	cg, ok := c.lb[group]
	if !ok || time.Since(cg.seenAt) > maxAge {
		return nil
	}
	return &cg
}

func (c *cacheState) redisBackoffActive(group string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.backoff[group]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(c.backoff, group)
		return false
	}
	return true
}

func (c *cacheState) markRedisDown(group string) {
	c.mu.Lock()
	c.backoff[group] = time.Now().Add(redisBackoffTTL)
	c.mu.Unlock()
}

func (c *cacheState) warnEvery(key string, interval time.Duration, msg string, args ...any) {
	now := time.Now()
	c.warnMu.Lock()
	last, ok := c.lastWarn[key]
	if ok && now.Sub(last) < interval {
		c.warnMu.Unlock()
		return
	}
	c.lastWarn[key] = now
	c.warnMu.Unlock()
	slog.Warn(msg, args...)
}

func (c *cacheState) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = make(map[string][]probeResult)
	c.lb = make(map[string]cachedGroup)
	c.backoff = make(map[string]time.Time)
}
