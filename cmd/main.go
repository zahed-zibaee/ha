package main

import (
	"context"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"ha/api"
	"ha/checks"
	"ha/config"
	"ha/envutil"
	"ha/logger"
	"ha/redisstore"
)

const leaderLockKey = "ha:leader"

type leadershipSettings struct {
	lockTTL         time.Duration
	renewEvery      time.Duration
	redisTimeout    time.Duration
	redisMaxRetries int
	redisBackoffMin time.Duration
	redisBackoffMax time.Duration
}

var renewLeaderLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("EXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

func main() {
	logLevel := envutil.GetDefault("LOG_LEVEL", "warn")
	logFormat := envutil.GetDefault("LOG_FORMAT", "json")
	logger.Configure(logLevel, logFormat)

	cfg, err := config.Load("")
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	targetIndex, duplicate := buildTargetIndex(cfg)
	if len(duplicate) > 0 {
		slog.Warn("duplicate target names detected", "dupes", duplicate)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	nodeID, err := os.Hostname()
	if err != nil || nodeID == "" {
		slog.Warn("hostname unavailable; using fallback node id", "err", err)
		nodeID = "unknown"
	}
	leaderTracker := newLeaderTracker(nodeID)
	leaderCfg := loadLeadershipSettings()

	redisOpts := redisstore.FromEnv()
	rawRedis := redisstore.NewClient(redisOpts)
	if err := redisstore.Ping(context.Background(), rawRedis); err != nil {
		slog.Warn("redis ping failed", "err", err, "addr", redisOpts.Addr)
	} else {
		slog.Info("redis connected", "addr", redisOpts.Addr)
	}

	logTargets(cfg)
	go leaderLoop(ctx, rawRedis, nodeID, cfg, leaderTracker, leaderCfg)

	listen := envutil.GetDefault("LISTEN_ADDR", ":8080")
	go func() {
		lbStrategy := envutil.GetDefault("LB_STRATEGY", "random")
		lbResponses := buildLBResponseIndex(cfg)
		lbTypes := buildLBTypeIndex(cfg)
		srv := api.NewServer(rawRedis, lbStrategy, targetIndex, lbResponses, lbTypes, leaderTracker.snapshot)
		if err := srv.Start(ctx, listen); err != nil {
			slog.Error("http server exited", "err", err)
			cancelAndExit(stop)
		}
	}()

	slog.Info("ha starting",
		"check_groups", len(cfg.Checks),
		"node_id", nodeID,
		"leader_lock_key", leaderLockKey,
		"leader_lock_ttl", leaderCfg.lockTTL.String(),
		"leader_renew_every", leaderCfg.renewEvery.String(),
		"leader_redis_timeout", leaderCfg.redisTimeout.String(),
		"leader_redis_max_retries", leaderCfg.redisMaxRetries,
		"redis_addr", redisOpts.Addr,
		"log_level", logLevel,
		"log_format", logFormat,
	)
	<-ctx.Done()
	slog.Info("shutdown signal received")
}

func startChecks(ctx context.Context, cfg *config.Config, rdb *redis.Client) []*sync.WaitGroup {
	var wgs []*sync.WaitGroup
	started := 0
	for name, chk := range cfg.Checks {
		if chk.Type != "http" {
			continue
		}
		targetDetails := make([]map[string]any, 0, len(chk.Targets))
		for _, t := range chk.Targets {
			headers := make([]string, 0, len(t.Headers))
			for k := range t.Headers {
				headers = append(headers, k)
			}
			targetDetails = append(targetDetails, map[string]any{
				"name":             t.Name,
				"url":              t.URL,
				"method":           t.Method,
				"expect_status":    t.ExpectStatus,
				"headers":          headers,
				"auth_basic":       t.AuthBasicUser != "" || t.AuthBasicPass != "",
				"auth_bearer":      t.AuthBearer != "",
				"follow_redirects": t.FollowRedirects,
				"max_redirects":    t.MaxRedirects,
				"interval":         t.Interval.String(),
				"timeout":          t.Timeout.String(),
				"retry":            t.Retry,
				"redis_ttl":        t.RedisTTL.String(),
			})
		}
		slog.Debug("http check group registered", "group", name, "targets", targetDetails)
		wg := checks.StartHTTPGroup(ctx, name, chk.Targets, rdb)
		wgs = append(wgs, wg)
		started++
	}
	if started == 0 {
		slog.Warn("no runnable check groups (only http supported now); probes not started")
	}
	return wgs
}

func logTargets(cfg *config.Config) {
	for name, chk := range cfg.Checks {
		targetNames := make([]string, 0, len(chk.Targets))
		for _, t := range chk.Targets {
			targetNames = append(targetNames, t.Name)
		}
		slog.Info("registered check group", "group", name, "type", chk.Type, "targets", len(chk.Targets), "names", targetNames)
	}
}

func buildTargetIndex(cfg *config.Config) (map[string]map[string]config.Target, []string) {
	index := make(map[string]map[string]config.Target)
	var dupes []string
	for group, chk := range cfg.Checks {
		if index[group] == nil {
			index[group] = make(map[string]config.Target)
		}
		for _, t := range chk.Targets {
			if _, exists := index[group][t.Name]; exists {
				dupes = append(dupes, group+":"+t.Name)
				continue
			}
			index[group][t.Name] = t
		}
	}
	return index, dupes
}

func buildLBResponseIndex(cfg *config.Config) map[string]map[string]map[string]any {
	index := make(map[string]map[string]map[string]any)
	for _, chk := range cfg.Checks {
		if len(chk.LB.TargetGroupResponses) == 0 {
			continue
		}
		for _, groupResp := range chk.LB.TargetGroupResponses {
			targetGroup := groupResp.TargetGroup
			if index[targetGroup] == nil {
				index[targetGroup] = make(map[string]map[string]any)
			}
			for _, rt := range groupResp.Targets {
				if rt.Response == nil {
					continue
				}
				resp := make(map[string]any, len(rt.Response)+1)
				for k, v := range rt.Response {
					resp[k] = v
				}
				if _, ok := resp["name"]; !ok {
					resp["name"] = rt.Name
				}
				index[targetGroup][rt.Name] = resp
			}
		}
	}
	return index
}

func buildLBTypeIndex(cfg *config.Config) map[string]string {
	index := make(map[string]string)
	for group, chk := range cfg.Checks {
		if chk.LB.Type == "" {
			continue
		}
		index[group] = chk.LB.Type
	}
	return index
}

func cancelAndExit(stop context.CancelFunc) {
	stop()
	os.Exit(1)
}

func leaderLoop(parent context.Context, rdb *redis.Client, nodeID string, cfg *config.Config, tracker *leaderTracker, settings leadershipSettings) {
	ticker := time.NewTicker(settings.renewEvery)
	defer ticker.Stop()

	var (
		cancel         context.CancelFunc
		activeWgs      []*sync.WaitGroup
		holdsLock      bool
		redisErrStreak int
	)

	stopProbeLoops := func(reason string) {
		if cancel == nil {
			tracker.transition("follower", reason, false, false, true)
			return
		}
		slog.Info("stopping checks", "node", nodeID, "reason", reason)
		cancel()
		cancel = nil
		for _, wg := range activeWgs {
			wg.Wait()
		}
		activeWgs = nil
		tracker.transition("follower", reason, holdsLock, false, true)
	}

	startProbeLoops := func(reason string) {
		if cancel != nil {
			return
		}
		probeCtx, c := context.WithCancel(parent)
		cancel = c
		activeWgs = startChecks(probeCtx, cfg, rdb)
		if holdsLock {
			tracker.transition("leader", reason, true, true, true)
		} else {
			tracker.transition("degraded", reason, false, true, true)
		}
		slog.Info("starting checks", "node", nodeID, "key", leaderLockKey, "mode", tracker.snapshot().State, "reason", reason)
	}

	evaluateLeadership := func() {
		if holdsLock {
			renewed, err := retryRedisBool(parent, settings, func(opCtx context.Context) (bool, error) {
				return renewLeaderLock(opCtx, rdb, nodeID, settings.lockTTL)
			})
			if err != nil {
				redisErrStreak++
				slog.Warn("leader lock renew failed; running checks without lock until Redis recovers", "node", nodeID, "err", err, "streak", redisErrStreak)
				holdsLock = false
				tracker.setLockHeld(false, "redis_error", "degraded")
				stopProbeLoops("redis_error")
				startProbeLoops("redis_error")
				return
			}
			redisErrStreak = 0
			if !renewed {
				slog.Warn("leader lock lost; stopping checks (another replica holds the lock)", "node", nodeID)
				holdsLock = false
				tracker.setLockHeld(false, "lock_lost", "follower")
				stopProbeLoops("lock_lost")
				return
			}
			tracker.transition("leader", "lock_renewed", true, cancel != nil, true)
			return
		}

		acquired, err := retryRedisBool(parent, settings, func(opCtx context.Context) (bool, error) {
			return acquireOrReclaimLeaderLock(opCtx, rdb, nodeID, settings.lockTTL)
		})
		if err != nil {
			redisErrStreak++
			tracker.setLockHeld(false, "redis_error", "degraded")
			if cancel == nil {
				slog.Warn("leader lock acquire failed; starting checks without Redis lock until Redis recovers", "node", nodeID, "err", err, "streak", redisErrStreak)
				startProbeLoops("redis_error")
			}
			if redisErrStreak >= 3 && cancel != nil {
				slog.Warn("redis error watchdog restart", "node", nodeID, "streak", redisErrStreak)
				stopProbeLoops("watchdog_restart")
				startProbeLoops("watchdog_restart")
				redisErrStreak = 0
			}
			return
		}
		redisErrStreak = 0
		if acquired {
			holdsLock = true
			tracker.setLockHeld(true, "lock_acquired", "leader")
			if cancel == nil {
				startProbeLoops("lock_acquired")
			}
			slog.Info("leadership acquired; holding Redis lock and running checks", "node", nodeID, "key", leaderLockKey)
			return
		}

		tracker.setLockHeld(false, "peer_leader", "follower")
		if cancel != nil {
			slog.Debug("redis reachable and another replica is leader; stopping local checks", "node", nodeID)
			stopProbeLoops("follower")
		}
		tracker.transition("follower", "peer_leader", false, cancel != nil, true)
	}

	defer func() {
		stopProbeLoops("shutdown")
		tracker.transition("follower", "shutdown", false, false, false)
	}()

	tracker.transition("initializing", "boot", false, false, false)
	evaluateLeadership()

	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
			evaluateLeadership()
		}
	}
}

func acquireOrReclaimLeaderLock(ctx context.Context, rdb *redis.Client, nodeID string, lockTTL time.Duration) (bool, error) {
	acquired, err := rdb.SetNX(ctx, leaderLockKey, nodeID, lockTTL).Result()
	if err != nil {
		return false, err
	}
	if acquired {
		return true, nil
	}

	currentOwner, err := rdb.Get(ctx, leaderLockKey).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if currentOwner != nodeID {
		return false, nil
	}
	return renewLeaderLock(ctx, rdb, nodeID, lockTTL)
}

func renewLeaderLock(ctx context.Context, rdb *redis.Client, nodeID string, lockTTL time.Duration) (bool, error) {
	ttlSeconds := int(lockTTL.Seconds())
	if ttlSeconds <= 0 {
		ttlSeconds = 1
	}
	res, err := renewLeaderLockScript.Run(ctx, rdb, []string{leaderLockKey}, nodeID, ttlSeconds).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

type leaderTracker struct {
	mu           sync.RWMutex
	lockHeld     bool
	probesActive bool
	ready        bool
	state        string
	reason       string
	nodeID       string
	since        time.Time
}

func newLeaderTracker(nodeID string) *leaderTracker {
	return &leaderTracker{nodeID: nodeID, since: time.Now(), state: "initializing", reason: "boot", ready: false}
}

func (t *leaderTracker) setLockHeld(v bool, reason string, state string) {
	t.mu.Lock()
	if t.lockHeld != v {
		t.lockHeld = v
		t.since = time.Now()
	}
	if state != "" {
		t.state = state
	}
	if reason != "" {
		t.reason = reason
	}
	t.mu.Unlock()
}

func (t *leaderTracker) transition(state, reason string, lockHeld, probesActive, ready bool) {
	t.mu.Lock()
	if t.lockHeld != lockHeld || t.probesActive != probesActive || t.state != state || t.reason != reason || t.ready != ready {
		t.lockHeld = lockHeld
		t.probesActive = probesActive
		t.state = state
		t.reason = reason
		t.ready = ready
		t.since = time.Now()
	}
	t.mu.Unlock()
}

func (t *leaderTracker) snapshot() api.LeaderStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return api.LeaderStatus{
		Leader:       t.lockHeld,
		ProbesActive: t.probesActive,
		Ready:        t.ready,
		State:        t.state,
		Reason:       t.reason,
		NodeID:       t.nodeID,
		Since:        t.since,
	}
}

func loadLeadershipSettings() leadershipSettings {
	lockTTL := parseDurationEnv("LOCK_TTL", 10*time.Second)
	renewEvery := parseDurationEnv("LOCK_RENEW_EVERY", 5*time.Second)
	redisTimeout := parseDurationEnv("LOCK_REDIS_TIMEOUT", 2*time.Second)
	if renewEvery <= 0 {
		renewEvery = 5 * time.Second
	}
	if lockTTL <= renewEvery {
		lockTTL = renewEvery * 2
	}
	return leadershipSettings{
		lockTTL:         lockTTL,
		renewEvery:      renewEvery,
		redisTimeout:    redisTimeout,
		redisMaxRetries: parseIntEnv("LOCK_REDIS_MAX_RETRIES", 3),
		redisBackoffMin: parseDurationEnv("LOCK_REDIS_BACKOFF_MIN", 100*time.Millisecond),
		redisBackoffMax: parseDurationEnv("LOCK_REDIS_BACKOFF_MAX", 700*time.Millisecond),
	}
}

func parseDurationEnv(name string, fallback time.Duration) time.Duration {
	raw := envutil.GetDefault(name, "")
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func parseIntEnv(name string, fallback int) int {
	raw := envutil.GetDefault(name, "")
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func retryRedisBool(parent context.Context, settings leadershipSettings, fn func(context.Context) (bool, error)) (bool, error) {
	attempts := settings.redisMaxRetries
	if attempts < 1 {
		attempts = 1
	}
	backoff := settings.redisBackoffMin
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	maxBackoff := settings.redisBackoffMax
	if maxBackoff < backoff {
		maxBackoff = backoff
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		opCtx, opCancel := context.WithTimeout(parent, settings.redisTimeout)
		ok, err := fn(opCtx)
		opCancel()
		if err == nil {
			return ok, nil
		}
		lastErr = err
		if i == attempts-1 {
			break
		}
		jitter := time.Duration(rand.Int63n(int64(backoff/2 + 1)))
		sleepFor := backoff + jitter
		select {
		case <-parent.Done():
			return false, parent.Err()
		case <-time.After(sleepFor):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return false, lastErr
}
