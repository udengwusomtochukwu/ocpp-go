package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

// hyde/lab: the operator/system-facing REST + SSE surface (epic #68: every
// capability reachable over REST; #69 rule 4: registry queryable over REST).
// Serves the EXACT Charger shape the EVCMS dashboard already expects
// (web/dashboard/lib/ocpp.ts) so OCPP_API_BASE flips it live unchanged.
const (
	envVarAPIPort  = "API_PORT"
	defaultAPIPort = "8080"
	envVarAPIToken = "API_TOKEN" // when set, POSTs require X-Internal-Token
	commandTimeout = 15 * time.Second
)

//go:embed openapi.yaml
var openapiSpec []byte

//go:embed docs.html
var docsPage []byte

func startREST(h *CentralSystemHandler) {
	port := defaultAPIPort
	if p, ok := os.LookupEnv(envVarAPIPort); ok && p != "" {
		port = p
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { route(w, r, h) })
	go func() {
		log.Infof("serving REST/SSE API on :%s/api", port)
		if err := http.ListenAndServe(":"+port, mux); err != nil {
			log.Errorf("api server stopped: %v", err)
		}
	}()
}

func route(w http.ResponseWriter, r *http.Request, h *CentralSystemHandler) {
	// CORS: the lab API is meant to be consumed by browser UIs anywhere.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Internal-Token")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet && path == "/api/health":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().UTC()})
	case r.Method == http.MethodGet && path == "/api/openapi.yaml":
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	case r.Method == http.MethodGet && path == "/api/docs":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(docsPage)
	case r.Method == http.MethodGet && path == "/api/events":
		serveSSE(w, r)
	case r.Method == http.MethodGet && path == "/api/chargers":
		writeJSON(w, http.StatusOK, map[string]any{"data": h.snapshotAll()})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/chargers/"):
		rest := strings.TrimPrefix(path, "/api/chargers/")
		if !strings.Contains(rest, "/") {
			if c, ok := h.snapshotOne(rest); ok {
				writeJSON(w, http.StatusOK, c)
			} else {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown charger"})
			}
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/chargers/"):
		rest := strings.TrimPrefix(path, "/api/chargers/")
		parts := strings.Split(rest, "/")
		if len(parts) == 3 && parts[1] == "commands" {
			if !authorized(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or bad X-Internal-Token"})
				return
			}
			handleCommand(w, r, h, parts[0], parts[2])
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func authorized(r *http.Request) bool {
	want := os.Getenv(envVarAPIToken)
	return want == "" || r.Header.Get("X-Internal-Token") == want
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- SSE ---

func serveSSE(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	chargerFilter := r.URL.Query().Get("charger")
	emit := func(ev Event) {
		if chargerFilter != "" && ev.Charger != chargerFilter {
			return
		}
		data, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
		f.Flush()
	}

	if n, err := strconv.Atoi(r.URL.Query().Get("replay")); err == nil && n > 0 {
		for _, ev := range bus.Replay(n) {
			emit(ev)
		}
	}

	ch, unsub := bus.Subscribe()
	defer unsub()
	// Comment heartbeat keeps Cloudflare-proxied connections alive (~100s idle cut).
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev := <-ch:
			emit(ev)
		case <-ping.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			f.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// --- commands (sync-with-timeout wrappers over the ocpp-go central system) ---

type cmdOutcome struct {
	status string
	detail map[string]any
	err    error
}

func await(done <-chan cmdOutcome) (cmdOutcome, bool) {
	select {
	case out := <-done:
		return out, true
	case <-time.After(commandTimeout):
		return cmdOutcome{}, false
	}
}

func handleCommand(w http.ResponseWriter, r *http.Request, h *CentralSystemHandler, chargerID, cmd string) {
	if _, known := h.snapshotOne(chargerID); !known {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown charger"})
		return
	}
	if !h.isOnline(chargerID) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "charger offline"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body) // empty body is fine
	str := func(k, def string) string {
		if v, ok := body[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	num := func(k string, def int) int {
		if v, ok := body[k].(float64); ok {
			return int(v)
		}
		return def
	}

	done := make(chan cmdOutcome, 1)
	var sendErr error
	feature := ""

	switch cmd {
	case "remote-start":
		feature = core.RemoteStartTransactionFeatureName
		idTag := str("idTag", "HYDE-LAB")
		connector := num("connectorId", 0)
		sendErr = centralSystem.RemoteStartTransaction(chargerID, func(c *core.RemoteStartTransactionConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, nil, err, c == nil)
		}, idTag, func(req *core.RemoteStartTransactionRequest) {
			if connector > 0 {
				req.ConnectorId = &connector
			}
		})
	case "remote-stop":
		feature = core.RemoteStopTransactionFeatureName
		txID := num("transactionId", -1)
		if txID < 0 {
			// UI ergonomics: omitted transactionId resolves to the charger's
			// single active transaction (dashboards don't track OCPP tx ids).
			if active, ok := h.activeTransaction(chargerID); ok {
				txID = active
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "transactionId required (no active transaction found)"})
				return
			}
		}
		sendErr = centralSystem.RemoteStopTransaction(chargerID, func(c *core.RemoteStopTransactionConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, nil, err, c == nil)
		}, txID)
	case "update-firmware":
		// OCPP 1.6 FirmwareManagement: the charger downloads the package from
		// `location` and reports progress via FirmwareStatusNotification
		// (visible in the registry + SSE). This unit signature-verifies
		// packages (VWGC.UpdateSignaturePublicKey) — only vendor-signed
		// packages will install.
		feature = firmware.UpdateFirmwareFeatureName
		loc := str("location", "")
		if !strings.HasPrefix(loc, "https://") && !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "ftp://") && !strings.HasPrefix(loc, "ftps://") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "location must be an http(s)/ftp(s) URL to the firmware package"})
			return
		}
		retrieve := types.NewDateTime(time.Now().Add(10 * time.Second))
		if s := str("retrieveDate", ""); s != "" {
			if t, perr := time.Parse(time.RFC3339, s); perr == nil {
				retrieve = types.NewDateTime(t)
			}
		}
		sendErr = centralSystem.UpdateFirmware(chargerID, func(c *firmware.UpdateFirmwareConfirmation, err error) {
			done <- confirmOutcome("Accepted", map[string]any{"location": loc, "retrieveDate": retrieve.FormatTimestamp()}, err, c == nil)
		}, loc, retrieve)
	case "unlock-connector":
		feature = core.UnlockConnectorFeatureName
		connector := num("connectorId", 1)
		sendErr = centralSystem.UnlockConnector(chargerID, func(c *core.UnlockConnectorConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, map[string]any{"connectorId": connector}, err, c == nil)
		}, connector)
	case "trigger":
		feature = remotetrigger.TriggerMessageFeatureName
		requested := str("requested", "StatusNotification")
		valid := map[string]bool{"BootNotification": true, "DiagnosticsStatusNotification": true,
			"FirmwareStatusNotification": true, "Heartbeat": true, "MeterValues": true, "StatusNotification": true}
		if !valid[requested] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid requested message"})
			return
		}
		sendErr = centralSystem.TriggerMessage(chargerID, func(c *remotetrigger.TriggerMessageConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, map[string]any{"requested": requested}, err, c == nil)
		}, remotetrigger.MessageTrigger(requested))
	case "reset":
		feature = core.ResetFeatureName
		kind := str("type", "Soft")
		if kind != "Soft" && kind != "Hard" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be Soft or Hard"})
			return
		}
		sendErr = centralSystem.Reset(chargerID, func(c *core.ResetConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, map[string]any{"type": kind}, err, c == nil)
		}, core.ResetType(kind))
	case "change-availability":
		// Take a charge point (or one connector) in/out of service. The lever
		// for fencing off faulty hardware remotely — e.g. a unit throwing a
		// recurring HCU precharge fault that keeps aborting customer sessions.
		// connectorId 0 (default) = the whole charge point.
		feature = core.ChangeAvailabilityFeatureName
		var avail core.AvailabilityType
		switch str("type", "") {
		case "Operative":
			avail = core.AvailabilityTypeOperative
		case "Inoperative":
			avail = core.AvailabilityTypeInoperative
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type must be Operative or Inoperative"})
			return
		}
		connector := num("connectorId", 0)
		sendErr = centralSystem.ChangeAvailability(chargerID, func(c *core.ChangeAvailabilityConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, map[string]any{"connectorId": connector, "type": string(avail)}, err, c == nil)
		}, connector, avail)
	case "get-configuration":
		var keys []string
		if raw, ok := body["keys"].([]any); ok {
			for _, k := range raw {
				if s, ok := k.(string); ok {
					keys = append(keys, s)
				}
			}
		}
		conf, err := syncGetConfiguration(chargerID, keys, h)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		finishCommand(w, h, chargerID, "GetConfiguration", cmdOutcome{status: "Accepted", detail: map[string]any{
			"configurationKey": conf, "count": len(conf),
		}})
		return
	case "change-configuration":
		feature = core.ChangeConfigurationFeatureName
		key, value := str("key", ""), str("value", "")
		if key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key required"})
			return
		}
		sendErr = centralSystem.ChangeConfiguration(chargerID, func(c *core.ChangeConfigurationConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, map[string]any{"key": key, "value": value}, err, c == nil)
		}, key, value)
	case "set-charging-profile":
		// EMS seam (plan 33): cap a connector (or the whole charge point,
		// connectorId 0) at `limit` in `unit` ("W" or "A").
		feature = smartcharging.SetChargingProfileFeatureName
		connector := num("connectorId", 0)
		limit, ok := body["limit"].(float64)
		if !ok || limit <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit (number > 0) required"})
			return
		}
		unit := str("unit", "W")
		if unit != "W" && unit != "A" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unit must be W or A"})
			return
		}
		purpose := types.ChargingProfilePurposeTxDefaultProfile
		if connector == 0 {
			purpose = types.ChargingProfilePurposeChargePointMaxProfile
		}
		profile := types.NewChargingProfile(
			num("profileId", 1), num("stackLevel", 0), purpose, types.ChargingProfileKindAbsolute,
			types.NewChargingSchedule(types.ChargingRateUnitType(unit), types.NewChargingSchedulePeriod(0, limit)))
		sendErr = centralSystem.SetChargingProfile(chargerID, func(c *smartcharging.SetChargingProfileConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, map[string]any{"connectorId": connector, "limit": limit, "unit": unit}, err, c == nil)
		}, connector, profile)
	case "clear-charging-profile":
		feature = smartcharging.ClearChargingProfileFeatureName
		sendErr = centralSystem.ClearChargingProfile(chargerID, func(c *smartcharging.ClearChargingProfileConfirmation, err error) {
			s := ""
			if c != nil {
				s = string(c.Status)
			}
			done <- confirmOutcome(s, nil, err, c == nil)
		})
	case "get-composite-schedule":
		feature = smartcharging.GetCompositeScheduleFeatureName
		connector := num("connectorId", 0)
		duration := num("durationSeconds", 3600)
		sendErr = centralSystem.GetCompositeSchedule(chargerID, func(c *smartcharging.GetCompositeScheduleConfirmation, err error) {
			detail := map[string]any{"connectorId": connector, "durationSeconds": duration}
			s := ""
			if c != nil {
				s = string(c.Status)
				if c.ChargingSchedule != nil {
					detail["chargingSchedule"] = c.ChargingSchedule
				}
			}
			done <- confirmOutcome(s, detail, err, c == nil)
		}, connector, duration)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown command", "commands": []string{
			"remote-start", "remote-stop", "trigger", "reset", "unlock-connector", "get-configuration",
			"change-configuration", "set-charging-profile", "clear-charging-profile", "get-composite-schedule",
			"update-firmware", "change-availability",
		}})
		return
	}

	if sendErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": sendErr.Error()})
		return
	}
	out, ok := await(done)
	if !ok {
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "timeout waiting for charger response"})
		return
	}
	if out.err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": out.err.Error()})
		return
	}
	finishCommand(w, h, chargerID, feature, out)
}

func confirmOutcome(status string, detail map[string]any, err error, nilConf bool) cmdOutcome {
	if err == nil && nilConf {
		err = fmt.Errorf("empty confirmation from charge point")
	}
	return cmdOutcome{status: status, detail: detail, err: err}
}

// finishCommand records the outbound message on the trace, mirrors the result
// onto the event stream, and writes the HTTP response.
func finishCommand(w http.ResponseWriter, h *CentralSystemHandler, chargerID, feature string, out cmdOutcome) {
	detailStr := ""
	if len(out.detail) > 0 {
		if b, err := json.Marshal(out.detail); err == nil {
			detailStr = string(b)
		}
	}
	h.traceOut(chargerID, feature, fmt.Sprintf("%s → %s", detailStr, out.status))
	data := map[string]any{"command": feature, "status": out.status}
	for k, v := range out.detail {
		data[k] = v
	}
	bus.Publish(Event{Type: "command.result", Charger: chargerID, Data: data})
	writeJSON(w, http.StatusOK, map[string]any{
		"charger": chargerID, "command": feature, "status": out.status, "detail": out.detail,
	})
}

// syncGetConfiguration fetches OCPP configuration keys and refreshes the
// registry's config cache. Shared by the REST endpoint and the post-boot
// prefetch in central_system_sim.go.
func syncGetConfiguration(chargerID string, keys []string, h *CentralSystemHandler) ([]restConfigKey, error) {
	type confResult struct {
		keys []restConfigKey
		err  error
	}
	done := make(chan confResult, 1)
	err := centralSystem.GetConfiguration(chargerID, func(c *core.GetConfigurationConfirmation, err error) {
		if err != nil || c == nil {
			if err == nil {
				err = fmt.Errorf("empty confirmation")
			}
			done <- confResult{err: err}
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
		done <- confResult{keys: out}
	}, keys)
	if err != nil {
		return nil, err
	}
	select {
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		h.update(chargerID, func(st *ChargePointState) { st.config = res.keys })
		return res.keys, nil
	case <-time.After(commandTimeout):
		return nil, fmt.Errorf("timeout waiting for configuration")
	}
}
