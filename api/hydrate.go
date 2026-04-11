package api

import (
	"sort"
	"time"

	"ha/config"
)

func hydrate(group string, stored []probeResult, index map[string]map[string]config.Target, cache *cacheState) []probeResult {
	out := make([]probeResult, 0, len(stored))
	tmap := index[group]
	for _, pr := range stored {
		if tmap != nil {
			if cfg, ok := tmap[pr.Target]; ok {
				pr.TargetMeta = targetMetaFromConfig(cfg)
			} else {
				cache.warnEvery("hydrate:missing_config:"+group, 5*time.Second, "redis entry missing config", "group", group, "target", pr.Target)
			}
		}
		out = append(out, pr)
	}
	return out
}

func reachableFromAll(all []probeResult) []probeResult {
	var out []probeResult
	for _, pr := range all {
		if pr.Reachable {
			out = append(out, pr)
		}
	}
	return out
}

func targetMetaFromConfig(cfg config.Target) targetMeta {
	return targetMeta{
		Name:     cfg.Name,
		URL:      cfg.URL,
		Endpoint: cfg.Endpoint,
		Bucket:   cfg.Bucket,
		Key:      cfg.Key,
		IP:       cfg.Host,
	}
}

func buildLBResponse(group string, reachable bool, errMsg string, pick *probeResult, overrides map[string]map[string]map[string]any) lbResponse {
	resp := lbResponse{
		"group":     group,
		"reachable": reachable,
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}

	name := ""
	var meta targetMeta
	if pick != nil {
		if pick.TargetMeta.Name != "" {
			name = pick.TargetMeta.Name
		} else {
			name = pick.Target
		}
		meta = pick.TargetMeta
	}

	if name != "" && overrides != nil {
		if g := overrides[group]; g != nil {
			if ov, ok := g[name]; ok && ov != nil {
				for k, v := range ov {
					if k == "group" || k == "reachable" || k == "error" {
						continue
					}
					resp[k] = v
				}
				if v, ok := resp["name"]; !ok {
					resp["name"] = name
				} else if s, ok := v.(string); ok && s == "" {
					resp["name"] = name
				}
				return resp
			}
		}
	}

	resp["name"] = name
	if meta.URL != "" {
		resp["url"] = meta.URL
	}
	if meta.Endpoint != "" {
		resp["endpoint"] = meta.Endpoint
	}
	if meta.Bucket != "" {
		resp["bucket"] = meta.Bucket
	}
	if meta.Key != "" {
		resp["key"] = meta.Key
	}
	if meta.IP != "" {
		resp["ip"] = meta.IP
	}
	return resp
}

func cacheFromConfig(group string, selector *lbSelector, index map[string]map[string]config.Target, cache *cacheState, errMsg string) *cachedGroup {
	return cacheFromConfigWithReachable(group, selector, index, cache, nil, errMsg)
}

func cacheFromConfigWithReachable(group string, selector *lbSelector, index map[string]map[string]config.Target, cache *cacheState, reachableNames map[string]struct{}, errMsg string) *cachedGroup {
	tmap := index[group]
	if len(tmap) == 0 {
		return nil
	}
	names := selector.ordered[group]
	if len(names) == 0 {
		names = make([]string, 0, len(tmap))
		for name := range tmap {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	hydratedAll := make([]probeResult, 0, len(names))
	hydratedUp := make([]probeResult, 0, len(names))
	useAllAsCandidates := reachableNames == nil
	for _, name := range names {
		cfg, ok := tmap[name]
		if !ok {
			continue
		}
		reachable := false
		if reachableNames != nil {
			_, reachable = reachableNames[name]
		}
		pr := probeResult{
			Reachable:  reachable,
			Target:     name,
			TargetMeta: targetMetaFromConfig(cfg),
		}
		if !reachable && errMsg != "" {
			pr.Error = errMsg
		}
		hydratedAll = append(hydratedAll, pr)
		if reachable || useAllAsCandidates {
			hydratedUp = append(hydratedUp, pr)
		}
	}
	cg := cachedGroup{data: groupData{}, hydratedAll: hydratedAll, hydratedUp: hydratedUp, weightedUp: buildWeightedPool(hydratedUp), seenAt: time.Now()}
	cache.mu.Lock()
	cache.lb[group] = cg
	cache.mu.Unlock()
	return &cg
}
