package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
)

// hyde/lab: idle-aware scheduled reset. Preventive daily recovery that clears
// transient/latched charger state (e.g. a lingering fault) WITHOUT interrupting
// a customer — it only resets a charge point that has no active transaction. If
// the charger is mid-session at the scheduled time it waits for an idle window
// (up to the grace period), then resets; if still busy, it skips for the day.
//
// Env:
//   SCHEDULED_RESET_AT        = "HH:MM" UTC, daily (empty disables the feature)
//   SCHEDULED_RESET_TYPE      = Soft | Hard   (default Soft)
//   SCHEDULED_RESET_GRACE_MIN = minutes to wait for idle before skipping (default 30)

const (
	envResetAt      = "SCHEDULED_RESET_AT"
	envResetType    = "SCHEDULED_RESET_TYPE"
	envResetGrace   = "SCHEDULED_RESET_GRACE_MIN"
	resetIdlePoll   = 1 * time.Minute
	resetGraceDflt  = 30 * time.Minute
)

func startScheduledReset(h *CentralSystemHandler) {
	at := strings.TrimSpace(os.Getenv(envResetAt))
	if at == "" {
		log.Infof("scheduler: %v unset — scheduled reset disabled", envResetAt)
		return
	}
	hh, mm, err := parseHHMM(at)
	if err != nil {
		log.Errorf("scheduler: invalid %v=%q, disabled: %v", envResetAt, at, err)
		return
	}
	rtype := core.ResetTypeSoft
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envResetType)), "Hard") {
		rtype = core.ResetTypeHard
	}
	grace := resetGraceDflt
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(envResetGrace))); err == nil && v > 0 {
		grace = time.Duration(v) * time.Minute
	}
	go schedulerLoop(h, hh, mm, rtype, grace)
	log.Infof("scheduler: idle-aware %s reset scheduled daily at %02d:%02d UTC (idle grace %v)", rtype, hh, mm, grace)
}

func parseHHMM(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("want HH:MM")
	}
	hh, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	mm, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("out of range")
	}
	return hh, mm, nil
}

func schedulerLoop(h *CentralSystemHandler, hh, mm int, rtype core.ResetType, grace time.Duration) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		time.Sleep(time.Until(next))
		for _, id := range h.onlineLiveChargers() {
			go h.idleAwareReset(id, rtype, grace, "scheduled")
		}
	}
}

// onlineLiveChargers snapshots the ids of connected real (allowlisted) chargers.
func (handler *CentralSystemHandler) onlineLiveChargers() []string {
	handler.mu.RLock()
	defer handler.mu.RUnlock()
	out := make([]string, 0, len(handler.chargePoints))
	for id, st := range handler.chargePoints {
		if st.online && isLive(id) {
			out = append(out, id)
		}
	}
	return out
}

// idleAwareReset resets a charge point only when it has no active transaction.
// It waits up to `grace` for an idle window; if still charging, it skips.
// reason is a short tag for the logs/trace ("scheduled" or "on-demand").
func (handler *CentralSystemHandler) idleAwareReset(id string, rtype core.ResetType, grace time.Duration, reason string) {
	deadline := time.Now().Add(grace)
	for {
		if !handler.isOnline(id) {
			log.Warnf("scheduler: %s offline — %s reset skipped", id, reason)
			return
		}
		if _, busy := handler.activeTransaction(id); !busy {
			break // idle: proceed
		}
		if !time.Now().Before(deadline) {
			log.Infof("scheduler: %s still charging after %v grace — %s reset skipped for now", id, grace, reason)
			return
		}
		time.Sleep(resetIdlePoll)
	}
	log.Infof("scheduler: issuing %s idle-aware %s reset to %s", reason, rtype, id)
	err := centralSystem.Reset(id, func(c *core.ResetConfirmation, err error) {
		s := ""
		if c != nil {
			s = string(c.Status)
		}
		if err != nil {
			log.Errorf("scheduler: %s reset of %s failed: %v", reason, id, err)
			return
		}
		log.Infof("scheduler: %s reset of %s -> %s", reason, id, s)
		handler.traceOut(id, core.ResetFeatureName, fmt.Sprintf("%s idle %s reset → %s", reason, rtype, s))
	}, rtype)
	if err != nil {
		log.Errorf("scheduler: %s reset send to %s failed: %v", reason, id, err)
	}
}
