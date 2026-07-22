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
)

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
		keys, err := fetchConfigKeys(id, []string{gridImportKey, gridTargetKey})
		if err != nil {
			log.Debugf("grid poll %s failed: %v", id, err)
			continue
		}
		data := map[string]any{}
		for _, k := range keys {
			v, perr := strconv.ParseFloat(k.Value, 64)
			if perr != nil {
				continue
			}
			data[k.Key] = v
			if tsdbCh != nil {
				select {
				case tsdbCh <- meterSample{
					ts: time.Now().UTC(), chargePoint: id, connectorID: 0,
					transactionID: -1, measurand: k.Key, unit: "raw",
					location: "Inlet", value: v, isSim: !isLive(id),
				}:
				default: // never block the poller on a full buffer
				}
			}
		}
		if len(data) > 0 {
			bus.Publish(Event{Type: "grid", Charger: id, Data: data})
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
