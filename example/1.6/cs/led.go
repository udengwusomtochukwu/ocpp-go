package main

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
)

// hyde/lab: derived charger status light. The PEAK250's corner LEDs are not
// reported over OCPP, but the vendor manual's state table (see
// deploy/ocpp-lab/PEAK250-hardware-notes.md §Status LEDs) maps 1:1 onto data
// the CS already has: connector statuses + errorCode, active transactions,
// the measured grid intake, and boot recency. This derives what the unit's
// lights are showing right now — for the dashboard and the REST registry.
// Caveat carried from the manual: actual colours may differ per firmware.

const (
	ledOffline      = 0 // no live socket (ours — not in the vendor table)
	ledStarting     = 1 // blue flashing: starting up
	ledBufferCharge = 2 // blue continuous: buffer batteries charging / ready
	ledCharging     = 3 // green flashing: recharging a vehicle
	ledAvailable    = 4 // green continuous: available (idle)
	ledUnavailable  = 5 // red flashing: forced SOC recalibration / not available
	ledFault        = 6 // red continuous: charging not possible
)

var ledNames = map[int]string{
	ledOffline:      "offline",
	ledStarting:     "blue_flashing",
	ledBufferCharge: "blue",
	ledCharging:     "green_flashing",
	ledAvailable:    "green",
	ledUnavailable:  "red_flashing",
	ledFault:        "red",
}

// ledPriority orders states by how notable they are when reducing a fleet to
// the single mode-level metric (worst/most interesting wins).
var ledPriority = []int{ledFault, ledUnavailable, ledCharging, ledBufferCharge, ledStarting, ledAvailable, ledOffline}

// gridImportW holds the latest decoded grid-import watts per charge point,
// fed by the config poller (raw = 100000 + W from the AC meter; ZD-confirmed).
var (
	gridImportMu sync.Mutex
	gridImportW  = map[string]float64{}
)

func setGridImportW(chargePointID string, raw float64) {
	w := raw - 100000
	if w < 0 {
		w = 0
	}
	gridImportMu.Lock()
	gridImportW[chargePointID] = w
	gridImportMu.Unlock()
}

func getGridImportW(chargePointID string) float64 {
	gridImportMu.Lock()
	defer gridImportMu.Unlock()
	return gridImportW[chargePointID]
}

// ledCodeFor derives one charge point's light per the vendor table.
// Caller must hold handler.mu (read).
func ledCodeFor(id string, st *ChargePointState) int {
	if !st.online {
		return ledOffline
	}
	faulted := st.errorCode != "" && st.errorCode != core.NoError
	charging := false
	known, unavailable := 0, 0
	for _, ci := range st.connectors {
		if ci.status == core.ChargePointStatusFaulted {
			faulted = true
		}
		if ci.status != "" {
			known++
			if ci.status == core.ChargePointStatusUnavailable {
				unavailable++
			}
		}
		if ci.hasTransactionInProgress() {
			charging = true
		}
	}
	switch {
	case faulted:
		return ledFault
	case charging:
		return ledCharging
	case known > 0 && unavailable == known:
		// The whole unit is out (forced SOC recalibration, out-of-hours,
		// firmware install) — a single gun out with the other free stays
		// available at unit level.
		return ledUnavailable
	case getGridImportW(id) > 1000:
		return ledBufferCharge
	case !st.lastBoot.IsZero() && time.Since(st.lastBoot) < 2*time.Minute:
		return ledStarting
	default:
		return ledAvailable
	}
}

// ledStateName is the REST projection helper (caller holds handler.mu).
func ledStateName(id string, st *ChargePointState) string {
	return ledNames[ledCodeFor(id, st)]
}

// startLedTicker publishes the fleet-level light per mode every 10 s:
// ocpp_led_state{mode} = the most notable state across that mode's chargers.
func startLedTicker(h *CentralSystemHandler) {
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			if mLedState == nil {
				continue
			}
			codes := map[string][]int{}
			h.mu.RLock()
			for id, st := range h.chargePoints {
				m := modeOf(id)
				codes[m] = append(codes[m], ledCodeFor(id, st))
			}
			h.mu.RUnlock()
			ctx := context.Background()
			for mode, cs := range codes {
				best := ledOffline
				for _, want := range ledPriority {
					found := false
					for _, c := range cs {
						if c == want {
							found = true
							break
						}
					}
					if found {
						best = want
						break
					}
				}
				mLedState.Record(ctx, int64(best), metric.WithAttributes(modeAttrValue(mode)))
			}
		}
	}()
}
