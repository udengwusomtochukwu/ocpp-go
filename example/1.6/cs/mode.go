package main

import (
	"os"
	"strings"
)

// hyde/lab: LIVE vs SIMULATION classification at the source. Every signal
// plane the CS emits — Prometheus metrics, the Timescale raw-sample store,
// structured logs, the REST registry projection — carries a live/sim marker
// derived from this ONE allowlist, so simulated chargers never pollute live
// views downstream (Grafana's global `mode` switch keys off it).
//
// LIVE_CHARGER_IDS is a comma-separated allowlist of charge-point ids that
// are real hardware; everything else (the internal compose sim, EXT01,
// ad-hoc harness ids) counts as a simulator. Cardinality note: the derived
// `mode` metric label has exactly two values ("live"|"sim") — the
// charge-point id itself stays OFF metric labels, as before.

const (
	envVarLiveChargerIDs  = "LIVE_CHARGER_IDS"
	defaultLiveChargerIDs = "A2111LHE0212606001"
)

var liveChargerIDs = func() map[string]struct{} {
	raw, ok := os.LookupEnv(envVarLiveChargerIDs)
	if !ok || strings.TrimSpace(raw) == "" {
		raw = defaultLiveChargerIDs
	}
	ids := map[string]struct{}{}
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}()

// isLive reports whether a charge point is real hardware (allowlisted).
func isLive(chargePointID string) bool {
	_, ok := liveChargerIDs[chargePointID]
	return ok
}

// modeOf is the two-valued classification used as the metric/log label.
func modeOf(chargePointID string) string {
	if isLive(chargePointID) {
		return "live"
	}
	return "sim"
}

// liveIDList returns the allowlist as a slice (for the Timescale backfill).
func liveIDList() []string {
	out := make([]string, 0, len(liveChargerIDs))
	for id := range liveChargerIDs {
		out = append(out, id)
	}
	return out
}
