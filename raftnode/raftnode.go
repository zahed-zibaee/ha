package raftnode

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
)

// Options holds basic Raft wiring derived from env/flags.
type Options struct {
	NodeID        string
	BindAddr      string
	AdvertiseAddr string
	Peers         []Peer
	Bootstrap     bool
	JoinAddrs     []string
	JoinTimeout   time.Duration
}

// Peer represents a cluster member.
type Peer struct {
	ID   string
	Addr string
}

// OptionsFromEnv builds Options from env vars with sane defaults.
func OptionsFromEnv() Options {
	nodeID := getenvDefault("RAFT_NODE_ID", "node1")
	bind := getenvDefault("RAFT_BIND_ADDR", "127.0.0.1:12000")
	adv := getenvDefault("RAFT_ADVERTISE_ADDR", bind)
	var peers []Peer
	if raw := os.Getenv("RAFT_PEERS"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			// format id@addr
			idAddr := strings.SplitN(p, "@", 2)
			if len(idAddr) != 2 {
				log.Printf("raft: skipping invalid peer %q (want id@addr)", p)
				continue
			}
			peers = append(peers, Peer{ID: idAddr[0], Addr: idAddr[1]})
		}
	}
	joinAddrs := parseList(os.Getenv("RAFT_JOIN_ADDRS"))
	bootstrap := strings.ToLower(getenvDefault("RAFT_BOOTSTRAP", "false")) == "true"
	joinTimeout := 10 * time.Second
	if v := os.Getenv("RAFT_JOIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			joinTimeout = d
		} else if n, err := strconv.Atoi(v); err == nil {
			joinTimeout = time.Duration(n) * time.Second
		}
	}
	return Options{
		NodeID:        nodeID,
		BindAddr:      bind,
		AdvertiseAddr: adv,
		Peers:         peers,
		Bootstrap:     bootstrap,
		JoinAddrs:     joinAddrs,
		JoinTimeout:   joinTimeout,
	}
}

// Start initializes an in-memory Raft node (no disk persistence).
// Caller owns the returned *raft.Raft lifecycle and the observation channel.
func Start(opts Options) (*raft.Raft, chan raft.Observation, bool, string, error) {
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(opts.NodeID)
	config.HeartbeatTimeout = 500 * time.Millisecond
	config.ElectionTimeout = 500 * time.Millisecond
	config.LeaderLeaseTimeout = 250 * time.Millisecond
	config.CommitTimeout = 200 * time.Millisecond
	config.Logger = hclog.New(&hclog.LoggerOptions{
		Name:   "raft",
		Level:  parseRaftLogLevel(),
		Output: os.Stderr,
	})
	// No snapshots/persistence per requirements.

	if opts.AdvertiseAddr == "" {
		opts.AdvertiseAddr = opts.BindAddr
	}
	adv, err := net.ResolveTCPAddr("tcp", opts.AdvertiseAddr)
	if err != nil {
		return nil, nil, false, "", fmt.Errorf("resolve advertise addr: %w", err)
	}

	transport, err := raft.NewTCPTransport(opts.BindAddr, adv, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, nil, false, "", fmt.Errorf("tcp transport: %w", err)
	}

	if opts.AdvertiseAddr == "" || strings.HasSuffix(opts.AdvertiseAddr, ":0") {
		opts.AdvertiseAddr = string(transport.LocalAddr())
	}

	logStore := raft.NewInmemStore()
	stableStore := raft.NewInmemStore()
	snapshots := raft.NewInmemSnapshotStore()
	hasState, _ := raft.HasExistingState(logStore, stableStore, snapshots)

	r, err := raft.NewRaft(config, NopFSM{}, logStore, stableStore, snapshots, transport)
	if err != nil {
		return nil, nil, false, "", fmt.Errorf("create raft: %w", err)
	}

	obsCh := make(chan raft.Observation, 64)
	obs := raft.NewObserver(obsCh, true, nil)
	r.RegisterObserver(obs)
	return r, obsCh, hasState, opts.AdvertiseAddr, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseRaftLogLevel() hclog.Level {
	level := strings.ToLower(os.Getenv("RAFT_LOG_LEVEL"))
	if level == "" {
		level = strings.ToLower(os.Getenv("LOG_LEVEL"))
	}
	switch level {
	case "debug":
		return hclog.Debug
	case "info", "":
		return hclog.Info
	case "warn", "warning":
		return hclog.Warn
	case "error":
		return hclog.Error
	default:
		return hclog.Info
	}
}

// NopFSM is a no-op FSM because we only care about leader election for scheduling.
type NopFSM struct{}

func (NopFSM) Apply(*raft.Log) interface{}         { return nil }
func (NopFSM) Snapshot() (raft.FSMSnapshot, error) { return nopSnapshot{}, nil }
func (NopFSM) Restore(rc io.ReadCloser) error      { return rc.Close() }

type nopSnapshot struct{}

func (nopSnapshot) Persist(sink raft.SnapshotSink) error { return sink.Cancel() }
func (nopSnapshot) Release()                             {}
