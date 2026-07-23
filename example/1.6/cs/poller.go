package main

import (
	"errors"
	"strconv"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
)

var errTimeout = errors.New("timeout waiting for configuration")

// hyde/lab: grid-side visibility via vendor config keys. The FlexPole exposes
// its live grid import and the import target as OCPP configuration keys —
// polling them gives MEASURED grid-side data (the demand-charge/leverage KPI
// input) instead of the SoC-derived estimate. Values are stored raw (vendor
// scale undocumented; observed ~100036 against a 63 A target — likely mA).
const (
	gridImportKey  = "VWGC.ChargingStationCurrentImport"
	gridTargetKey  = "VWGC.ChargingStationCurrentImportTarget"
	gridPollPeriod = 60 * time.Second

	// Commercial + hardware-wear keys — unambiguous units (unlike the grid
	// import, whose vendor scale is unconfirmed and therefore stored raw).
	priceKey      = "PosCtrlr.PricePerKwh"
	currencyKey   = "PosCtrlr.Currency"
	preauthKey    = "PosCtrlr.PreAuthorizationAmount"
	plugCyclesKey = "VWGC.ChargeGunPlugCycleCounters"
)

// pollKeys is the full set fetched in one GetConfiguration per charger per tick.
var pollKeys = []string{gridImportKey, gridTargetKey, priceKey, currencyKey, preauthKey, plugCyclesKey}

func startGridPoller(h *CentralSystemHandler) {
	go func() {
		t := time.NewTicker(gridPollPeriod)
		defer t.Stop()
		for range t.C {
			pollGrid(h)
		}
	}()
}

func pollGrid(h *CentralSystemHandler) {
	h.mu.RLock()
	ids := make([]string, 0, 2)
	for id, st := range h.chargePoints {
		if st.online && isLive(id) {
			ids = append(ids, id)
		}
	}
	h.mu.RUnlock()
	for _, id := range ids {
		keys, err := fetchConfigKeys(id, pollKeys)
		if err != nil {
			log.Debugf("charger poll %s failed: %v", id, err)
			continue
		}
		kv := make(map[string]string, len(keys))
		for _, k := range keys {
			kv[k.Key] = k.Value
		}
		// Grid import/target -> Timescale (raw vendor units) + SSE. Scale is
		// unconfirmed, so we never convert it to amps/watts here.
		grid := map[string]any{}
		for _, gk := range []string{gridImportKey, gridTargetKey} {
			v, perr := strconv.ParseFloat(kv[gk], 64)
			if perr != nil {
				continue
			}
			grid[gk] = v
			if gk == gridImportKey {
				setGridImportW(id, v) // feeds the derived LED state (led.go)
			}
			if tsdbCh != nil {
				select {
				case tsdbCh <- meterSample{
					ts: time.Now().UTC(), chargePoint: id, connectorID: 0,
					transactionID: -1, measurand: gk, unit: "raw",
					location: "Inlet", value: v, isSim: !isLive(id),
				}:
				default: // never block the poller on a full buffer
				}
			}
		}
		if len(grid) > 0 {
			bus.Publish(Event{Type: "grid", Charger: id, Data: grid})
		}
		// Commercial + wear -> Prometheus metrics + SSE (unambiguous units).
		if cd := recordCommercial(id, kv); len(cd) > 0 {
			bus.Publish(Event{Type: "commercial", Charger: id, Data: cd})
		}
	}
}

// fetchConfigKeys reads specific configuration keys WITHOUT touching the
// registry's full config cache (unlike syncGetConfiguration, which replaces it).
func fetchConfigKeys(chargerID string, want []string) ([]restConfigKey, error) {
	type res struct {
		keys []restConfigKey
		err  error
	}
	done := make(chan res, 1)
	err := centralSystem.GetConfiguration(chargerID, func(c *core.GetConfigurationConfirmation, err error) {
		if err != nil || c == nil {
			done <- res{err: err}
			return
		}
		out := make([]restConfigKey, 0, len(c.ConfigurationKey))
		for _, k := range c.ConfigurationKey {
			v := ""
			if k.Value != nil {
				v = *k.Value
			}
			out = append(out, restConfigKey{Key: k.Key, Value: v, Readonly: k.Readonly})
		}
		done <- res{keys: out}
	}, want)
	if err != nil {
		return nil, err
	}
	select {
	case r := <-done:
		return r.keys, r.err
	case <-time.After(commandTimeout):
		return nil, errTimeout
	}
}
