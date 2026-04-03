package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"ha/config"
)

// RedisClient defines redis operations needed by the API.
type RedisClient interface {
	HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd
	HMGet(ctx context.Context, key string, fields ...string) *redis.SliceCmd
	ZRangeWithScores(ctx context.Context, key string, start, stop int64) *redis.ZSliceCmd
}

// LeaderStatus captures current leadership state for the node.
type LeaderStatus struct {
	Leader bool
	State  string
	NodeID string
	Since  time.Time
}

// Start HTTP server.
func Start(ctx context.Context, addr string, rdb RedisClient, lbStrategy string, targets map[string]map[string]config.Target, lbResponses map[string]map[string]map[string]any, lbTypes map[string]string, leaderFn func() LeaderStatus, joinFn func(id, addr string) (int, string, error)) error {
	selector := newSelector(lbStrategy, targets, lbTypes)
	mux := http.NewServeMux()
	mux.Handle("/v1/check/", checkHandler(rdb, targets))
	mux.Handle("/v1/lb/", lbHandler(rdb, selector, targets, lbResponses))
	mux.Handle("/v1/leader", leaderHandler(leaderFn))
	if joinFn != nil {
		mux.Handle("/v1/raft/join", joinHandler(joinFn))
	}
	mux.Handle("/metrics", metricsHandler())
	mux.Handle("/health", healthHandler())
	handler := httpLoggingMiddleware(mux)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		<-ctx.Done()
		slog.Info("http server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("http server listening", "addr", addr)
	err := srv.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func httpLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", status, "bytes", rec.bytes, "duration_ms", time.Since(start).Milliseconds())
	})
}

// Data types.
type probeResult struct {
	Reachable  bool       `json:"reachable"`
	Status     string     `json:"status,omitempty"`
	CheckedAt  int64      `json:"checked_at"`
	LatencyMs  int64      `json:"latency_ms"`
	Error      string     `json:"error,omitempty"`
	Type       string     `json:"type"`
	Target     string     `json:"target"`
	TargetMeta targetMeta `json:"target_meta,omitempty"`
	Stale      bool       `json:"stale,omitempty"`
}

type targetMeta struct {
	Name         string `json:"name,omitempty"`
	URL          string `json:"url,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
	Key          string `json:"key,omitempty"`
	IP           string `json:"ip,omitempty"`
	IntervalMS   int64  `json:"interval_ms,omitempty"`
	TimeoutMS    int64  `json:"timeout_ms,omitempty"`
	Retry        int    `json:"retry,omitempty"`
	JitterPct    int    `json:"jitter_pct,omitempty"`
	ExpectStatus []int  `json:"expect_status,omitempty"`
}

type checkResponse struct {
	Group   string        `json:"group"`
	Targets []probeResult `json:"targets"`
	Message string        `json:"message,omitempty"`
	Redis   string        `json:"redis_status,omitempty"`
}

type lbResponse map[string]any

type leaderResponse struct {
	Leader    bool   `json:"leader"`
	Status    string `json:"status"`
	NodeID    string `json:"node_id,omitempty"`
	RaftState string `json:"raft_state,omitempty"`
	SinceUnix int64  `json:"since_unix,omitempty"`
}

type joinRequest struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type joinResponse struct {
	OK     bool   `json:"ok"`
	Leader string `json:"leader,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Handlers.
func checkHandler(rdb RedisClient, index map[string]map[string]config.Target) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := strings.TrimPrefix(r.URL.Path, "/v1/check/")
		if group == "" {
			http.Error(w, "group required", http.StatusBadRequest)
			return
		}
		start := time.Now()
		redisStatus := "ok"
		errStr := ""
		targets := 0
		defer func() {
			slog.Debug("check request", "group", group, "targets", targets, "redis_status", redisStatus, "err", errStr, "duration_ms", time.Since(start).Milliseconds())
			checkRequests.WithLabelValues(redisStatus).Inc()
			checkLatencyMs.WithLabelValues(redisStatus).Observe(float64(time.Since(start).Milliseconds()))
			checkTargets.WithLabelValues(redisStatus).Set(float64(targets))
		}()
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		res, err := loadGroup(ctx, rdb, group)
		if err != nil {
			warnEvery("check:redis_error:"+group, 5*time.Second, "check read failed", "group", group, "err", err)
			w.WriteHeader(http.StatusOK)
			redisStatus = "error"
			errStr = err.Error()
			_ = json.NewEncoder(w).Encode(checkResponse{Group: group, Redis: "error", Message: err.Error()})
			return
		}
		hydrated := hydrate(group, res, index)
		targets = len(hydrated)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(checkResponse{Group: group, Targets: hydrated, Redis: redisStatus})
	})
}

func leaderHandler(leaderFn func() LeaderStatus) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leader := false
		status := "unknown"
		nodeID := ""
		raftState := ""
		var since time.Time
		if leaderFn != nil {
			ls := leaderFn()
			leader = ls.Leader
			nodeID = ls.NodeID
			raftState = ls.State
			since = ls.Since
			if leader {
				status = "leader"
			} else {
				status = "follower"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		var sinceUnix int64
		if !since.IsZero() {
			sinceUnix = since.Unix()
		}
		_ = json.NewEncoder(w).Encode(leaderResponse{Leader: leader, Status: status, NodeID: nodeID, RaftState: raftState, SinceUnix: sinceUnix})
	})
}

func joinHandler(joinFn func(id, addr string) (int, string, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req joinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.ID == "" || req.Addr == "" {
			http.Error(w, "id and addr required", http.StatusBadRequest)
			return
		}
		status, leader, err := joinFn(req.ID, req.Addr)
		if status == 0 {
			status = http.StatusOK
		}
		resp := joinResponse{OK: status == http.StatusOK, Leader: leader}
		if err != nil {
			resp.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
}

func lbHandler(rdb RedisClient, selector *lbSelector, index map[string]map[string]config.Target, lbResponses map[string]map[string]map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := strings.TrimPrefix(r.URL.Path, "/v1/lb/")
		if group == "" {
			http.Error(w, "group required", http.StatusBadRequest)
			return
		}
		groupStrategy := selector.strategyForGroup(group)
		start := time.Now()
		path := "unknown"
		cacheHit := false
		errStr := ""
		errType := "none"
		targets := 0
		reachable := 0
		defer func() {
			slog.Debug("lb request", "group", group, "path", path, "cache_hit", cacheHit, "targets", targets, "reachable", reachable, "err", errStr, "duration_ms", time.Since(start).Milliseconds())
			lbRequests.WithLabelValues(path, strconv.FormatBool(cacheHit)).Inc()
			lbLatencyMs.WithLabelValues(path).Observe(float64(time.Since(start).Milliseconds()))
			if errType != "none" {
				lbErrors.WithLabelValues(errType).Inc()
			}
		}()
		if cg := lbCacheGetFresh(group, lbCacheMaxAge); cg != nil {
			pick := pickFromCacheGroup(group, selector, cg)
			path = "cache"
			cacheHit = true
			targets = len(cg.hydratedAll)
			reachable = len(cg.hydratedUp)
			writeLB(w, buildLBResponse(group, pick.Reachable, pick.Error, &pick, lbResponses))
			return
		}
		if redisBackoffActive(group) {
			if cg := cacheFromConfig(group, selector, index, "redis_backoff"); cg != nil {
				path = "redis_backoff"
				errType = "redis_backoff"
				targets = len(cg.hydratedAll)
				reachable = len(cg.hydratedUp)
				pick := pickFromCacheGroup(group, selector, cg)
				writeLB(w, buildLBResponse(group, pick.Reachable, "redis_backoff", &pick, lbResponses))
				return
			}
		}
		redisCtx, cancel := context.WithTimeout(r.Context(), redisLBTimeout)
		defer cancel()
		var res []probeResult
		if selector.isWeighted(group) {
			zs, err := loadUpScoresWithTimeout(redisCtx, rdb, group)
			if err != nil {
				markRedisDown(group)
				warnEvery("lb:redis_up_error:"+group, 5*time.Second, "lb read failed", "group", group, "err", err)
				path = "redis_up_error"
				errStr = err.Error()
				if errors.Is(err, context.DeadlineExceeded) {
					errType = "redis_timeout"
				} else {
					errType = "redis_error"
				}
				if cg := cacheFromConfig(group, selector, index, "redis_up_error"); cg != nil {
					path = "config_fallback"
					targets = len(cg.hydratedAll)
					reachable = len(cg.hydratedUp)
					pick := pickFromCacheGroup(group, selector, cg)
					writeLB(w, buildLBResponse(group, pick.Reachable, "redis_error: "+err.Error(), &pick, lbResponses))
					return
				}
				writeLB(w, buildLBResponse(group, false, "redis_error: "+err.Error(), nil, lbResponses))
				return
			}
			if len(zs) == 0 {
				warnEvery("lb:redis_up_empty:"+group, 5*time.Second, "redis up set empty", "group", group)
				errType = "redis_up_empty"
				if cg := cacheFromConfig(group, selector, index, "redis_up_empty"); cg != nil {
					path = "config_fallback"
					targets = len(cg.hydratedAll)
					reachable = len(cg.hydratedUp)
					pick := pickFromCacheGroup(group, selector, cg)
					writeLB(w, buildLBResponse(group, pick.Reachable, "redis_up_empty", &pick, lbResponses))
					return
				}
				writeLB(w, buildLBResponse(group, false, "redis_up_empty", nil, lbResponses))
				return
			}
			tmap := index[group]
			hydratedUp := make([]probeResult, 0, len(zs))
			for _, z := range zs {
				name := fmt.Sprint(z.Member)
				cfg, found := tmap[name]
				if !found {
					warnEvery("lb:missing_config:"+group, 5*time.Second, "redis up target missing config", "group", group, "target", name)
					errType = "missing_config"
					continue
				}
				hydratedUp = append(hydratedUp, probeResult{
					Reachable:  true,
					Target:     cfg.Name,
					LatencyMs:  int64(z.Score),
					TargetMeta: targetMetaFromConfig(cfg),
				})
			}
			if len(hydratedUp) == 0 {
				if cg := cacheFromConfig(group, selector, index, "missing_config"); cg != nil {
					path = "config_fallback"
					targets = len(cg.hydratedAll)
					reachable = len(cg.hydratedUp)
					pick := pickFromCacheGroup(group, selector, cg)
					writeLB(w, buildLBResponse(group, pick.Reachable, "missing_config", &pick, lbResponses))
					return
				}
				writeLB(w, buildLBResponse(group, false, "missing_config", nil, lbResponses))
				return
			}
			hydratedAll := hydratedUp
			targets = len(hydratedAll)
			reachable = len(hydratedUp)
			path = "redis_up_scores"
			cg := lbCacheSet(group, groupData{all: nil, up: hydratedUp}, hydratedAll, hydratedUp)
			pick := pickFromCacheGroup(group, selector, cg)
			writeLB(w, buildLBResponse(group, pick.Reachable, pick.Error, &pick, lbResponses))
			return
		}
		if groupStrategy == "random" {
			if name, ok, err := loadOneUpNameWithTimeout(redisCtx, rdb, group); err != nil {
				markRedisDown(group)
				warnEvery("lb:redis_up_error:"+group, 5*time.Second, "lb read failed", "group", group, "err", err)
				path = "redis_up_error"
				errStr = err.Error()
				if errors.Is(err, context.DeadlineExceeded) {
					errType = "redis_timeout"
				} else {
					errType = "redis_error"
				}
				if cg := cacheFromConfig(group, selector, index, "redis_up_error"); cg != nil {
					path = "config_fallback"
					targets = len(cg.hydratedAll)
					reachable = len(cg.hydratedUp)
					pick := pickFromCacheGroup(group, selector, cg)
					writeLB(w, buildLBResponse(group, pick.Reachable, "redis_error: "+err.Error(), &pick, lbResponses))
					return
				}
				writeLB(w, buildLBResponse(group, false, "redis_error: "+err.Error(), nil, lbResponses))
				return
			} else if ok {
				if tmap := index[group]; tmap != nil {
					if cfg, found := tmap[name]; found {
						path = "redis_up_name"
						targets = 1
						reachable = 1
						cacheFromConfigWithReachable(group, selector, index, map[string]struct{}{name: {}}, "")
						pick := probeResult{Reachable: true, Target: cfg.Name, TargetMeta: targetMetaFromConfig(cfg)}
						writeLB(w, buildLBResponse(group, true, "", &pick, lbResponses))
						return
					}
					warnEvery("lb:missing_config:"+group, 5*time.Second, "redis up target missing config", "group", group, "target", name)
					errType = "missing_config"
					if cg := cacheFromConfig(group, selector, index, "missing_config"); cg != nil {
						path = "config_fallback"
						targets = len(cg.hydratedAll)
						reachable = len(cg.hydratedUp)
						pick := pickFromCacheGroup(group, selector, cg)
						writeLB(w, buildLBResponse(group, pick.Reachable, "missing_config", &pick, lbResponses))
						return
					}
				}
				// fall through to config fallback if target missing in config
			} else {
				warnEvery("lb:redis_up_empty:"+group, 5*time.Second, "redis up set empty", "group", group)
				errType = "redis_up_empty"
				if cg := cacheFromConfig(group, selector, index, "redis_up_empty"); cg != nil {
					path = "config_fallback"
					targets = len(cg.hydratedAll)
					reachable = len(cg.hydratedUp)
					pick := pickFromCacheGroup(group, selector, cg)
					writeLB(w, buildLBResponse(group, pick.Reachable, "redis_up_empty", &pick, lbResponses))
					return
				}
				writeLB(w, buildLBResponse(group, false, "redis_up_empty", nil, lbResponses))
				return
			}
		}
		if groupStrategy == "round-robin" {
			if cg := cacheFromConfig(group, selector, index, "config_only"); cg != nil {
				path = "config_rr"
				targets = len(cg.hydratedAll)
				reachable = len(cg.hydratedUp)
				errType = "config_only"
				pick := pickFromCacheGroup(group, selector, cg)
				writeLB(w, buildLBResponse(group, pick.Reachable, pick.Error, &pick, lbResponses))
				return
			}
		}
		if res == nil {
			var err error
			res, err = loadGroupWithTimeout(redisCtx, rdb, group)
			if err != nil {
				markRedisDown(group)
				warnEvery("lb:redis_hgetall_error:"+group, 5*time.Second, "lb read failed", "group", group, "err", err)
				path = "redis_hgetall_error"
				errStr = err.Error()
				if errors.Is(err, context.DeadlineExceeded) {
					errType = "redis_timeout"
				} else {
					errType = "redis_error"
				}
				fallback := cacheGet(group)
				if len(fallback) == 0 {
					writeLB(w, buildLBResponse(group, false, "redis_error: "+err.Error(), nil, lbResponses))
					return
				}
				path = "cache_fallback"
				cacheHit = true
				targets = len(fallback)
				res = fallback
			} else {
				path = "redis_hgetall"
			}
		}
		if len(res) == 0 {
			path = "empty"
			errType = "no_targets"
			writeLB(w, buildLBResponse(group, false, "no targets found", nil, lbResponses))
			return
		}
		hydratedAll := hydrate(group, res, index)
		hydratedUp := reachableFromAll(hydratedAll)
		targets = len(hydratedAll)
		reachable = len(hydratedUp)
		cg := lbCacheSet(group, groupData{all: res, up: hydratedUp}, hydratedAll, hydratedUp)
		pick := pickFromCacheGroup(group, selector, cg)
		if path == "unknown" {
			path = "hydrate"
		}
		writeLB(w, buildLBResponse(group, pick.Reachable, pick.Error, &pick, lbResponses))
	})
}

func writeLB(w http.ResponseWriter, resp lbResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// Data loading
func loadGroup(ctx context.Context, rdb RedisClient, group string) ([]probeResult, error) {
	select {
	case redisSem <- struct{}{}:
		defer func() { <-redisSem }()
	default:
		slog.Debug("redis concurrency high; proceeding without waiting", "group", group)
	}
	hashKey := "hc:" + group
	m, err := rdb.HGetAll(ctx, hashKey).Result()
	if err != nil {
		return nil, err
	}
	results := make([]probeResult, 0, len(m))
	for field, v := range m {
		var pr probeResult
		if err := json.Unmarshal([]byte(v), &pr); err != nil {
			continue
		}
		pr.Target = field
		results = append(results, pr)
	}
	cacheSet(group, results)
	return results, nil
}

func loadUpScores(ctx context.Context, rdb RedisClient, group string) ([]redis.Z, error) {
	select {
	case redisSem <- struct{}{}:
		defer func() { <-redisSem }()
	default:
		slog.Debug("redis concurrency high; proceeding without waiting", "group", group)
	}
	upKey := "hc:" + group + ":up"
	return rdb.ZRangeWithScores(ctx, upKey, 0, -1).Result()
}

func loadOneUpName(ctx context.Context, rdb RedisClient, group string) (string, bool, error) {
	zs, err := loadUpScores(ctx, rdb, group)
	if err != nil {
		return "", false, err
	}
	if len(zs) == 0 {
		return "", false, nil
	}
	pick := zs[rand.Intn(len(zs))].Member
	name, ok := pick.(string)
	if ok && name != "" {
		return name, true, nil
	}
	return fmt.Sprint(pick), true, nil
}

func loadUpScoresWithTimeout(ctx context.Context, rdb RedisClient, group string) ([]redis.Z, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		zs  []redis.Z
		err error
	}
	ch := make(chan result, 1)
	go func() {
		zs, err := loadUpScores(ctx, rdb, group)
		ch <- result{zs: zs, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-ch:
		return out.zs, out.err
	}
}

func loadGroupWithTimeout(ctx context.Context, rdb RedisClient, group string) ([]probeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		res []probeResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		res, err := loadGroup(ctx, rdb, group)
		ch <- result{res: res, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-ch:
		return out.res, out.err
	}
}

func loadOneUpNameWithTimeout(ctx context.Context, rdb RedisClient, group string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	type result struct {
		name string
		ok   bool
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		name, ok, err := loadOneUpName(ctx, rdb, group)
		ch <- result{name: name, ok: ok, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case out := <-ch:
		return out.name, out.ok, out.err
	}
}

// Caching helpers
var cacheMu sync.Mutex
var lastCache = map[string][]probeResult{}
var redisSem = make(chan struct{}, 256)
var lbCache = map[string]cachedGroup{}

const lbCacheMaxAge = 5 * time.Second
const redisLBTimeout = 200 * time.Millisecond
const redisBackoffTTL = 1 * time.Second
const weightedPoolMax = 1000

var redisBackoffMu sync.Mutex
var redisBackoffUntil = map[string]time.Time{}

func redisBackoffActive(group string) bool {
	redisBackoffMu.Lock()
	defer redisBackoffMu.Unlock()
	until, ok := redisBackoffUntil[group]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(redisBackoffUntil, group)
		return false
	}
	return true
}

func markRedisDown(group string) {
	redisBackoffMu.Lock()
	redisBackoffUntil[group] = time.Now().Add(redisBackoffTTL)
	redisBackoffMu.Unlock()
}

var warnMu sync.Mutex
var lastWarn = map[string]time.Time{}

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

func cacheSet(group string, res []probeResult) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	lastCache[group] = res
}

func cacheGet(group string) []probeResult {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return lastCache[group]
}

func lbCacheSet(group string, data groupData, hydratedAll, hydratedUp []probeResult) *cachedGroup {
	weightedUp := buildWeightedPool(hydratedUp)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cg := cachedGroup{data: data, hydratedAll: hydratedAll, hydratedUp: hydratedUp, weightedUp: weightedUp, seenAt: time.Now()}
	lbCache[group] = cg
	return &cg
}

func lbCacheGetFresh(group string, maxAge time.Duration) *cachedGroup {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cg, ok := lbCache[group]
	if !ok || time.Since(cg.seenAt) > maxAge {
		return nil
	}
	return &cg
}

func warnEvery(key string, interval time.Duration, msg string, args ...any) {
	now := time.Now()
	warnMu.Lock()
	last, ok := lastWarn[key]
	if ok && now.Sub(last) < interval {
		warnMu.Unlock()
		return
	}
	lastWarn[key] = now
	warnMu.Unlock()
	slog.Warn(msg, args...)
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

func cacheFromConfig(group string, selector *lbSelector, index map[string]map[string]config.Target, errMsg string) *cachedGroup {
	return cacheFromConfigWithReachable(group, selector, index, nil, errMsg)
}

func cacheFromConfigWithReachable(group string, selector *lbSelector, index map[string]map[string]config.Target, reachableNames map[string]struct{}, errMsg string) *cachedGroup {
	tmap := index[group]
	if len(tmap) == 0 {
		return nil
	}
	names := selector.ordered[group]
	if len(names) == 0 {
		names = make([]string, 0, len(tmap))
		for name := range tmap {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	hydratedAll := make([]probeResult, 0, len(names))
	hydratedUp := make([]probeResult, 0, len(names))
	useAllAsCandidates := reachableNames == nil
	for _, name := range names {
		cfg, ok := tmap[name]
		if !ok {
			continue
		}
		reachable := false
		if reachableNames != nil {
			_, reachable = reachableNames[name]
		}
		pr := probeResult{
			Reachable:  reachable,
			Target:     name,
			TargetMeta: targetMetaFromConfig(cfg),
		}
		if !reachable && errMsg != "" {
			pr.Error = errMsg
		}
		hydratedAll = append(hydratedAll, pr)
		if reachable || useAllAsCandidates {
			hydratedUp = append(hydratedUp, pr)
		}
	}
	cg := cachedGroup{data: groupData{}, hydratedAll: hydratedAll, hydratedUp: hydratedUp, weightedUp: buildWeightedPool(hydratedUp), seenAt: time.Now()}
	cacheMu.Lock()
	lbCache[group] = cg
	cacheMu.Unlock()
	return &cg
}

// Hydration and selection helpers
func hydrate(group string, stored []probeResult, index map[string]map[string]config.Target) []probeResult {
	out := make([]probeResult, 0, len(stored))
	tmap := index[group]
	for _, pr := range stored {
		if tmap != nil {
			if cfg, ok := tmap[pr.Target]; ok {
				pr.TargetMeta = targetMetaFromConfig(cfg)
			} else {
				warnEvery("hydrate:missing_config:"+group, 5*time.Second, "redis entry missing config", "group", group, "target", pr.Target)
			}
		}
		out = append(out, pr)
	}
	return out
}

func reachableFromAll(all []probeResult) []probeResult {
	var out []probeResult
	for _, pr := range all {
		if pr.Reachable {
			out = append(out, pr)
		}
	}
	return out
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

// Selector

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

func targetMetaFromConfig(cfg config.Target) targetMeta {
	return targetMeta{
		Name:     cfg.Name,
		URL:      cfg.URL,
		Endpoint: cfg.Endpoint,
		Bucket:   cfg.Bucket,
		Key:      cfg.Key,
		IP:       cfg.Host,
	}
}

func buildLBResponse(group string, reachable bool, errMsg string, pick *probeResult, overrides map[string]map[string]map[string]any) lbResponse {
	resp := lbResponse{
		"group":     group,
		"reachable": reachable,
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}

	name := ""
	var meta targetMeta
	if pick != nil {
		if pick.TargetMeta.Name != "" {
			name = pick.TargetMeta.Name
		} else {
			name = pick.Target
		}
		meta = pick.TargetMeta
	}

	if name != "" && overrides != nil {
		if g := overrides[group]; g != nil {
			if ov, ok := g[name]; ok && ov != nil {
				for k, v := range ov {
					if k == "group" || k == "reachable" || k == "error" {
						continue
					}
					resp[k] = v
				}
				if v, ok := resp["name"]; !ok {
					resp["name"] = name
				} else if s, ok := v.(string); ok && s == "" {
					resp["name"] = name
				}
				return resp
			}
		}
	}

	resp["name"] = name
	if meta.URL != "" {
		resp["url"] = meta.URL
	}
	if meta.Endpoint != "" {
		resp["endpoint"] = meta.Endpoint
	}
	if meta.Bucket != "" {
		resp["bucket"] = meta.Bucket
	}
	if meta.Key != "" {
		resp["key"] = meta.Key
	}
	if meta.IP != "" {
		resp["ip"] = meta.IP
	}
	return resp
}
