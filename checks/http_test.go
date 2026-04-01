package checks

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"ha/config"
)

// stubRedis is a minimal in-memory redis client substitute for tests.
type stubRedis struct {
	hash   map[string]map[string]string
	expire map[string]time.Duration
}

func (s *stubRedis) SAdd(ctx context.Context, key string, members ...interface{}) *redis.IntCmd {
	if s.hash == nil {
		s.hash = make(map[string]map[string]string)
	}
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(int64(len(members)))
	return cmd
}

func (s *stubRedis) SRem(ctx context.Context, key string, members ...interface{}) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(int64(len(members)))
	return cmd
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func (s *stubRedis) HSet(ctx context.Context, key string, values ...interface{}) *redis.IntCmd {
	if s.hash == nil {
		s.hash = make(map[string]map[string]string)
	}
	if len(values) != 2 {
		cmd := redis.NewIntCmd(ctx)
		cmd.SetErr(assertErr("expected field/value"))
		return cmd
	}
	field, _ := values[0].(string)
	var valStr string
	switch v := values[1].(type) {
	case string:
		valStr = v
	case []byte:
		valStr = string(v)
	default:
		b, _ := json.Marshal(v)
		valStr = string(b)
	}
	if s.hash[key] == nil {
		s.hash[key] = make(map[string]string)
	}
	s.hash[key][field] = valStr
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(1)
	return cmd
}

func (s *stubRedis) Expire(ctx context.Context, key string, ttl time.Duration) *redis.BoolCmd {
	if s.expire == nil {
		s.expire = make(map[string]time.Duration)
	}
	s.expire[key] = ttl
	cmd := redis.NewBoolCmd(ctx)
	cmd.SetVal(true)
	return cmd
}

func TestRunHTTPProbeSuccess(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen tcp4: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	target := config.Target{
		Name:         "ok",
		URL:          srv.URL,
		Timeout:      500 * time.Millisecond,
		Retry:        0,
		ExpectStatus: []int{200},
		RedisTTL:     2 * time.Second,
	}

	res := runHTTPProbe(context.Background(), target)
	if !res.Reachable {
		t.Fatalf("expected reachable, got %#v", res)
	}
	if res.Type != "http" {
		t.Fatalf("expected type http, got %s", res.Type)
	}
	if res.Status != "up" {
		t.Fatalf("expected status up, got %s", res.Status)
	}
	if res.Target != "ok" {
		t.Fatalf("expected target ok, got %s", res.Target)
	}
}

func TestRunHTTPProbeFailsStatus(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen tcp4: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	target := config.Target{
		Name:         "fail",
		URL:          srv.URL,
		Timeout:      500 * time.Millisecond,
		Retry:        0,
		ExpectStatus: []int{200},
		RedisTTL:     2 * time.Second,
	}

	res := runHTTPProbe(context.Background(), target)
	if res.Reachable {
		t.Fatalf("expected unreachable on 500")
	}
	if res.Error == "" {
		t.Fatalf("expected error message")
	}
	if res.Status != "down" {
		t.Fatalf("expected status down, got %s", res.Status)
	}
	if res.Target != "fail" {
		t.Fatalf("expected target fail, got %s", res.Target)
	}
}

func TestRunHTTPProbeMethodHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-Test") != "yes" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.Target{
		Name:         "post",
		URL:          srv.URL,
		Method:       "post",
		Headers:      map[string]string{"X-Test": "yes"},
		Timeout:      500 * time.Millisecond,
		Retry:        0,
		ExpectStatus: []int{200},
		RedisTTL:     2 * time.Second,
	}

	res := runHTTPProbe(context.Background(), target)
	if !res.Reachable {
		t.Fatalf("expected reachable, got %#v", res)
	}
}

func TestRunHTTPProbeBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user" || pass != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.Target{
		Name:          "basic",
		URL:           srv.URL,
		AuthBasicUser: "user",
		AuthBasicPass: "pass",
		Timeout:       500 * time.Millisecond,
		Retry:         0,
		ExpectStatus:  []int{200},
		RedisTTL:      2 * time.Second,
	}

	res := runHTTPProbe(context.Background(), target)
	if !res.Reachable {
		t.Fatalf("expected reachable, got %#v", res)
	}
}

func TestRunHTTPProbeBearerAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := config.Target{
		Name:         "bearer",
		URL:          srv.URL,
		AuthBearer:   "token123",
		Timeout:      500 * time.Millisecond,
		Retry:        0,
		ExpectStatus: []int{200},
		RedisTTL:     2 * time.Second,
	}

	res := runHTTPProbe(context.Background(), target)
	if !res.Reachable {
		t.Fatalf("expected reachable, got %#v", res)
	}
}

func TestRunHTTPProbeNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	}))
	defer srv.Close()

	follow := false
	target := config.Target{
		Name:            "noredirect",
		URL:             srv.URL,
		FollowRedirects: &follow,
		Timeout:         500 * time.Millisecond,
		Retry:           0,
		ExpectStatus:    []int{200},
		RedisTTL:        2 * time.Second,
	}

	res := runHTTPProbe(context.Background(), target)
	if res.Reachable {
		t.Fatalf("expected unreachable on redirect without follow")
	}
}

func TestRunHTTPProbeMaxRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	follow := true
	target := config.Target{
		Name:            "redirects",
		URL:             srv.URL,
		FollowRedirects: &follow,
		MaxRedirects:    1,
		Timeout:         500 * time.Millisecond,
		Retry:           0,
		ExpectStatus:    []int{200},
		RedisTTL:        2 * time.Second,
	}

	res := runHTTPProbe(context.Background(), target)
	if res.Reachable {
		t.Fatalf("expected unreachable on max redirects")
	}
	if res.Error == "" {
		t.Fatalf("expected error on max redirects")
	}
}

func TestWriteResultWritesKeyAndTTL(t *testing.T) {
	s := &stubRedis{}
	res := probeResult{
		Reachable: true,
		CheckedAt: 123,
		LatencyMs: 10,
		Type:      "http",
	}
	ctx := context.Background()
	if err := writeResult(ctx, s, "group", "target", res, 5*time.Second); err != nil {
		t.Fatalf("writeResult error: %v", err)
	}
	if len(s.hash["hc:group"]) != 1 {
		t.Fatalf("expected hash entry written")
	}
	if s.expire["hc:group"] != 5*time.Second {
		t.Fatalf("unexpected ttl %v", s.expire["hc:group"])
	}
	var decoded probeResult
	val := s.hash["hc:group"]["target"]
	if err := json.Unmarshal([]byte(val), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Reachable || decoded.Type != "http" {
		t.Fatalf("decoded mismatch: %#v", decoded)
	}
}
