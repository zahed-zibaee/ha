package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) checkHandler() http.Handler {
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
		res, err := s.loadGroup(ctx, group)
		if err != nil {
			s.cache.warnEvery("check:redis_error:"+group, 5*time.Second, "check read failed", "group", group, "err", err)
			w.WriteHeader(http.StatusOK)
			redisStatus = "error"
			errStr = err.Error()
			_ = json.NewEncoder(w).Encode(checkResponse{Group: group, Redis: "error", Message: err.Error()})
			return
		}
		hydrated := hydrate(group, res, s.targets, s.cache)
		targets = len(hydrated)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(checkResponse{Group: group, Targets: hydrated, Redis: redisStatus})
	})
}

func (s *Server) leaderHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leader := false
		probes := false
		status := "unknown"
		nodeID := ""
		var since time.Time
		if s.leaderFn != nil {
			ls := s.leaderFn()
			leader = ls.Leader
			probes = ls.ProbesActive
			nodeID = ls.NodeID
			since = ls.Since
			switch {
			case leader:
				status = "leader"
			case probes:
				status = "degraded"
			default:
				status = "follower"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		var sinceUnix int64
		if !since.IsZero() {
			sinceUnix = since.Unix()
		}
		_ = json.NewEncoder(w).Encode(leaderResponse{
			Leader: leader, ProbesActive: probes, Status: status, NodeID: nodeID, SinceUnix: sinceUnix,
		})
	})
}

func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
}

// lbHandler serves /v1/lb/{group} with a multi-layer fallback chain.
func (s *Server) lbHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := strings.TrimPrefix(r.URL.Path, "/v1/lb/")
		if group == "" {
			http.Error(w, "group required", http.StatusBadRequest)
			return
		}

		start := time.Now()
		result := s.resolveLB(r.Context(), group)
		defer func() {
			slog.Debug("lb request", "group", group, "path", result.path, "cache_hit", result.cacheHit, "targets", result.targets, "reachable", result.reachable, "err", result.errStr, "duration_ms", time.Since(start).Milliseconds())
			lbRequests.WithLabelValues(result.path, strconv.FormatBool(result.cacheHit)).Inc()
			lbLatencyMs.WithLabelValues(result.path).Observe(float64(time.Since(start).Milliseconds()))
			if result.errType != "none" {
				lbErrors.WithLabelValues(result.errType).Inc()
			}
		}()

		writeLB(w, buildLBResponse(group, result.pick.Reachable, result.pick.Error, &result.pick, s.lbResponses))
	})
}

type lbResult struct {
	pick      probeResult
	path      string
	cacheHit  bool
	targets   int
	reachable int
	errStr    string
	errType   string
}

func newLBResult() lbResult {
	return lbResult{path: "unknown", errType: "none"}
}

// resolveLB performs the multi-layer LB resolution, keeping the handler thin.
func (s *Server) resolveLB(reqCtx context.Context, group string) lbResult {
	result := newLBResult()
	groupStrategy := s.selector.strategyForGroup(group)

	if cg := s.cache.lbGetFresh(group, lbCacheMaxAge); cg != nil {
		return s.resolveFromCache(group, cg)
	}

	if s.cache.redisBackoffActive(group) {
		if r := s.resolveFromConfigFallback(group, "redis_backoff"); r != nil {
			r.path = "redis_backoff"
			r.errType = "redis_backoff"
			r.pick.Error = "redis_backoff"
			return *r
		}
	}

	redisCtx, cancel := context.WithTimeout(reqCtx, redisLBTimeout)
	defer cancel()

	if s.selector.isWeighted(group) {
		return s.resolveWeighted(redisCtx, group)
	}
	if groupStrategy == "random" {
		if r := s.resolveRandom(redisCtx, group); r != nil {
			return *r
		}
	}
	if groupStrategy == "round-robin" {
		if r := s.resolveRoundRobin(redisCtx, group); r != nil {
			return *r
		}
	}

	return s.resolveViaHGETALL(redisCtx, group, result)
}

func (s *Server) resolveFromCache(group string, cg *cachedGroup) lbResult {
	pick := pickFromCacheGroup(group, s.selector, cg)
	return lbResult{
		pick:      pick,
		path:      "cache",
		cacheHit:  true,
		targets:   len(cg.hydratedAll),
		reachable: len(cg.hydratedUp),
		errType:   "none",
	}
}

func (s *Server) resolveFromConfigFallback(group string, errMsg string) *lbResult {
	cg := cacheFromConfig(group, s.selector, s.targets, s.cache, errMsg)
	if cg == nil {
		return nil
	}
	pick := pickFromCacheGroup(group, s.selector, cg)
	return &lbResult{
		pick:      pick,
		path:      "config_fallback",
		targets:   len(cg.hydratedAll),
		reachable: len(cg.hydratedUp),
		errType:   "none",
	}
}

func (s *Server) resolveWeighted(ctx context.Context, group string) lbResult {
	result := newLBResult()

	zs, err := s.loadUpScoresWithTimeout(ctx, group)
	if err != nil {
		return s.handleRedisError(group, err, "lb:redis_up_error:"+group, "redis_up_error")
	}
	if len(zs) == 0 {
		return s.handleEmptyUpSet(group)
	}

	tmap := s.targets[group]
	hydratedUp := make([]probeResult, 0, len(zs))
	for _, z := range zs {
		name := fmt.Sprint(z.Member)
		cfg, found := tmap[name]
		if !found {
			s.cache.warnEvery("lb:missing_config:"+group, 5*time.Second, "redis up target missing config", "group", group, "target", name)
			result.errType = "missing_config"
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
		if r := s.resolveFromConfigFallback(group, "missing_config"); r != nil {
			r.pick.Error = "missing_config"
			return *r
		}
		result.pick = probeResult{Error: "missing_config"}
		result.path = "missing_config"
		result.errType = "missing_config"
		return result
	}

	result.path = "redis_up_scores"
	result.targets = len(hydratedUp)
	result.reachable = len(hydratedUp)
	cg := s.cache.lbSet(group, groupData{all: nil, up: hydratedUp}, hydratedUp, hydratedUp)
	result.pick = pickFromCacheGroup(group, s.selector, cg)
	return result
}

func (s *Server) resolveRandom(ctx context.Context, group string) *lbResult {
	name, ok, err := s.loadOneUpNameWithTimeout(ctx, group)
	if err != nil {
		r := s.handleRedisError(group, err, "lb:redis_up_error:"+group, "redis_up_error")
		return &r
	}
	if !ok {
		r := s.handleEmptyUpSet(group)
		return &r
	}
	tmap := s.targets[group]
	if tmap != nil {
		if cfg, found := tmap[name]; found {
			cacheFromConfigWithReachable(group, s.selector, s.targets, s.cache, map[string]struct{}{name: {}}, "")
			pick := probeResult{Reachable: true, Target: cfg.Name, TargetMeta: targetMetaFromConfig(cfg)}
			return &lbResult{pick: pick, path: "redis_up_name", targets: 1, reachable: 1, errType: "none"}
		}
		s.cache.warnEvery("lb:missing_config:"+group, 5*time.Second, "redis up target missing config", "group", group, "target", name)
		if r := s.resolveFromConfigFallback(group, "missing_config"); r != nil {
			r.pick.Error = "missing_config"
			r.errType = "missing_config"
			return r
		}
	}
	return nil
}

func (s *Server) resolveRoundRobin(ctx context.Context, group string) *lbResult {
	zs, err := s.loadUpScoresWithTimeout(ctx, group)
	if err != nil {
		r := s.handleRedisError(group, err, "lb:redis_up_error:"+group, "redis_up_error")
		return &r
	}
	if len(zs) == 0 {
		r := s.handleEmptyUpSet(group)
		return &r
	}

	reachableNames := make(map[string]struct{}, len(zs))
	for _, z := range zs {
		reachableNames[fmt.Sprint(z.Member)] = struct{}{}
	}

	cg := cacheFromConfigWithReachable(group, s.selector, s.targets, s.cache, reachableNames, "")
	if cg == nil {
		return nil
	}
	pick := pickFromCacheGroup(group, s.selector, cg)
	return &lbResult{
		pick:      pick,
		path:      "redis_up_rr",
		targets:   len(cg.hydratedAll),
		reachable: len(cg.hydratedUp),
		errType:   "none",
	}
}

func (s *Server) resolveViaHGETALL(ctx context.Context, group string, result lbResult) lbResult {
	res, err := s.loadGroupWithTimeout(ctx, group)
	if err != nil {
		s.cache.markRedisDown(group)
		s.cache.warnEvery("lb:redis_hgetall_error:"+group, 5*time.Second, "lb read failed", "group", group, "err", err)
		result.path = "redis_hgetall_error"
		result.errStr = err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			result.errType = "redis_timeout"
		} else {
			result.errType = "redis_error"
		}
		fallback := s.cache.get(group)
		if len(fallback) == 0 {
			result.pick = probeResult{Error: "redis_error: " + err.Error()}
			return result
		}
		result.path = "cache_fallback"
		result.cacheHit = true
		result.targets = len(fallback)
		res = fallback
	} else {
		result.path = "redis_hgetall"
	}

	if len(res) == 0 {
		result.path = "empty"
		result.errType = "no_targets"
		result.pick = probeResult{Error: "no targets found"}
		return result
	}

	hydratedAll := hydrate(group, res, s.targets, s.cache)
	hydratedUp := reachableFromAll(hydratedAll)
	result.targets = len(hydratedAll)
	result.reachable = len(hydratedUp)
	cg := s.cache.lbSet(group, groupData{all: res, up: hydratedUp}, hydratedAll, hydratedUp)
	result.pick = pickFromCacheGroup(group, s.selector, cg)
	if result.path == "unknown" {
		result.path = "hydrate"
	}
	return result
}

func (s *Server) handleRedisError(group string, err error, warnKey, path string) lbResult {
	s.cache.markRedisDown(group)
	s.cache.warnEvery(warnKey, 5*time.Second, "lb read failed", "group", group, "err", err)

	result := newLBResult()
	result.path = path
	result.errStr = err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		result.errType = "redis_timeout"
	} else {
		result.errType = "redis_error"
	}

	if r := s.resolveFromConfigFallback(group, ""); r != nil {
		r.errType = result.errType
		r.errStr = result.errStr
		r.pick.Error = "redis_error: " + err.Error()
		return *r
	}
	result.pick = probeResult{Error: "redis_error: " + err.Error()}
	return result
}

func (s *Server) handleEmptyUpSet(group string) lbResult {
	s.cache.warnEvery("lb:redis_up_empty:"+group, 5*time.Second, "redis up set empty", "group", group)
	if r := s.resolveFromConfigFallback(group, "redis_up_empty"); r != nil {
		r.errType = "redis_up_empty"
		r.pick.Error = "redis_up_empty"
		return *r
	}
	return lbResult{
		pick:    probeResult{Error: "redis_up_empty"},
		path:    "redis_up_empty",
		errType: "redis_up_empty",
	}
}

func writeLB(w http.ResponseWriter, resp lbResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
