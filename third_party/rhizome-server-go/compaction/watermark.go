package compaction

import "sort"

// Site is what the server knows about one replica for watermark purposes: how far it has pulled
// the relay log, and when it was last seen. LastSeenUnixMs lets a dead device be evicted so it
// can't pin the watermark forever (spec/compaction.md, "Stale-site eviction").
type Site struct {
	LastPullSeq    int64
	LastSeenUnixMs int64
}

// Watermark computes the tombstone-purge watermark = min(LastPullSeq) over the sites still
// considered active, and returns the sorted ids of sites evicted as stale (unseen for STRICTLY
// longer than staleHorizonMs). An evicted site is excluded from the min so one dead device cannot
// pin the watermark; if it ever returns it does a correct full cursor=0 re-pull.
//
// With no active site (all stale, or none known) the watermark is 0 — nothing is purged, which is
// safe (there is no active reader to protect anyway). Eviction is reported, never silent.
func Watermark(sites map[string]Site, nowUnixMs, staleHorizonMs int64) (watermark int64, evicted []string) {
	haveActive := false
	for id, s := range sites {
		if nowUnixMs-s.LastSeenUnixMs > staleHorizonMs {
			evicted = append(evicted, id)
			continue
		}
		if !haveActive || s.LastPullSeq < watermark {
			watermark = s.LastPullSeq
			haveActive = true
		}
	}
	if !haveActive {
		watermark = 0
	}
	sort.Strings(evicted)
	return watermark, evicted
}
