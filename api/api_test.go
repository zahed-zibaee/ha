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
	zset   map[string]map[string]float64
	getErr error
}

func decodeLB(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func lbString(resp map[string]any, key string) string {
	if v, ok := resp[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func lbBool(resp map[string]any, key string) bool {
	if v, ok := resp[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
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
		zset: map[string]map[string]float64{"hc:g:up": {"up": 10}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"up":   {Name: "up", URL: "https://up"},
			"down": {Name: "down", URL: "https://down"},
		},
	}

	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	selector := newSelector("random", index, nil)
	lbHandler(rdb, selector, index, nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	if lbString(resp, "name") != "up" || lbString(resp, "url") != "https://up" || !lbBool(resp, "reachable") {
		t.Fatalf("unexpected resp: %#v", resp)
	}
}

func TestLBResponseOverrideReplacesTargetData(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"t1": 5}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"t1": {Name: "t1", URL: "https://original"},
		},
	}
	overrides := map[string]map[string]map[string]any{
		"g": {
			"t1": {
				"url":         "https://override",
				"bucket":      "b1",
				"description": "custom",
			},
		},
	}

	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	selector := newSelector("random", index, nil)
	lbHandler(rdb, selector, index, overrides).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	if lbString(resp, "name") != "t1" {
		t.Fatalf("expected name t1, got %#v", resp)
	}
	if lbString(resp, "url") != "https://override" {
		t.Fatalf("expected override url, got %#v", resp)
	}
	if lbString(resp, "bucket") != "b1" || lbString(resp, "description") != "custom" {
		t.Fatalf("expected override fields, got %#v", resp)
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
	selector := newSelector("random", index, nil)
	lbHandler(rdb, selector, index, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	if lbString(resp, "name") != "ok" || lbString(resp, "url") != "https://ok" || !lbBool(resp, "reachable") {
		t.Fatalf("unexpected resp: %#v", resp)
	}
}

func TestLBConfigFallbackRoundRobin(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"b": {Name: "b", URL: "https://b"},
			"a": {Name: "a", URL: "https://a"},
		},
	}
	selector := newSelector("round-robin", index, nil)
	handler := lbHandler(rdb, selector, index, nil)

	req1 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("status %d", rr1.Code)
	}
	resp1 := decodeLB(t, rr1)
	if lbString(resp1, "name") != "a" {
		t.Fatalf("expected first target a, got %#v", resp1)
	}

	req2 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status %d", rr2.Code)
	}
	resp2 := decodeLB(t, rr2)
	if lbString(resp2, "name") != "b" {
		t.Fatalf("expected second target b, got %#v", resp2)
	}
}

func TestLBConfigFallbackMissingConfig(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"ghost": 1}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	selector := newSelector("random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	name := lbString(resp, "name")
	if name != "a" && name != "b" {
		t.Fatalf("unexpected target: %#v", resp)
	}
	if lbBool(resp, "reachable") {
		t.Fatalf("expected reachable=false, got %#v", resp)
	}
	if lbString(resp, "error") != "missing_config" {
		t.Fatalf("expected missing_config error, got %#v", resp)
	}
}

func TestLBConfigFallbackRedisUpEmpty(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	selector := newSelector("random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	name := lbString(resp, "name")
	if name != "a" && name != "b" {
		t.Fatalf("unexpected target: %#v", resp)
	}
	if lbBool(resp, "reachable") {
		t.Fatalf("expected reachable=false, got %#v", resp)
	}
	if lbString(resp, "error") != "redis_up_empty" {
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
	selector := newSelector("random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	name := lbString(resp, "name")
	if name != "a" && name != "b" {
		t.Fatalf("unexpected target: %#v", resp)
	}
	if lbBool(resp, "reachable") {
		t.Fatalf("expected reachable=false, got %#v", resp)
	}
	if !strings.Contains(lbString(resp, "error"), "redis_error: boom") {
		t.Fatalf("expected redis_error, got %#v", resp)
	}
}

func TestLBRedisUpSeedsCache(t *testing.T) {
	clearCaches()
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"a": 3}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	selector := newSelector("random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	lbHandler(rdb, selector, index, nil).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if cg := lbCacheGetFresh("g", lbCacheMaxAge); cg == nil || len(cg.hydratedUp) != 1 || cg.hydratedUp[0].Target != "a" {
		t.Fatalf("expected cache seeded with a, got %#v", cg)
	}

	rdbErr := &fakeRedis{getErr: assertErr("boom")}
	req2 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr2 := httptest.NewRecorder()
	lbHandler(rdbErr, selector, index, nil).ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status %d", rr2.Code)
	}
	resp := decodeLB(t, rr2)
	if lbString(resp, "name") != "a" || !lbBool(resp, "reachable") {
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

func (f *fakeRedis) ZRangeWithScores(ctx context.Context, key string, start, stop int64) *redis.ZSliceCmd {
	cmd := redis.NewZSliceCmd(ctx)
	if f.getErr != nil {
		cmd.SetErr(f.getErr)
		return cmd
	}
	var out []redis.Z
	if f.zset != nil {
		if zs, ok := f.zset[key]; ok {
			for member, score := range zs {
				out = append(out, redis.Z{Member: member, Score: score})
			}
		}
	} else if f.set != nil {
		if ss, ok := f.set[key]; ok {
			for _, member := range ss {
				out = append(out, redis.Z{Member: member, Score: 1})
			}
		}
	}
	cmd.SetVal(out)
	return cmd
}
