package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
)

// hyde/lab: registry state extensions + the REST projection. The JSON shapes
// here are the CONTRACT the EVCMS dashboard already expects — they mirror
// web/dashboard/lib/ocpp.ts (Charger / Connector / ConfigKey / TraceEntry /
// Fault) in hydecharge-ocpi branch feat/evcms-dashboard-pr. Change them in
// lockstep or not at all.

const (
	envVarPublicWsURL = "PUBLIC_WS_URL"
	defaultPublicWs   = "wss://ocpp-lab.hydecharge.com"
	maxTraceEntries   = 40
	maxFaults         = 50
)

// --- wire shapes (dashboard contract) ---

type restConnector struct {
	ID        string  `json:"id"`
	Standard  string  `json:"standard"`
	Format    string  `json:"format"`
	PowerType string  `json:"powerType"`
	MaxKw     float64 `json:"maxKw"`
	Status    string  `json:"status"`
	TariffID  *string `json:"tariffId"`
}

type restEvse struct {
	UID        string          `json:"uid"`
	EvseID     string          `json:"evseId"`
	Status     string          `json:"status"`
	Connectors []restConnector `json:"connectors"`
}

type restConfigKey struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Readonly bool   `json:"readonly"`
}

type restTraceEntry struct {
	Time    string `json:"time"`
	Dir     string `json:"dir"` // "in" | "out"
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

type restFault struct {
	Time     string `json:"time"`
	Code     string `json:"code"`
	Info     string `json:"info"`
	Severity string `json:"severity"` // info | warning | critical
	Resolved bool   `json:"resolved"`
}

type restStats struct {
	UptimePct       float64 `json:"uptimePct"`
	AvailabilityPct float64 `json:"availabilityPct"`
	UtilizationPct  float64 `json:"utilizationPct"`
	Sessions30d     int     `json:"sessions30d"`
	Energy30dKwh    float64 `json:"energy30dKwh"`
	Revenue30d      float64 `json:"revenue30d"`
	Faults30d       int     `json:"faults30d"`
	AvgSessionKwh   float64 `json:"avgSessionKwh"`
}

type restCharger struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	OcppID            string          `json:"ocppId"`
	Serial            string          `json:"serial"`
	Vendor            string          `json:"vendor"`
	Model             string          `json:"model"`
	Firmware          string          `json:"firmware"`
	Ocpp              string          `json:"ocpp"`
	Capabilities      []string        `json:"capabilities"`
	Status            string          `json:"status"` // online|charging|offline|faulted
	Endpoint          string          `json:"endpoint"`
	LastHeartbeat     string          `json:"lastHeartbeat"`
	LastBoot          string          `json:"lastBoot"`
	HeartbeatInterval int             `json:"heartbeatInterval"`
	SignalDbm         int             `json:"signalDbm"`
	LocationID        *string         `json:"locationId"`
	LocationName      *string         `json:"locationName"`
	Evses             []restEvse      `json:"evses"`
	Config            []restConfigKey `json:"config"`
	Trace             []restTraceEntry `json:"trace"`
	FirmwareStatus    string          `json:"firmwareStatus"`
	LastDiagnostics   string          `json:"lastDiagnostics"`
	Downtime          []any           `json:"downtime"`
	Faults            []restFault     `json:"faults"`
	Tickets           []any           `json:"tickets"`
	Stats             restStats       `json:"stats"`
}

// --- state mutation helpers (all under handler.mu) ---

// getOrCreate returns the state for id, creating it if unseen. Caller must
// hold handler.mu (write).
func (handler *CentralSystemHandler) getOrCreate(id string) *ChargePointState {
	st, ok := handler.chargePoints[id]
	if !ok {
		st = &ChargePointState{connectors: map[int]*ConnectorInfo{}, transactions: map[int]*TransactionInfo{}}
		handler.chargePoints[id] = st
	}
	return st
}

// update runs fn against the (created-if-missing) state under the write lock
// and stamps lastSeen — every inbound OCPP message proves liveness.
func (handler *CentralSystemHandler) update(id string, fn func(st *ChargePointState)) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	st := handler.getOrCreate(id)
	st.lastSeen = time.Now()
	if fn != nil {
		fn(st)
	}
}

// pushTrace appends one OCPP message to the per-charger trace ring.
// Caller must hold handler.mu.
func (st *ChargePointState) pushTrace(dir, message, detail string) {
	st.trace = append(st.trace, restTraceEntry{
		Time: time.Now().Format("15:04:05"), Dir: dir, Message: message, Detail: detail,
	})
	if len(st.trace) > maxTraceEntries {
		st.trace = st.trace[len(st.trace)-maxTraceEntries:]
	}
}

// pushFault records a fault entry (newest first, capped).
// Caller must hold handler.mu.
func (st *ChargePointState) pushFault(code, info, severity string) {
	st.faults = append([]restFault{{
		Time: time.Now().Format("2 Jan 15:04"), Code: code, Info: info, Severity: severity,
	}}, st.faults...)
	if len(st.faults) > maxFaults {
		st.faults = st.faults[:maxFaults]
	}
}

// traceOut records an outbound (CS→CP) message from the REST command layer.
func (handler *CentralSystemHandler) traceOut(id, message, detail string) {
	handler.update(id, func(st *ChargePointState) { st.pushTrace("out", message, detail) })
}

// --- projection (dashboard shape) ---

func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	}
}

func fmtDay(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2 Jan 15:04")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// project converts one charge point's state into the dashboard Charger shape.
// Caller must hold handler.mu (read).
func project(id string, st *ChargePointState) restCharger {
	status := "online"
	charging := false
	connectorIDs := make([]int, 0, len(st.connectors))
	for cid, ci := range st.connectors {
		connectorIDs = append(connectorIDs, cid)
		if ci.hasTransactionInProgress() {
			charging = true
		}
	}
	sort.Ints(connectorIDs)
	faulted := st.errorCode != "" && st.errorCode != core.NoError
	for _, ci := range st.connectors {
		if ci.status == core.ChargePointStatusFaulted {
			faulted = true
		}
	}
	switch {
	case !st.online:
		status = "offline"
	case faulted:
		status = "faulted"
	case charging:
		status = "charging"
	}

	evses := make([]restEvse, 0, len(connectorIDs))
	for _, cid := range connectorIDs {
		ci := st.connectors[cid]
		evses = append(evses, restEvse{
			UID: fmt.Sprintf("%s-%d", id, cid), EvseID: "", Status: string(ci.status),
			Connectors: []restConnector{{
				ID: strconv.Itoa(cid), Standard: "—", Format: "—", PowerType: "—",
				MaxKw: 0, Status: string(ci.status), TariffID: nil,
			}},
		})
	}

	sessions := 0
	var energyWh float64
	for _, tx := range st.transactions {
		sessions++
		if tx.hasTransactionEnded() && tx.endMeter >= tx.startMeter {
			energyWh += float64(tx.endMeter - tx.startMeter)
		}
	}
	avg := 0.0
	if sessions > 0 {
		avg = energyWh / 1000 / float64(sessions)
	}

	trace := make([]restTraceEntry, len(st.trace))
	// dashboard renders newest-first; our ring is oldest-first
	for i, te := range st.trace {
		trace[len(st.trace)-1-i] = te
	}

	faults := st.faults
	if faults == nil {
		faults = []restFault{}
	}
	cfg := st.config
	if cfg == nil {
		cfg = []restConfigKey{}
	}

	return restCharger{
		ID: id, Name: id, OcppID: id,
		Serial: orDash(st.bootSerial), Vendor: orDash(st.bootVendor),
		Model: orDash(st.bootModel), Firmware: orDash(st.bootFirmware),
		Ocpp:         "OCPP 1.6J",
		Capabilities: []string{"Remote start/stop", "Smart charging", "Remote trigger"},
		Status:       status,
		Endpoint:     publicWsBase() + "/" + id,
		LastHeartbeat: humanizeSince(st.lastSeen),
		LastBoot:      fmtDay(st.lastBoot),
		HeartbeatInterval: st.heartbeatIntervalS,
		SignalDbm:     0,
		Evses:         evses,
		Config:        cfg,
		Trace:         trace,
		FirmwareStatus:  orDash(string(st.firmwareStatus)),
		LastDiagnostics: orDash(string(st.diagnosticsStatus)),
		Downtime:      []any{},
		Faults:        faults,
		Tickets:       []any{},
		Stats: restStats{
			Sessions30d: sessions, Energy30dKwh: roundTo(energyWh/1000, 2),
			AvgSessionKwh: roundTo(avg, 2), Faults30d: len(faults),
		},
	}
}

func roundTo(v float64, dp int) float64 {
	p := 1.0
	for i := 0; i < dp; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

func publicWsBase() string {
	if v := os.Getenv(envVarPublicWsURL); v != "" {
		return v
	}
	return defaultPublicWs
}

// snapshotAll projects every known charge point, sorted by id.
func (handler *CentralSystemHandler) snapshotAll() []restCharger {
	handler.mu.RLock()
	defer handler.mu.RUnlock()
	ids := make([]string, 0, len(handler.chargePoints))
	for id := range handler.chargePoints {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]restCharger, 0, len(ids))
	for _, id := range ids {
		out = append(out, project(id, handler.chargePoints[id]))
	}
	return out
}

// snapshotOne projects a single charge point.
func (handler *CentralSystemHandler) snapshotOne(id string) (restCharger, bool) {
	handler.mu.RLock()
	defer handler.mu.RUnlock()
	st, ok := handler.chargePoints[id]
	if !ok {
		return restCharger{}, false
	}
	return project(id, st), true
}

// isOnline reports whether the charge point currently holds a live socket.
func (handler *CentralSystemHandler) isOnline(id string) bool {
	handler.mu.RLock()
	defer handler.mu.RUnlock()
	st, ok := handler.chargePoints[id]
	return ok && st.online
}
