package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of the YAML/env configuration.
type Config struct {
	Defaults Defaults               `yaml:"default"`
	Checks   map[string]CheckConfig `yaml:"checks"`
}

// Defaults apply when a field is omitted at check/target level.
type Defaults struct {
	Interval      time.Duration `yaml:"interval"`
	Timeout       time.Duration `yaml:"timeout"`
	RedisTTL      time.Duration `yaml:"redisTTL"`
	Retry         int           `yaml:"retry"`
	StartTogether bool          `yaml:"startTogether"`
}

// CheckConfig represents a group of targets of a single type.
type CheckConfig struct {
	Type          string        `yaml:"type"`
	Interval      time.Duration `yaml:"interval"`
	Timeout       time.Duration `yaml:"timeout"`
	RedisTTL      time.Duration `yaml:"redisTTL"`
	Retry         int           `yaml:"retry"`
	JitterPct     int           `yaml:"-"` // auto-set
	StartTogether bool          `yaml:"startTogether"`
	Targets       []Target      `yaml:"targets"`
	LB            LBConfig      `yaml:"lb"`
}

// LBConfig controls load balancer response behavior.
// Defaults: random selection; when response_targets is empty, /v1/lb uses target meta fields as the response.
type LBConfig struct {
	Type            string             `yaml:"type"`
	ResponseTargets []LBResponseTarget `yaml:"response_targets"`
	LegacyTargets   []LBResponseTarget `yaml:"responseTargets"`
}

// LBResponseTarget customizes /v1/lb response for a target name.
type LBResponseTarget struct {
	Name     string         `yaml:"name"`
	Response map[string]any `yaml:"response"`
}

// Target is a union of fields used by all probe types.
type Target struct {
	Name            string            `yaml:"name"`
	Interval        time.Duration     `yaml:"interval"`
	Timeout         time.Duration     `yaml:"timeout"`
	RedisTTL        time.Duration     `yaml:"redisTTL"`
	Retry           int               `yaml:"retry"`
	JitterPct       int               `yaml:"-"`                // auto-set
	Endpoint        string            `yaml:"endpoint"`         // S3-compatible + object/bucket
	Bucket          string            `yaml:"bucket"`           // S3-compatible
	Key             string            `yaml:"key"`              // S3-compatible object
	URL             string            `yaml:"url"`              // HTTP
	ExpectStatus    StatusList        `yaml:"response"`         // HTTP expected statuses
	Method          string            `yaml:"method"`           // HTTP method
	Headers         map[string]string `yaml:"headers"`          // HTTP headers
	AuthBasicUser   string            `yaml:"auth_basic_user"`  // HTTP basic auth user
	AuthBasicPass   string            `yaml:"auth_basic_pass"`  // HTTP basic auth pass
	AuthBearer      string            `yaml:"auth_bearer"`      // HTTP bearer token
	FollowRedirects *bool             `yaml:"follow_redirects"` // HTTP follow redirects
	MaxRedirects    int               `yaml:"max_redirects"`    // HTTP redirect limit
	Addr            string            `yaml:"addr"`             // TCP/TLS/gRPC
	Hostname        string            `yaml:"hostname"`         // DNS
	Record          string            `yaml:"record"`           // DNS record type
	Resolver        string            `yaml:"resolver"`         // DNS custom resolver
	Host            string            `yaml:"host"`             // ICMP ping
	PacketSize      int               `yaml:"packet_size"`
	MinValidDays    int               `yaml:"min_valid_days"` // TLS cert
	Service         string            `yaml:"service"`        // gRPC health service
	Region          string            `yaml:"region"`         // S3-compatible
	AccessKey       string            `yaml:"access_key"`     // S3-compatible
	SecretKey       string            `yaml:"secret_key"`     // S3-compatible
	UsePathStyle    bool              `yaml:"use_path_style"` // S3-compatible
}

// StatusList allows YAML scalar "200,201" or a YAML sequence of ints.
type StatusList []int

// UnmarshalYAML implements custom parsing for StatusList.
func (s *StatusList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		var ints []int
		if err := value.Decode(&ints); err != nil {
			return err
		}
		*s = ints
		return nil
	case yaml.ScalarNode:
		txt := strings.TrimSpace(value.Value)
		if txt == "" {
			*s = nil
			return nil
		}
		parts := strings.Split(txt, ",")
		var ints []int
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return fmt.Errorf("invalid status code %q: %w", p, err)
			}
			ints = append(ints, n)
		}
		*s = ints
		return nil
	default:
		return fmt.Errorf("status list must be sequence or scalar")
	}
}

// Load reads configuration from YAML at path (or CONFIG_PATH env, or config-targets.yaml) and validates it.
func Load(path string) (*Config, error) {
	if path == "" {
		if env := os.Getenv("CONFIG_PATH"); env != "" {
			path = env
		} else {
			path = "config-targets.yaml"
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	// defaults if not provided
	const defaultJitterPct = 5
	if cfg.Defaults.Interval == 0 {
		cfg.Defaults.Interval = 10 * time.Second
	}
	if cfg.Defaults.Timeout == 0 {
		cfg.Defaults.Timeout = 2 * time.Second
	}
	if cfg.Defaults.RedisTTL == 0 {
		cfg.Defaults.RedisTTL = 15 * time.Second
	}
	if cfg.Defaults.Retry == 0 {
		cfg.Defaults.Retry = 0
	}
	cfg.Defaults.StartTogether = true

	for name, chk := range cfg.Checks {
		if chk.Interval == 0 {
			chk.Interval = cfg.Defaults.Interval
		}
		if chk.Timeout == 0 {
			chk.Timeout = cfg.Defaults.Timeout
		}
		if chk.RedisTTL == 0 {
			chk.RedisTTL = cfg.Defaults.RedisTTL
		}
		if chk.Retry == 0 {
			chk.Retry = cfg.Defaults.Retry
		}
		// Jitter is automatic and small; ignore config if provided.
		chk.JitterPct = defaultJitterPct
		chk.StartTogether = true // enforce policy

		for i := range chk.Targets {
			t := &chk.Targets[i]
			if t.Interval == 0 {
				t.Interval = chk.Interval
			}
			if t.Timeout == 0 {
				t.Timeout = chk.Timeout
			}
			if t.RedisTTL == 0 {
				t.RedisTTL = chk.RedisTTL
			}
			if t.Retry == 0 {
				t.Retry = chk.Retry
			}
			t.JitterPct = chk.JitterPct
			if len(t.ExpectStatus) == 0 {
				t.ExpectStatus = []int{200}
			}
			if t.FollowRedirects == nil {
				v := true
				t.FollowRedirects = &v
			}
			if t.MaxRedirects == 0 {
				t.MaxRedirects = 10
			}
		}
		cfg.Checks[name] = chk
	}
}

func validate(cfg *Config) error {
	if len(cfg.Checks) == 0 {
		return fmt.Errorf("no checks defined")
	}

	for groupName, chk := range cfg.Checks {
		if chk.Type == "" {
			return fmt.Errorf("check %q: type is required", groupName)
		}
		if chk.Interval <= 0 {
			return fmt.Errorf("check %q: interval must be >0", groupName)
		}
		if chk.Timeout <= 0 {
			return fmt.Errorf("check %q: timeout must be >0", groupName)
		}
		if chk.RedisTTL <= 0 {
			return fmt.Errorf("check %q: redisTTL must be >0", groupName)
		}
		if chk.Timeout >= chk.RedisTTL {
			return fmt.Errorf("check %q: timeout (%s) must be less than redisTTL (%s)", groupName, chk.Timeout, chk.RedisTTL)
		}
		if !chk.StartTogether {
			return fmt.Errorf("check %q: startTogether must be true", groupName)
		}
		if len(chk.Targets) == 0 {
			return fmt.Errorf("check %q: no targets defined", groupName)
		}

		for i := range chk.Targets {
			t := &chk.Targets[i]
			if t.Name == "" {
				return fmt.Errorf("check %q target %d: name is required", groupName, i)
			}
			if t.Interval <= 0 {
				return fmt.Errorf("check %q target %q: interval must be >0", groupName, t.Name)
			}
			if t.Timeout <= 0 {
				return fmt.Errorf("check %q target %q: timeout must be >0", groupName, t.Name)
			}
			if t.RedisTTL <= 0 {
				return fmt.Errorf("check %q target %q: redisTTL must be >0", groupName, t.Name)
			}
			if t.Timeout >= t.RedisTTL {
				return fmt.Errorf("check %q target %q: timeout (%s) must be less than redisTTL (%s)", groupName, t.Name, t.Timeout, t.RedisTTL)
			}

			if err := validateTargetByType(chk.Type, groupName, t); err != nil {
				return err
			}
		}
		if len(chk.LB.LegacyTargets) > 0 {
			return fmt.Errorf("check %q: lb.responseTargets is not supported; use response_targets", groupName)
		}
		for i, rt := range chk.LB.ResponseTargets {
			if strings.TrimSpace(rt.Name) == "" {
				return fmt.Errorf("check %q: lb.response_targets[%d].name is required", groupName, i)
			}
			if rt.Response == nil {
				return fmt.Errorf("check %q: lb.response_targets[%d].response is required", groupName, i)
			}
		}
		if chk.LB.Type != "" {
			st := strings.ToLower(strings.TrimSpace(chk.LB.Type))
			switch st {
			case "random", "round-robin", "roundrobin", "rr", "weighted", "weighted-latency", "latency", "weighted-rr", "weighted-round-robin", "weighted_round_robin", "weightedrr":
			default:
				return fmt.Errorf("check %q: lb.type %q is invalid", groupName, chk.LB.Type)
			}
		}
	}
	return nil
}

func validateTargetByType(typ, group string, t *Target) error {
	switch strings.ToLower(typ) {
	case "bucket":
		if t.Endpoint == "" || t.Bucket == "" {
			return fmt.Errorf("check %q target %q (bucket): endpoint and bucket are required", group, t.Name)
		}
	case "object":
		if t.Endpoint == "" || t.Bucket == "" || t.Key == "" {
			return fmt.Errorf("check %q target %q (object): endpoint, bucket, and key are required", group, t.Name)
		}
	case "http":
		if t.URL == "" {
			return fmt.Errorf("check %q target %q (http): url is required", group, t.Name)
		}
		if t.Method != "" {
			switch strings.ToUpper(t.Method) {
			case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
			default:
				return fmt.Errorf("check %q target %q (http): unsupported method %q", group, t.Name, t.Method)
			}
		}
		if t.AuthBearer != "" && (t.AuthBasicUser != "" || t.AuthBasicPass != "") {
			return fmt.Errorf("check %q target %q (http): bearer auth cannot be combined with basic auth", group, t.Name)
		}
		if t.AuthBasicPass != "" && t.AuthBasicUser == "" {
			return fmt.Errorf("check %q target %q (http): auth_basic_user required when auth_basic_pass is set", group, t.Name)
		}
		if t.MaxRedirects < 0 {
			return fmt.Errorf("check %q target %q (http): max_redirects must be >= 0", group, t.Name)
		}
	case "tcp":
		if t.Addr == "" {
			return fmt.Errorf("check %q target %q (tcp): addr is required", group, t.Name)
		}
	case "dns":
		if t.Hostname == "" || t.Record == "" {
			return fmt.Errorf("check %q target %q (dns): hostname and record are required", group, t.Name)
		}
		switch strings.ToUpper(t.Record) {
		case "A", "AAAA", "CNAME", "TXT", "MX", "NS", "SRV":
		default:
			return fmt.Errorf("check %q target %q (dns): unsupported record %q", group, t.Name, t.Record)
		}
	case "ping":
		if t.Host == "" {
			return fmt.Errorf("check %q target %q (ping): host is required", group, t.Name)
		}
	case "tls":
		if t.Addr == "" {
			return fmt.Errorf("check %q target %q (tls): addr is required", group, t.Name)
		}
		if t.MinValidDays == 0 {
			t.MinValidDays = 7
		}
	case "grpc":
		if t.Addr == "" {
			return fmt.Errorf("check %q target %q (grpc): addr is required", group, t.Name)
		}
	default:
		return fmt.Errorf("check %q: unknown type %q", group, typ)
	}
	return nil
}
