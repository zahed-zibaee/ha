package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/raft"
	"github.com/redis/go-redis/v9"
	"ha/api"
	"ha/checks"
	"ha/config"
	"ha/logger"
	"ha/raftnode"
	"ha/redisstore"
)

func main() {
	logLevel := getenvDefault("LOG_LEVEL", "warn")
	logFormat := getenvDefault("LOG_FORMAT", "json")
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

	raftOpts := raftnode.OptionsFromEnv()
	r, obsCh, hasState, advAddr, err := raftnode.Start(raftOpts)
	if err != nil {
		slog.Error("raft start failed", "err", err)
		os.Exit(1)
	}
	leaderCh := r.LeaderCh()
	go logObservations(obsCh)
	leaderTracker := newLeaderTracker(r, raftOpts.NodeID)

	redisOpts := redisstore.FromEnv()
	rawRedis := redisstore.NewClient(redisOpts)
	if err := redisstore.Ping(context.Background(), rawRedis); err != nil {
		slog.Warn("redis ping failed", "err", err, "addr", redisOpts.Addr)
	} else {
		slog.Info("redis connected", "addr", redisOpts.Addr)
	}

	logTargets(cfg)
	go manageChecks(ctx, r, leaderCh, cfg, rawRedis, leaderTracker)
	joinFn := func(id, addr string) (int, string, error) {
		if r.State() != raft.Leader {
			return http.StatusConflict, "", nil
		}
		f := r.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), 0, 0)
		if err := f.Error(); err != nil {
			if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
				return http.StatusConflict, "", err
			}
			return http.StatusInternalServerError, "", err
		}
		return http.StatusOK, "", nil
	}
	go func() {
		listen := getenvDefault("LISTEN_ADDR", ":8080")
		lbStrategy := getenvDefault("LB_STRATEGY", "random")
		if err := api.Start(ctx, listen, rawRedis, lbStrategy, targetIndex, leaderTracker.snapshot, joinFn); err != nil {
			slog.Error("http server exited", "err", err)
			cancelAndExit(stop)
		}
	}()

	if !hasState {
		if raftOpts.Bootstrap {
			if err := bootstrapSingle(r, raftOpts.NodeID, advAddr); err != nil {
				slog.Error("raft bootstrap failed", "err", err)
				cancelAndExit(stop)
			} else {
				slog.Info("raft bootstrap complete", "node", raftOpts.NodeID)
			}
		} else {
			go joinLoop(ctx, raftOpts.JoinAddrs, raftOpts.NodeID, advAddr, raftOpts.JoinTimeout)
		}
	}

	slog.Info("ha starting (skeleton)", "check_groups", len(cfg.Checks), "raft_node", raftOpts.NodeID, "raft_bind", raftOpts.BindAddr, "log_level", logLevel, "log_format", logFormat)
	<-ctx.Done()
	slog.Info("shutdown signal received")
}

func logObservations(ch <-chan raft.Observation) {
	for o := range ch {
		switch v := o.Data.(type) {
		case raft.LeaderObservation:
			slog.Debug("raft leader observed", "leader", v.LeaderID)
		case raft.RequestVoteRequest:
			slog.Debug("raft vote request", "candidate", v.Candidate, "term", v.Term)
		}
	}
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func startChecks(ctx context.Context, cfg *config.Config, rdb *redis.Client) {
	// only HTTP checks wired for now
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
		checks.StartHTTPGroup(ctx, name, chk.Targets, rdb)
		started++
	}
	if started == 0 {
		slog.Warn("no runnable check groups (only http supported now); probes not started")
	}
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

func cancelAndExit(stop context.CancelFunc) {
	stop()
	os.Exit(1)
}

func tryJoinCluster(ctx context.Context, addrs []string, nodeID, raftAddr string, timeout time.Duration) bool {
	if len(addrs) == 0 {
		return false
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	body := map[string]string{"id": nodeID, "addr": raftAddr}
	payload, _ := json.Marshal(body)
	for time.Now().Before(deadline) {
		for _, addr := range addrs {
			url := strings.TrimRight(addr, "/") + "/v1/raft/join"
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				slog.Info("raft join succeeded", "node", nodeID, "addr", raftAddr, "via", addr)
				return true
			}
		}
		time.Sleep(1 * time.Second)
	}
	slog.Warn("raft join failed", "node", nodeID, "addr", raftAddr, "timeout", timeout)
	return false
}

func joinLoop(ctx context.Context, addrs []string, nodeID, raftAddr string, timeout time.Duration) {
	if len(addrs) == 0 {
		slog.Warn("raft join disabled; no join addrs configured", "node", nodeID)
		return
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	nextLog := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		if tryJoinCluster(ctx, addrs, nodeID, raftAddr, timeout) {
			return
		}
		if time.Now().After(nextLog) {
			slog.Warn("raft join failed; will retry", "node", nodeID, "addr", raftAddr, "timeout", timeout)
			nextLog = time.Now().Add(10 * time.Second)
		}
		time.Sleep(2 * time.Second)
	}
}

func bootstrapSingle(r *raft.Raft, nodeID, addr string) error {
	f := r.BootstrapCluster(raft.Configuration{
		Servers: []raft.Server{
			{ID: raft.ServerID(nodeID), Address: raft.ServerAddress(addr)},
		},
	})
	return f.Error()
}

type leaderTracker struct {
	mu     sync.RWMutex
	status api.LeaderStatus
}

func newLeaderTracker(r *raft.Raft, nodeID string) *leaderTracker {
	state := r.State()
	return &leaderTracker{
		status: api.LeaderStatus{
			Leader: state == raft.Leader,
			State:  state.String(),
			NodeID: nodeID,
			Since:  time.Now(),
		},
	}
}

func (t *leaderTracker) update(r *raft.Raft, isLeader bool) {
	t.mu.Lock()
	t.status.Leader = isLeader
	t.status.State = r.State().String()
	t.status.Since = time.Now()
	t.mu.Unlock()
}

func (t *leaderTracker) snapshot() api.LeaderStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status
}

// manageChecks starts/stops probes based on raft leadership.
func manageChecks(parent context.Context, r *raft.Raft, leaderCh <-chan bool, cfg *config.Config, rdb *redis.Client, tracker *leaderTracker) {
	var cancel context.CancelFunc
	if tracker != nil {
		tracker.update(r, r.State() == raft.Leader)
	}
	// handle current state
	if r.State() == raft.Leader {
		leaderCtx, c := context.WithCancel(parent)
		cancel = c
		slog.Info("starting checks as current leader")
		startChecks(leaderCtx, cfg, rdb)
	}
	for isLeader := range leaderCh {
		if tracker != nil {
			tracker.update(r, isLeader)
		}
		if isLeader {
			if cancel != nil {
				cancel()
			}
			leaderCtx, c := context.WithCancel(parent)
			cancel = c
			slog.Info("became leader; starting checks")
			startChecks(leaderCtx, cfg, rdb)
		} else {
			if cancel != nil {
				slog.Warn("lost leadership; stopping checks")
				cancel()
				cancel = nil
			}
		}
	}
}
