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

func newTestServer(rdb RedisClient, strategy string, index map[string]map[string]config.Target, overrides map[string]map[string]map[string]any) *Server {
	return NewServer(rdb, strategy, index, overrides, nil, nil)
}

func newTestServerWithLBTypes(rdb RedisClient, strategy string, index map[string]map[string]config.Target, overrides map[string]map[string]map[string]any, lbTypes map[string]string) *Server {
	return NewServer(rdb, strategy, index, overrides, lbTypes, nil)
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

// --- loadGroup / hydrate tests ---

func TestLoadGroupReturnsResults(t *testing.T) {
	val := probeResult{Reachable: true, Target: "t1"}
	b, _ := json.Marshal(val)
	rdb := &fakeRedis{hash: map[string]map[string]string{"hc:g": {"t1": string(b)}}}
	srv := newTestServer(rdb, "random", nil, nil)
	ctx := context.Background()
	results, err := srv.loadGroup(ctx, "g")
	if err != nil {
		t.Fatalf("loadGroup error: %v", err)
	}
	if len(results) != 1 || results[0].Target != "t1" {
		t.Fatalf("unexpected results: %#v", results)
	}
	index := map[string]map[string]config.Target{
		"g": {"t1": {Name: "t1", URL: "https://example.com/health"}},
	}
	h := hydrate("g", results, index, srv.cache)
	if h[0].TargetMeta.URL != "https://example.com/health" {
		t.Fatalf("hydrate failed: %#v", h[0])
	}
}

func TestLoadGroupMultipleTargets(t *testing.T) {
	v1 := probeResult{Reachable: true, Target: "a"}
	v2 := probeResult{Reachable: false, Target: "b", Error: "timeout"}
	b1, _ := json.Marshal(v1)
	b2, _ := json.Marshal(v2)
	rdb := &fakeRedis{hash: map[string]map[string]string{"hc:g": {"a": string(b1), "b": string(b2)}}}
	srv := newTestServer(rdb, "random", nil, nil)
	results, err := srv.loadGroup(context.Background(), "g")
	if err != nil {
		t.Fatalf("loadGroup error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestLoadGroupCachesResults(t *testing.T) {
	val := probeResult{Reachable: true, Target: "t1"}
	b, _ := json.Marshal(val)
	rdb := &fakeRedis{hash: map[string]map[string]string{"hc:g": {"t1": string(b)}}}
	srv := newTestServer(rdb, "random", nil, nil)
	_, _ = srv.loadGroup(context.Background(), "g")
	cached := srv.cache.get("g")
	if len(cached) != 1 || cached[0].Target != "t1" {
		t.Fatalf("expected cache to be populated, got %#v", cached)
	}
}

func TestLoadGroupEmptyHash(t *testing.T) {
	rdb := &fakeRedis{hash: map[string]map[string]string{"hc:g": {}}}
	srv := newTestServer(rdb, "random", nil, nil)
	results, err := srv.loadGroup(context.Background(), "g")
	if err != nil {
		t.Fatalf("loadGroup error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty results, got %d", len(results))
	}
}

func TestLoadGroupRedisError(t *testing.T) {
	rdb := &fakeRedis{getErr: assertErr("down")}
	srv := newTestServer(rdb, "random", nil, nil)
	_, err := srv.loadGroup(context.Background(), "g")
	if err == nil {
		t.Fatal("expected error from loadGroup")
	}
}

// --- LB handler tests ---

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
	srv := newTestServer(rdb, "random", index, nil)
	srv.lbHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	if lbString(resp, "name") != "up" || lbString(resp, "url") != "https://up" || !lbBool(resp, "reachable") {
		t.Fatalf("unexpected resp: %#v", resp)
	}
}

func TestLBHandlerEmptyGroupReturns400(t *testing.T) {
	srv := newTestServer(&fakeRedis{}, "random", nil, nil)
	req := httptest.NewRequest("GET", "/v1/lb/", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestLBResponseOverrideReplacesTargetData(t *testing.T) {
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
	srv := newTestServer(rdb, "random", index, overrides)
	srv.lbHandler().ServeHTTP(rr, req)

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

func TestLBResponseOverrideDoesNotOverwriteReservedKeys(t *testing.T) {
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"t1": 5}},
	}
	index := map[string]map[string]config.Target{
		"g": {"t1": {Name: "t1", URL: "https://t1"}},
	}
	overrides := map[string]map[string]map[string]any{
		"g": {"t1": {"group": "hacked", "reachable": false, "error": "injected", "custom": "yes"}},
	}
	srv := newTestServer(rdb, "random", index, overrides)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
	resp := decodeLB(t, rr)
	if lbString(resp, "group") != "g" {
		t.Fatalf("group should not be overridden, got %#v", resp)
	}
	if !lbBool(resp, "reachable") {
		t.Fatalf("reachable should not be overridden, got %#v", resp)
	}
	if lbString(resp, "custom") != "yes" {
		t.Fatalf("custom field missing, got %#v", resp)
	}
}

func TestLBUsesCacheOnRedisError(t *testing.T) {
	index := map[string]map[string]config.Target{
		"g": {"ok": {Name: "ok", URL: "https://ok"}},
	}
	srv := newTestServer(&fakeRedis{getErr: assertErr("boom")}, "random", index, nil)

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
	srv.cache.lbSet("g", data, hydratedAll, hydratedUp)

	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	if lbString(resp, "name") != "ok" || lbString(resp, "url") != "https://ok" || !lbBool(resp, "reachable") {
		t.Fatalf("unexpected resp: %#v", resp)
	}
}

func TestLBConfigFallbackRoundRobin(t *testing.T) {
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"b": {Name: "b", URL: "https://b"},
			"a": {Name: "a", URL: "https://a"},
		},
	}
	srv := newTestServer(rdb, "round-robin", index, nil)
	handler := srv.lbHandler()

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

func TestLBRoundRobinWrapsAround(t *testing.T) {
	rdb := &fakeRedis{zset: map[string]map[string]float64{"hc:g:up": {}}}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	srv := newTestServer(rdb, "round-robin", index, nil)
	handler := srv.lbHandler()

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest("GET", "/v1/lb/g", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		resp := decodeLB(t, rr)
		seen[lbString(resp, "name")]++
	}
	if seen["a"] != 2 || seen["b"] != 2 {
		t.Fatalf("expected even distribution, got %v", seen)
	}
}

func TestLBConfigFallbackMissingConfig(t *testing.T) {
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"ghost": 1}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	srv := newTestServer(rdb, "random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
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
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	srv := newTestServer(rdb, "random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
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
	rdb := &fakeRedis{getErr: assertErr("boom")}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	srv := newTestServer(rdb, "random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
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
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"a": 3}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	srv := newTestServer(rdb, "random", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if cg := srv.cache.lbGetFresh("g", lbCacheMaxAge); cg == nil || len(cg.hydratedUp) != 1 || cg.hydratedUp[0].Target != "a" {
		t.Fatalf("expected cache seeded with a, got %#v", cg)
	}

	srv.rdb = &fakeRedis{getErr: assertErr("boom")}
	req2 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr2 := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status %d", rr2.Code)
	}
	resp := decodeLB(t, rr2)
	if lbString(resp, "name") != "a" || !lbBool(resp, "reachable") {
		t.Fatalf("expected cached a reachable, got %#v", resp)
	}
}

func TestLBWeightedSelectsFromZSET(t *testing.T) {
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"fast": 10, "slow": 900}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"fast": {Name: "fast", URL: "https://fast"},
			"slow": {Name: "slow", URL: "https://slow"},
		},
	}
	srv := newTestServer(rdb, "weighted", index, nil)
	seen := map[string]int{}
	handler := srv.lbHandler()
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("GET", "/v1/lb/g", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		resp := decodeLB(t, rr)
		seen[lbString(resp, "name")]++
	}
	if seen["fast"] == 0 || seen["slow"] == 0 {
		t.Fatalf("expected both targets served, got %v", seen)
	}
	if seen["fast"] <= seen["slow"] {
		t.Fatalf("expected fast to be picked more often (lower latency), got fast=%d slow=%d", seen["fast"], seen["slow"])
	}
}

func TestLBWeightedPartialMissingConfigDoesNotDegrade(t *testing.T) {
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"fast": 10, "ghost": 900}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"fast": {Name: "fast", URL: "https://fast"},
		},
	}
	srv := newTestServer(rdb, "weighted", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	if lbString(resp, "name") != "fast" {
		t.Fatalf("expected fast target, got %#v", resp)
	}
	if rr.Header().Get("X-HA-Degraded") != "false" {
		t.Fatalf("expected non-degraded response, got headers=%v", rr.Header())
	}
	if rr.Header().Get("X-HA-Error-Type") != "none" {
		t.Fatalf("expected X-HA-Error-Type=none, got %q", rr.Header().Get("X-HA-Error-Type"))
	}
}

func TestLBWeightedAllMissingConfigUsesConfigFallback(t *testing.T) {
	rdb := &fakeRedis{
		zset: map[string]map[string]float64{"hc:g:up": {"ghost": 10}},
	}
	index := map[string]map[string]config.Target{
		"g": {
			"fast": {Name: "fast", URL: "https://fast"},
		},
	}
	srv := newTestServer(rdb, "weighted", index, nil)
	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if rr.Header().Get("X-HA-Degraded") != "false" {
		t.Fatalf("expected non-degraded config fallback, got headers=%v", rr.Header())
	}
	if rr.Header().Get("X-HA-Error-Type") != "none" {
		t.Fatalf("expected X-HA-Error-Type=none, got %q", rr.Header().Get("X-HA-Error-Type"))
	}
	if rr.Header().Get("X-HA-Path") != "config_fallback" {
		t.Fatalf("expected config_fallback path, got %q", rr.Header().Get("X-HA-Path"))
	}
}

func TestLBPerGroupStrategyOverride(t *testing.T) {
	rdb := &fakeRedis{zset: map[string]map[string]float64{"hc:g:up": {}}}
	index := map[string]map[string]config.Target{
		"g": {
			"a": {Name: "a", URL: "https://a"},
			"b": {Name: "b", URL: "https://b"},
		},
	}
	lbTypes := map[string]string{"g": "round-robin"}
	srv := newTestServerWithLBTypes(rdb, "random", index, nil, lbTypes)
	handler := srv.lbHandler()

	req1 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)
	first := lbString(decodeLB(t, rr1), "name")

	req2 := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	second := lbString(decodeLB(t, rr2), "name")

	if first == second {
		t.Fatalf("round-robin override should alternate targets, got %s twice", first)
	}
}

func TestLBRedisBackoffServesConfigFallback(t *testing.T) {
	rdb := &fakeRedis{getErr: assertErr("boom")}
	index := map[string]map[string]config.Target{
		"g": {"a": {Name: "a", URL: "https://a"}},
	}
	srv := newTestServer(rdb, "random", index, nil)
	srv.cache.markRedisDown("g")

	req := httptest.NewRequest("GET", "/v1/lb/g", nil)
	rr := httptest.NewRecorder()
	srv.lbHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	resp := decodeLB(t, rr)
	if lbString(resp, "name") != "a" {
		t.Fatalf("expected config fallback target a, got %#v", resp)
	}
	if lbString(resp, "error") != "redis_backoff" {
		t.Fatalf("expected redis_backoff error, got %#v", resp)
	}
}

// --- Check handler tests ---

func TestCheckHandlerReturnsTargets(t *testing.T) {
	val := probeResult{Reachable: true, Target: "t1", Status: "up", Type: "http"}
	b, _ := json.Marshal(val)
	rdb := &fakeRedis{hash: map[string]map[string]string{"hc:g": {"t1": string(b)}}}
	index := map[string]map[string]config.Target{
		"g": {"t1": {Name: "t1", URL: "https://t1"}},
	}
	srv := newTestServer(rdb, "random", index, nil)
	req := httptest.NewRequest("GET", "/v1/check/g", nil)
	rr := httptest.NewRecorder()
	srv.checkHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp checkResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Group != "g" {
		t.Fatalf("expected group g, got %s", resp.Group)
	}
	if resp.Redis != "ok" {
		t.Fatalf("expected redis_status ok, got %s", resp.Redis)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].Target != "t1" {
		t.Fatalf("expected 1 target t1, got %#v", resp.Targets)
	}
	if resp.Targets[0].TargetMeta.URL != "https://t1" {
		t.Fatalf("expected hydrated URL, got %#v", resp.Targets[0].TargetMeta)
	}
}

func TestCheckHandlerOnRedisError(t *testing.T) {
	rdb := &fakeRedis{getErr: assertErr("boom")}
	srv := newTestServer(rdb, "random", nil, nil)
	req := httptest.NewRequest("GET", "/v1/check/g", nil)
	rr := httptest.NewRecorder()
	srv.checkHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp checkResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Redis != "error" {
		t.Fatalf("expected redis_status=error, got %s", resp.Redis)
	}
	if !strings.Contains(resp.Message, "boom") {
		t.Fatalf("expected error message, got %s", resp.Message)
	}
}

func TestCheckHandlerEmptyGroupReturns400(t *testing.T) {
	srv := newTestServer(&fakeRedis{}, "random", nil, nil)
	req := httptest.NewRequest("GET", "/v1/check/", nil)
	rr := httptest.NewRecorder()
	srv.checkHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// --- Health handler tests ---

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

// --- Leader handler tests ---

func TestLeaderHandlerLeader(t *testing.T) {
	leaderFn := func() LeaderStatus {
		return LeaderStatus{Leader: true, ProbesActive: true, NodeID: "n1", Since: time.Unix(1000, 0)}
	}
	srv := NewServer(&fakeRedis{}, "random", nil, nil, nil, leaderFn)
	req := httptest.NewRequest("GET", "/v1/leader", nil)
	rr := httptest.NewRecorder()
	srv.leaderHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp leaderResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Leader || resp.Status != "leader" || resp.NodeID != "n1" || !resp.ProbesActive {
		t.Fatalf("unexpected leader resp: %#v", resp)
	}
	if resp.SinceUnix != 1000 {
		t.Fatalf("expected since_unix=1000, got %d", resp.SinceUnix)
	}
}

func TestLeaderHandlerFollower(t *testing.T) {
	leaderFn := func() LeaderStatus {
		return LeaderStatus{Leader: false, ProbesActive: false, NodeID: "n2", Since: time.Unix(2000, 0)}
	}
	srv := NewServer(&fakeRedis{}, "random", nil, nil, nil, leaderFn)
	req := httptest.NewRequest("GET", "/v1/leader", nil)
	rr := httptest.NewRecorder()
	srv.leaderHandler().ServeHTTP(rr, req)
	var resp leaderResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Leader || resp.Status != "follower" || resp.ProbesActive {
		t.Fatalf("expected follower, got %#v", resp)
	}
}

func TestLeaderHandlerDegraded(t *testing.T) {
	leaderFn := func() LeaderStatus {
		return LeaderStatus{Leader: false, ProbesActive: true, NodeID: "n3", Since: time.Unix(3000, 0)}
	}
	srv := NewServer(&fakeRedis{}, "random", nil, nil, nil, leaderFn)
	req := httptest.NewRequest("GET", "/v1/leader", nil)
	rr := httptest.NewRecorder()
	srv.leaderHandler().ServeHTTP(rr, req)
	var resp leaderResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Leader || resp.Status != "degraded" || !resp.ProbesActive || resp.NodeID != "n3" {
		t.Fatalf("expected degraded probes without lock, got %#v", resp)
	}
}

func TestLeaderHandlerNilFn(t *testing.T) {
	srv := NewServer(&fakeRedis{}, "random", nil, nil, nil, nil)
	req := httptest.NewRequest("GET", "/v1/leader", nil)
	rr := httptest.NewRecorder()
	srv.leaderHandler().ServeHTTP(rr, req)
	var resp leaderResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Status != "unknown" {
		t.Fatalf("expected unknown status, got %#v", resp)
	}
}

// --- Cache / selector unit tests ---

func TestCacheStateSetGet(t *testing.T) {
	c := newCacheState()
	data := []probeResult{{Reachable: true, Target: "x"}}
	c.set("g", data)
	got := c.get("g")
	if len(got) != 1 || got[0].Target != "x" {
		t.Fatalf("cache get mismatch: %#v", got)
	}
}

func TestCacheStateLBSetGetFresh(t *testing.T) {
	c := newCacheState()
	up := []probeResult{{Reachable: true, Target: "a", LatencyMs: 5}}
	cg := c.lbSet("g", groupData{}, up, up)
	if cg == nil || len(cg.hydratedUp) != 1 {
		t.Fatalf("lbSet returned nil or empty")
	}
	fresh := c.lbGetFresh("g", 5*time.Second)
	if fresh == nil {
		t.Fatal("expected fresh cache entry")
	}
	stale := c.lbGetFresh("g", 0)
	if stale != nil {
		t.Fatal("expected nil for maxAge=0")
	}
}

func TestCacheStateClear(t *testing.T) {
	c := newCacheState()
	c.set("g", []probeResult{{Reachable: true}})
	c.markRedisDown("g")
	c.clear()
	if got := c.get("g"); len(got) != 0 {
		t.Fatalf("expected clear to remove data, got %#v", got)
	}
	if c.redisBackoffActive("g") {
		t.Fatal("expected clear to remove backoff")
	}
}

func TestRedisBackoff(t *testing.T) {
	c := newCacheState()
	if c.redisBackoffActive("g") {
		t.Fatal("backoff should be inactive initially")
	}
	c.markRedisDown("g")
	if !c.redisBackoffActive("g") {
		t.Fatal("backoff should be active after markRedisDown")
	}
}

func TestNormalizeStrategy(t *testing.T) {
	cases := map[string]string{
		"random":               "random",
		"round-robin":          "round-robin",
		"roundrobin":           "round-robin",
		"rr":                   "round-robin",
		"weighted":             "weighted",
		"weighted-latency":     "weighted",
		"latency":              "weighted",
		"weighted-rr":          "weighted-rr",
		"weighted-round-robin": "weighted-rr",
		"weightedrr":           "weighted-rr",
		"bogus":                "",
		"":                     "",
	}
	for input, want := range cases {
		got := normalizeStrategy(input)
		if got != want {
			t.Errorf("normalizeStrategy(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuildWeightedPoolFavorsLowLatency(t *testing.T) {
	up := []probeResult{
		{Target: "fast", LatencyMs: 10},
		{Target: "slow", LatencyMs: 900},
	}
	pool := buildWeightedPool(up)
	if len(pool) == 0 {
		t.Fatal("expected non-empty pool")
	}
	fast, slow := 0, 0
	for _, p := range pool {
		if p.Target == "fast" {
			fast++
		} else {
			slow++
		}
	}
	if fast <= slow {
		t.Fatalf("expected fast > slow in pool, got fast=%d slow=%d", fast, slow)
	}
}

func TestBuildWeightedPoolEmpty(t *testing.T) {
	pool := buildWeightedPool(nil)
	if pool != nil {
		t.Fatalf("expected nil pool for empty input, got %d entries", len(pool))
	}
}

func TestReachableFromAll(t *testing.T) {
	all := []probeResult{
		{Reachable: true, Target: "a"},
		{Reachable: false, Target: "b"},
		{Reachable: true, Target: "c"},
	}
	up := reachableFromAll(all)
	if len(up) != 2 {
		t.Fatalf("expected 2 reachable, got %d", len(up))
	}
}

func TestBuildLBResponseBasicFields(t *testing.T) {
	pick := probeResult{Reachable: true, Target: "t1", TargetMeta: targetMeta{Name: "t1", URL: "https://t1"}}
	resp := buildLBResponse("g", true, "", "none", "cache", &pick, nil)
	if resp["group"] != "g" || resp["name"] != "t1" || resp["url"] != "https://t1" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if resp["reachable"] != true {
		t.Fatalf("expected reachable=true, got %#v", resp)
	}
}

func TestBuildLBResponseWithError(t *testing.T) {
	resp := buildLBResponse("g", false, "some error", "redis_error", "redis_hgetall_error", nil, nil)
	if resp["error"] != "some error" {
		t.Fatalf("expected error field, got %#v", resp)
	}
	if resp["name"] != "" {
		t.Fatalf("expected empty name with nil pick, got %#v", resp)
	}
}

// --- fakeRedis implementation ---

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
