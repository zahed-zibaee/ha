package api

import (
	"context"
	"encoding/json"
	"errors"
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
	SRandMemberN(ctx context.Context, key string, count int64) *redis.StringSliceCmd
}

// LeaderStatus captures current leadership state for the node.
type LeaderStatus struct {
	Leader bool
	State  string
	NodeID string
	Since  time.Time
}

// Start HTTP server.
func Start(ctx context.Context, addr string, rdb RedisClient, lbStrategy string, targets map[string]map[string]config.Target, leaderFn func() LeaderStatus, joinFn func(id, addr string) (int, string, error)) error {
	selector := newSelector(lbStrategy, targets)
	mux := http.NewServeMux()
	mux.Handle("/v1/check/", checkHandler(rdb, targets))
	mux.Handle("/v1/lb/", lbHandler(rdb, selector, targets))
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

type targetConnect struct {
	Name     string `json:"name,omitempty"`
	URL      string `json:"url,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Key      string `json:"key,omitempty"`
	IP       string `json:"ip,omitempty"`
}

type lbResponse struct {
	Group     string        `json:"group"`
	Target    targetConnect `json:"target"`
	Reachable bool          `json:"reachable"`
	Error     string        `json:"error,omitempty"`
}

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

func lbHandler(rdb RedisClient, selector *lbSelector, index map[string]map[string]config.Target) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := strings.TrimPrefix(r.URL.Path, "/v1/lb/")
		if group == "" {
			http.Error(w, "group required", http.StatusBadRequest)
			return
		}
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
			pick := selector.pickHydrated(group, cg.hydratedUp, cg.hydratedAll)
			path = "cache"
			cacheHit = true
			targets = len(cg.hydratedAll)
			reachable = len(cg.hydratedUp)
			writeLB(w, lbResponse{Group: group, Target: toConnect(pick.TargetMeta), Reachable: pick.Reachable, Error: pick.Error})
			return
		}
		if redisBackoffActive(group) {
			if cg := cacheFromConfig(group, selector, index, "redis_backoff"); cg != nil {
				path = "redis_backoff"
				errType = "redis_backoff"
				targets = len(cg.hydratedAll)
				reachable = len(cg.hydratedUp)
				pick := selector.pickHydrated(group, cg.hydratedUp, cg.hydratedAll)
				writeLB(w, lbResponse{Group: group, Target: toConnect(pick.TargetMeta), Reachable: pick.Reachable, Error: "redis_backoff"})
				return
			}
		}
		redisCtx, cancel := context.WithTimeout(r.Context(), redisLBTimeout)
		defer cancel()
		var res []probeResult
		if selector.strategy == "random" {
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
					pick := selector.pickHydrated(group, cg.hydratedUp, cg.hydratedAll)
					writeLB(w, lbResponse{Group: group, Target: toConnect(pick.TargetMeta), Reachable: pick.Reachable, Error: "redis_error: " + err.Error()})
					return
				}
				writeLB(w, lbResponse{Group: group, Reachable: false, Error: "redis_error: " + err.Error()})
				return
			} else if ok {
				if tmap := index[group]; tmap != nil {
					if cfg, found := tmap[name]; found {
						path = "redis_up_name"
						targets = 1
						reachable = 1
						cacheFromConfigWithReachable(group, selector, index, map[string]struct{}{name: {}}, "")
						writeLB(w, lbResponse{Group: group, Target: connectFromConfig(cfg), Reachable: true})
						return
					}
					warnEvery("lb:missing_config:"+group, 5*time.Second, "redis up target missing config", "group", group, "target", name)
					errType = "missing_config"
					if cg := cacheFromConfig(group, selector, index, "missing_config"); cg != nil {
						path = "config_fallback"
						targets = len(cg.hydratedAll)
						reachable = len(cg.hydratedUp)
						pick := selector.pickHydrated(group, cg.hydratedUp, cg.hydratedAll)
						writeLB(w, lbResponse{Group: group, Target: toConnect(pick.TargetMeta), Reachable: pick.Reachable, Error: "missing_config"})
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
					pick := selector.pickHydrated(group, cg.hydratedUp, cg.hydratedAll)
					writeLB(w, lbResponse{Group: group, Target: toConnect(pick.TargetMeta), Reachable: pick.Reachable, Error: "redis_up_empty"})
					return
				}
				writeLB(w, lbResponse{Group: group, Reachable: false, Error: "redis_up_empty"})
				return
			}
		}
		if selector.strategy != "random" {
			if cg := cacheFromConfig(group, selector, index, "config_only"); cg != nil {
				path = "config_rr"
				targets = len(cg.hydratedAll)
				reachable = len(cg.hydratedUp)
				errType = "config_only"
				pick := selector.pickHydrated(group, cg.hydratedUp, cg.hydratedAll)
				writeLB(w, lbResponse{Group: group, Target: toConnect(pick.TargetMeta), Reachable: pick.Reachable, Error: pick.Error})
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
					writeLB(w, lbResponse{Group: group, Reachable: false, Error: "redis_error: " + err.Error()})
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
			writeLB(w, lbResponse{Group: group, Reachable: false, Error: "no targets found"})
			return
		}
		hydratedAll := hydrate(group, res, index)
		hydratedUp := reachableFromAll(hydratedAll)
		targets = len(hydratedAll)
		reachable = len(hydratedUp)
		pick := selector.pickHydrated(group, hydratedUp, hydratedAll)
		lbCacheSet(group, groupData{all: res, up: hydratedUp}, hydratedAll, hydratedUp)
		if path == "unknown" {
			path = "hydrate"
		}
		writeLB(w, lbResponse{Group: group, Target: toConnect(pick.TargetMeta), Reachable: pick.Reachable, Error: pick.Error})
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

func loadOneUpName(ctx context.Context, rdb RedisClient, group string) (string, bool, error) {
	select {
	case redisSem <- struct{}{}:
		defer func() { <-redisSem }()
	default:
		slog.Debug("redis concurrency high; proceeding without waiting", "group", group)
	}
	upKey := "hc:" + group + ":up"
	names, err := rdb.SRandMemberN(ctx, upKey, 1).Result()
	if err != nil {
		return "", false, err
	}
	if len(names) == 0 {
		return "", false, nil
	}
	return names[0], true, nil
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

func lbCacheSet(group string, data groupData, hydratedAll, hydratedUp []probeResult) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	lbCache[group] = cachedGroup{data: data, hydratedAll: hydratedAll, hydratedUp: hydratedUp, seenAt: time.Now()}
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
	cg := cachedGroup{data: groupData{}, hydratedAll: hydratedAll, hydratedUp: hydratedUp, seenAt: time.Now()}
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

// Selector

type lbSelector struct {
	strategy string
	mu       sync.Mutex
	counter  map[string]int
	targets  map[string]map[string]config.Target
	ordered  map[string][]string
}

func newSelector(strategy string, targets map[string]map[string]config.Target) *lbSelector {
	s := strings.ToLower(strategy)
	if s != "round-robin" && s != "roundrobin" && s != "rr" {
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
	return &lbSelector{strategy: s, counter: make(map[string]int), targets: targets, ordered: ordered}
}

func (s *lbSelector) pickHydrated(group string, reachable, all []probeResult) probeResult {
	if len(reachable) == 0 {
		return all[0]
	}
	if s.strategy == "random" {
		return reachable[rand.Intn(len(reachable))]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.counter[group] % len(reachable)
	s.counter[group]++
	return reachable[idx]
}

func toConnect(tm targetMeta) targetConnect {
	return targetConnect{
		Name:     tm.Name,
		URL:      tm.URL,
		Endpoint: tm.Endpoint,
		Bucket:   tm.Bucket,
		Key:      tm.Key,
		IP:       tm.IP,
	}
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

func connectFromConfig(cfg config.Target) targetConnect {
	return toConnect(targetMetaFromConfig(cfg))
}
