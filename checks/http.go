package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"ha/config"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"
)

// StartHTTPGroup launches one goroutine per HTTP target. Each goroutine stops when ctx is canceled.
func StartHTTPGroup(ctx context.Context, group string, targets []config.Target, rdb RedisSet) {
	tracker := newGroupTracker(group, targets)
	for _, t := range targets {
		// Copy loop variable
		target := t
		go runHTTPLoop(ctx, group, target, rdb, tracker)
	}
}

func runHTTPLoop(ctx context.Context, group string, target config.Target, rdb RedisSet, tracker *groupTracker) {
	jitter := jitterDuration(target.Interval, target.JitterPct)
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			start := time.Now()
			slog.Debug("http probe start", "group", group, "target", target.Name, "url", target.URL)
			result := runHTTPProbe(ctx, target)
			slog.Debug("http probe result", "group", group, "target", target.Name, "reachable", result.Reachable, "status", result.Status, "latency_ms", result.LatencyMs, "err", result.Error)
			recordProbe("http")
			writeStart := time.Now()
			if err := writeResult(ctx, rdb, group, target.Name, result, target.RedisTTL); err != nil {
				recordProbeWriteError("http")
				slog.Error("http probe write failed", "group", group, "target", target.Name, "err", err)
			} else {
				slog.Debug("http probe", "group", group, "target", target.Name, "reachable", result.Reachable, "status", result.Status, "latency_ms", result.LatencyMs, "write_ms", time.Since(writeStart).Milliseconds(), "loop_ms", time.Since(start).Milliseconds())
			}
			tracker.update(target.Name, result)
			// next tick with jitter
			timer.Reset(target.Interval + jitterDuration(target.Interval, target.JitterPct))
		}
	}
}

type probeResult struct {
	Reachable bool   `json:"reachable"`
	Status    string `json:"status"`
	CheckedAt int64  `json:"checked_at"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	Type      string `json:"type"`
	Target    string `json:"target"` // target name (field)
}

// RedisSet defines the subset of redis methods needed for writing results.
type RedisSet interface {
	HSet(ctx context.Context, key string, values ...interface{}) *redis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	SAdd(ctx context.Context, key string, members ...interface{}) *redis.IntCmd
	SRem(ctx context.Context, key string, members ...interface{}) *redis.IntCmd
}

func runHTTPProbe(parent context.Context, target config.Target) probeResult {
	start := time.Now()
	err := tryHTTP(parent, target)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return probeResult{
			Reachable: false,
			Status:    "down",
			CheckedAt: time.Now().Unix(),
			LatencyMs: latency,
			Error:     err.Error(),
			Type:      "http",
			Target:    target.Name,
		}
	}
	return probeResult{
		Reachable: true,
		Status:    "up",
		CheckedAt: time.Now().Unix(),
		LatencyMs: latency,
		Type:      "http",
		Target:    target.Name,
	}
}

func tryHTTP(parent context.Context, target config.Target) error {
	if target.URL == "" {
		return errors.New("missing url")
	}
	method := strings.ToUpper(target.Method)
	if method == "" {
		method = http.MethodGet
	}
	followRedirects := true
	if target.FollowRedirects != nil {
		followRedirects = *target.FollowRedirects
	}
	maxRedirects := target.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 10
	}
	attempts := target.Retry + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		ctx, cancel := context.WithTimeout(parent, target.Timeout)
		req, err := http.NewRequestWithContext(ctx, method, target.URL, nil)
		if err != nil {
			cancel()
			return err
		}
		for k, v := range target.Headers {
			req.Header.Set(k, v)
		}
		if target.AuthBearer != "" {
			req.Header.Set("Authorization", "Bearer "+target.AuthBearer)
		} else if target.AuthBasicUser != "" || target.AuthBasicPass != "" {
			req.SetBasicAuth(target.AuthBasicUser, target.AuthBasicPass)
		}
		client := &http.Client{}
		if !followRedirects {
			client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			}
		} else if maxRedirects > 0 {
			client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				if len(via) > 0 {
					req.Header = via[0].Header.Clone()
				}
				return nil
			}
		}
		resp, err := client.Do(req)
		if err == nil {
			ok := statusAllowed(resp.StatusCode, target.ExpectStatus)
			resp.Body.Close()
			cancel()
			if ok {
				return nil
			}
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		} else {
			lastErr = err
			cancel()
		}
		if i < attempts-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return lastErr
}

func statusAllowed(code int, allowed []int) bool {
	for _, c := range allowed {
		if c == code {
			return true
		}
	}
	return false
}

func writeResult(ctx context.Context, rdb RedisSet, group, name string, res probeResult, ttl time.Duration) error {
	hashKey := fmt.Sprintf("hc:%s", group)
	upKey := fmt.Sprintf("hc:%s:up", group)
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	// small jitter to coalesce writes
	time.Sleep(time.Duration(rand.Intn(30)) * time.Millisecond)
	backoff := 50 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		if err := rdb.HSet(ctx, hashKey, name, b).Err(); err != nil {
			slog.Debug("redis hset failed", "group", group, "target", name, "err", err, "attempt", attempt)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		if res.Reachable {
			_ = rdb.SAdd(ctx, upKey, name).Err()
		} else {
			_ = rdb.SRem(ctx, upKey, name).Err()
		}
		if err := rdb.Expire(ctx, hashKey, ttl).Err(); err != nil {
			slog.Debug("redis expire failed", "group", group, "target", name, "err", err, "attempt", attempt)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		if err := rdb.Expire(ctx, upKey, ttl).Err(); err != nil {
			slog.Debug("redis expire failed (up set)", "group", group, "target", name, "err", err, "attempt", attempt)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		return nil
	}
	return fmt.Errorf("redis write failed after retries")
}

func jitterDuration(interval time.Duration, pct int) time.Duration {
	if pct <= 0 {
		return 0
	}
	f := float64(interval) * (float64(pct) / 100.0)
	if f <= 0 {
		return 0
	}
	return time.Duration(rand.Float64() * f)
}

type groupTracker struct {
	group    string
	statuses map[string]probeResult
	mu       sync.Mutex
	lastWarn bool
}

func newGroupTracker(group string, targets []config.Target) *groupTracker {
	st := make(map[string]probeResult, len(targets))
	for _, t := range targets {
		st[t.Name] = probeResult{Reachable: false}
	}
	return &groupTracker{group: group, statuses: st}
}

func (g *groupTracker) update(name string, res probeResult) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.statuses[name] = res

	total := len(g.statuses)
	up := 0
	for _, pr := range g.statuses {
		if pr.Reachable {
			up++
		}
	}

	if up == 0 && !g.lastWarn {
		slog.Warn("all targets unreachable", "group", g.group, "total", total, "last_target", name, "last_err", res.Error)
		g.lastWarn = true
	}
	if up > 0 && g.lastWarn {
		slog.Info("group recovered", "group", g.group, "reachable", up, "total", total)
		g.lastWarn = false
	}
}
