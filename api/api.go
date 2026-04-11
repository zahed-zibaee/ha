package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
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

// LeaderStatus captures Redis lock and local probe-runner state for the node.
// When Redis is unreachable, Leader is false on all nodes but ProbesActive can be true
// on every replica so target health checks still run (writes to Redis will fail until it returns).
type LeaderStatus struct {
	Leader       bool
	ProbesActive bool
	NodeID       string
	Since        time.Time
}

// Server holds all state needed by the API handlers.
type Server struct {
	rdb         RedisClient
	selector    *lbSelector
	targets     map[string]map[string]config.Target
	lbResponses map[string]map[string]map[string]any
	leaderFn    func() LeaderStatus
	cache       *cacheState
}

// NewServer creates a Server with all dependencies.
func NewServer(rdb RedisClient, lbStrategy string, targets map[string]map[string]config.Target, lbResponses map[string]map[string]map[string]any, lbTypes map[string]string, leaderFn func() LeaderStatus) *Server {
	return &Server{
		rdb:         rdb,
		selector:    newSelector(lbStrategy, targets, lbTypes),
		targets:     targets,
		lbResponses: lbResponses,
		leaderFn:    leaderFn,
		cache:       newCacheState(),
	}
}

// Start launches the HTTP server. Blocks until context is canceled or a fatal error occurs.
func (s *Server) Start(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/v1/check/", s.checkHandler())
	mux.Handle("/v1/lb/", s.lbHandler())
	mux.Handle("/v1/leader", s.leaderHandler())
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
	Leader       bool   `json:"leader"`
	ProbesActive bool   `json:"probes_active"`
	Status       string `json:"status"`
	NodeID       string `json:"node_id,omitempty"`
	SinceUnix    int64  `json:"since_unix,omitempty"`
}

// Middleware.

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
		if r.URL.Path == "/health" {
			return
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", status, "bytes", rec.bytes, "duration_ms", time.Since(start).Milliseconds())
	})
}

// Data loading methods on Server.

func (s *Server) loadGroup(ctx context.Context, group string) ([]probeResult, error) {
	select {
	case redisSem <- struct{}{}:
		defer func() { <-redisSem }()
	default:
		slog.Debug("redis concurrency high; proceeding without waiting", "group", group)
	}
	hashKey := "hc:" + group
	m, err := s.rdb.HGetAll(ctx, hashKey).Result()
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
	s.cache.set(group, results)
	return results, nil
}

func (s *Server) loadUpScores(ctx context.Context, group string) ([]redis.Z, error) {
	select {
	case redisSem <- struct{}{}:
		defer func() { <-redisSem }()
	default:
		slog.Debug("redis concurrency high; proceeding without waiting", "group", group)
	}
	upKey := "hc:" + group + ":up"
	return s.rdb.ZRangeWithScores(ctx, upKey, 0, -1).Result()
}

func (s *Server) loadOneUpName(ctx context.Context, group string) (string, bool, error) {
	zs, err := s.loadUpScores(ctx, group)
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

func (s *Server) loadUpScoresWithTimeout(ctx context.Context, group string) ([]redis.Z, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		zs  []redis.Z
		err error
	}
	ch := make(chan result, 1)
	go func() {
		zs, err := s.loadUpScores(ctx, group)
		ch <- result{zs: zs, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-ch:
		return out.zs, out.err
	}
}

func (s *Server) loadGroupWithTimeout(ctx context.Context, group string) ([]probeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		res []probeResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		res, err := s.loadGroup(ctx, group)
		ch <- result{res: res, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-ch:
		return out.res, out.err
	}
}

func (s *Server) loadOneUpNameWithTimeout(ctx context.Context, group string) (string, bool, error) {
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
		name, ok, err := s.loadOneUpName(ctx, group)
		ch <- result{name: name, ok: ok, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case out := <-ch:
		return out.name, out.ok, out.err
	}
}
