package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadAppliesDefaultsAndJitter(t *testing.T) {
	yml := `
default:
  interval: 0s
checks:
  web:
    type: http
    targets:
      - name: a
        url: https://example.com
`
	path := writeTempConfig(t, yml)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	web := cfg.Checks["web"]
	if got, want := web.Interval, 10*time.Second; got != want {
		t.Fatalf("interval = %v, want %v", got, want)
	}
	if got, want := web.Timeout, 2*time.Second; got != want {
		t.Fatalf("timeout = %v, want %v", got, want)
	}
	if got, want := web.RedisTTL, 15*time.Second; got != want {
		t.Fatalf("redisTTL = %v, want %v", got, want)
	}
	if got, want := web.JitterPct, 5; got != want {
		t.Fatalf("jitterPct = %v, want %v", got, want)
	}
	if !web.StartTogether {
		t.Fatalf("startTogether should be enforced true")
	}
	if len(web.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(web.Targets))
	}
	tgt := web.Targets[0]
	if got, want := tgt.JitterPct, 5; got != want {
		t.Fatalf("target jitterPct = %v, want %v", got, want)
	}
	if len(tgt.ExpectStatus) != 1 || tgt.ExpectStatus[0] != 200 {
		t.Fatalf("expectStatus default not applied, got %#v", tgt.ExpectStatus)
	}
	if tgt.Timeout >= tgt.RedisTTL {
		t.Fatalf("timeout must be less than redisTTL")
	}
}

func TestLoadRejectsTimeoutNotLessThanTTL(t *testing.T) {
	yml := `
checks:
  web:
    type: http
    timeout: 3s
    redisTTL: 3s
    targets:
      - name: a
        url: https://example.com
`
	path := writeTempConfig(t, yml)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected validation error for timeout >= redisTTL")
	}
}

func TestLoadRejectsUnknownType(t *testing.T) {
	yml := `
checks:
  bad:
    type: nope
    targets:
      - name: a
        url: https://example.com
`
	path := writeTempConfig(t, yml)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected validation error for unknown type")
	}
}

func TestLoadTargetGroupResponses(t *testing.T) {
	yml := `
checks:
  web:
    type: http
    targets:
      - name: shared
        url: https://web.example.com
    lb:
      target_group_responses:
        - web:
            - name: shared
              response:
                url: https://public-web.example.com
  api:
    type: http
    targets:
      - name: shared
        url: https://api.example.com
    lb:
      target_group_responses:
        - api:
            - name: shared
              response:
                url: https://public-api.example.com
`
	path := writeTempConfig(t, yml)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := cfg.Checks["web"].LB.TargetGroupResponses[0].TargetGroup; got != "web" {
		t.Fatalf("web target group = %q, want %q", got, "web")
	}
	if got := cfg.Checks["web"].LB.TargetGroupResponses[0].Targets[0].Name; got != "shared" {
		t.Fatalf("web target name = %q, want %q", got, "shared")
	}
	if got := cfg.Checks["api"].LB.TargetGroupResponses[0].TargetGroup; got != "api" {
		t.Fatalf("api target group = %q, want %q", got, "api")
	}
}

func TestLoadRejectsUnknownLBTargetGroupResponsesGroup(t *testing.T) {
	yml := `
checks:
  web:
    type: http
    targets:
      - name: a
        url: https://example.com
    lb:
      target_group_responses:
        - missing:
            - name: a
              response:
                url: https://public.example.com
`
	path := writeTempConfig(t, yml)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected validation error for unknown lb target group")
	}
}

func TestLoadRejectsUnknownLBTargetGroupResponsesTargetName(t *testing.T) {
	yml := `
checks:
  web:
    type: http
    targets:
      - name: a
        url: https://example.com
    lb:
      target_group_responses:
        - web:
            - name: missing
              response:
                url: https://public.example.com
`
	path := writeTempConfig(t, yml)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected validation error for unknown lb target name")
	}
}

func TestLoadRejectsLegacyLBResponseTargets(t *testing.T) {
	yml := `
checks:
  web:
    type: http
    targets:
      - name: a
        url: https://example.com
    lb:
      response_targets:
        - name: a
          response:
            url: https://public.example.com
`
	path := writeTempConfig(t, yml)
	if _, err := Load(path); err == nil {
		t.Fatalf("expected validation error for legacy response_targets")
	}
}
