package raftnode

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func TestStartSingleNodeBecomesLeader(t *testing.T) {
	opts := Options{
		NodeID:   "test-node",
		BindAddr: "127.0.0.1:0", // let OS pick a free port
		Bootstrap: true,
	}

	r, _, hasState, advAddr, err := Start(opts)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "not advertisable") {
			t.Skipf("skipping: transport not permitted in sandbox: %v", err)
		}
		t.Fatalf("Start failed: %v", err)
	}
	defer r.Shutdown()
	if !hasState {
		f := r.BootstrapCluster(raft.Configuration{
			Servers: []raft.Server{
				{ID: raft.ServerID(opts.NodeID), Address: raft.ServerAddress(advAddr)},
			},
		})
		if err := f.Error(); err != nil {
			t.Fatalf("bootstrap failed: %v", err)
		}
	}

	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("leader not elected in time; state=%v", r.State())
		case <-tick.C:
			if r.State() == raft.Leader {
				return
			}
		}
	}
}
