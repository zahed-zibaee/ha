package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"ha/config"
)

type fakeRedis struct {
	hash   map[string]map[string]string
	set    map[string][]string
	getErr error
}

func TestLoadGroupReturnsResults(t *testing.T) {
	val := probeResult{Reachable: true, Target: "t1"}
	b, _ := json.Marshal(val)
	rdb := &fakeRedis{hash: map[string]map[string]string{"hc:g": {"t1": string(b)}}}
	ctx := context.Background()
	results, err := loadGroup(ctx, rdb, "g")
	if err != nil {
		t.Fatalf("loadGroup error: %v", err)
	}
	if len(results) != 1 || results[0].Target != "t1" {
		t.Fatalf("unexpected results: %#v", results)
	}
	index := map[string]map[string]config.Target{
		"g": {"t1": {Name: "t1", URL: "https://example.com/health"}},
	}
	h := hydrate("g", results, index)
	if h[0].TargetMeta.URL != "https://example.com/health" {
		t.Fatalf("hydrate failed: %#v", h[0])
	}
}

func TestLBHandlerChoosesReachable(t *testing.T) {
	rdb := &fakeRedis{
		set: map[string][]string{"hc:g:up": {"up"}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"up":   {Name: "up", URL: "https://up"},
			"down": {Name: "down", URL: "https://down"},
		},
	}

	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	selector := newSelector("random", index)
	lbHandler(rdb, selector, index).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp lbResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target.Name != "up" || resp.Target.URL != "https://up" || !resp.Reachable {
		t.Fatalf("unexpected resp: %#v", resp)
	}
}

func TestCheckHandlerOnRedisError(t *testing.T) {
	rdb := &fakeRedis{getErr: assertErr("boom")}
	req := httptest.NewRequest("GET", "/v1/check/g", nil)
	rr := httptest.NewRecorder()
	checkHandler(rdb, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "boom") {
		t.Fatalf("expected error message, got %s", rr.Body.String())
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/health", nil)
	rr := httptest.NewRecorder()
	healthHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestLBUsesCacheOnRedisError(t *testing.T) {
	clearCaches()
	// seed lb cache with hydrated data
	data := groupData{
		all: []probeResult{{Reachable: true, Target: "ok"}},
		up:  []probeResult{{Reachable: true, Target: "ok"}},
	}
	hydratedAll := []probeResult{{
		Reachable: true,
		Target:    "ok",
		TargetMeta: targetMeta{
			Name: "ok",
			URL:  "https://ok",
		},
	}}
	hydratedUp := hydratedAll
	lbCacheSet("g", data, hydratedAll, hydratedUp)

	// redis will error
	rdb := &fakeRedis{getErr: assertErr("boom")}
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	index := map[string]map[string]config.Target{
		"g": {"ok": {Name: "ok", URL: "https://ok"}},
	}
	selector := newSelector("random", index)
	lbHandler(rdb, selector, index).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp lbResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target.Name != "ok" || resp.Target.URL != "https://ok" || !resp.Reachable {
		t.Fatalf("unexpected resp: %#v", resp)
	}
}

func TestLBConfigFallbackRoundRobin(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		set: map[string][]string{"hc:g:up": {}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"b": {Name: "b", URL: "https://b"},
			"a": {Name: "a", URL: "https://a"},
		},
	}
	selector := newSelector("round-robin", index)
	handler := lbHandler(rdb, selector, index)

	req1 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("status %d", rr1.Code)
	}
	var resp1 lbResponse
	if err := json.NewDecoder(rr1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp1.Target.Name != "a" {
		t.Fatalf("expected first target a, got %#v", resp1)
	}

	req2 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status %d", rr2.Code)
	}
	var resp2 lbResponse
	if err := json.NewDecoder(rr2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Target.Name != "b" {
		t.Fatalf("expected second target b, got %#v", resp2)
	}
}

func TestLBConfigFallbackMissingConfig(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		set: map[string][]string{"hc:g:up": {"ghost"}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	selector := newSelector("random", index)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp lbResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target.Name != "a" && resp.Target.Name != "b" {
		t.Fatalf("unexpected target: %#v", resp)
	}
	if resp.Reachable {
		t.Fatalf("expected reachable=false, got %#v", resp)
	}
	if resp.Error != "missing_config" {
		t.Fatalf("expected missing_config error, got %#v", resp)
	}
}

func TestLBConfigFallbackRedisUpEmpty(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		set: map[string][]string{"hc:g:up": {}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	selector := newSelector("random", index)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp lbResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target.Name != "a" && resp.Target.Name != "b" {
		t.Fatalf("unexpected target: %#v", resp)
	}
	if resp.Reachable {
		t.Fatalf("expected reachable=false, got %#v", resp)
	}
	if resp.Error != "redis_up_empty" {
		t.Fatalf("expected redis_up_empty error, got %#v", resp)
	}
}

func TestLBConfigFallbackRedisUpError(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{getErr: assertErr("boom")}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	selector := newSelector("random", index)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp lbResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target.Name != "a" && resp.Target.Name != "b" {
		t.Fatalf("unexpected target: %#v", resp)
	}
	if resp.Reachable {
		t.Fatalf("expected reachable=false, got %#v", resp)
	}
	if !strings.Contains(resp.Error, "redis_error: boom") {
		t.Fatalf("expected redis_error, got %#v", resp)
	}
}

func TestLBRedisUpSeedsCache(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		set: map[string][]string{"hc:g:up": {"a"}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	selector := newSelector("random", index)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if cg := lbCacheGetFresh("g", lbCacheMaxAge); cg == nil || len(cg.hydratedUp) != 1 || cg.hydratedUp[0].Target != "a" {
		t.Fatalf("expected cache seeded with a, got %#v", cg)
	}

	rdbErr := &fakeRedis{getErr: assertErr("boom")}
	req2 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr2 := httptest.NewRecorder()
	lbHandler(rdbErr, selector, index).ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status %d", rr2.Code)
	}
	var resp lbResponse
	if err := json.NewDecoder(rr2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Target.Name != "a" || !resp.Reachable {
		t.Fatalf("expected cached a reachable, got %#v", resp)
	}
}

func clearCaches() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	lastCache = map[string][]probeResult{}
	lbCache = map[string]cachedGroup{}
	clearRedisBackoff()
}

func clearRedisBackoff() {
	redisBackoffMu.Lock()
	defer redisBackoffMu.Unlock()
	redisBackoffUntil = map[string]time.Time{}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
func (f *fakeRedis) HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd {
	cmd := redis.NewMapStringStringCmd(ctx)
	if f.getErr != nil {
		cmd.SetErr(f.getErr)
		return cmd
	}
	out := map[string]string{}
	if f.hash != nil {
		if h, ok := f.hash[key]; ok {
			for k, v := range h {
				out[k] = v
			}
		}
	}
	cmd.SetVal(out)
	return cmd
}

func (f *fakeRedis) HMGet(ctx context.Context, key string, fields ...string) *redis.SliceCmd {
	cmd := redis.NewSliceCmd(ctx)
	if f.getErr != nil {
		cmd.SetErr(f.getErr)
		return cmd
	}
	var out []interface{}
	h := map[string]string{}
	if f.hash != nil {
		h = f.hash[key]
	}
	for _, fld := range fields {
		if v, ok := h[fld]; ok {
			out = append(out, v)
		} else {
			out = append(out, nil)
		}
	}
	cmd.SetVal(out)
	return cmd
}

func (f *fakeRedis) SRandMemberN(ctx context.Context, key string, count int64) *redis.StringSliceCmd {
	cmd := redis.NewStringSliceCmd(ctx)
	if f.getErr != nil {
		cmd.SetErr(f.getErr)
		return cmd
	}
	res := []string{}
	if f.set != nil {
		res = append(res, f.set[key]...)
	}
	if len(res) > int(count) {
		res = res[:count]
	}
	cmd.SetVal(res)
	return cmd
}
