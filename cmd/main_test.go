package main

import (
	"testing"

	"ha/config"
)

func TestBuildLBResponseIndexUsesTargetGroup(t *testing.T) {
	cfg := &config.Config{
		Checks: map[string]config.CheckConfig{
			"source": {
				LB: config.LBConfig{
					TargetGroupResponses: []config.LBTargetGroupResponses{
						{
							TargetGroup: "dest",
							Targets: []config.LBResponseTarget{
								{
									Name: "shared",
									Response: map[string]any{
										"url": "https://dest.example.com",
									},
								},
							},
						},
					},
				},
			},
			"dest": {},
		},
	}

	index := buildLBResponseIndex(cfg)
	if _, ok := index["source"]; ok {
		t.Fatalf("did not expect override under source group: %#v", index["source"])
	}
	if got := index["dest"]["shared"]["url"]; got != "https://dest.example.com" {
		t.Fatalf("url = %#v, want %q", got, "https://dest.example.com")
	}
	if got := index["dest"]["shared"]["name"]; got != "shared" {
		t.Fatalf("name = %#v, want %q", got, "shared")
	}
}
