package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/logging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/securefirmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/security"
	"github.com/sirupsen/logrus"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

var (
	nextTransactionId = 1
)

// TransactionInfo contains info about a transaction
type TransactionInfo struct {
	id          int
	startTime   *types.DateTime
	endTime     *types.DateTime
	startMeter  int
	endMeter    int
	connectorId int
	idTag       string
}

func (ti *TransactionInfo) hasTransactionEnded() bool {
	return ti.endTime != nil && !ti.endTime.IsZero()
}

// ConnectorInfo contains status and ongoing transaction ID for a connector
type ConnectorInfo struct {
	status             core.ChargePointStatus
	currentTransaction int
}

func (ci *ConnectorInfo) hasTransactionInProgress() bool {
	return ci.currentTransaction >= 0
}

// ChargePointState contains some simple state for a connected charge point
type ChargePointState struct {
	status            core.ChargePointStatus
	diagnosticsStatus firmware.DiagnosticsStatus
	firmwareStatus    firmware.FirmwareStatus
	connectors        map[int]*ConnectorInfo // No assumptions about the # of connectors
	transactions      map[int]*TransactionInfo
	errorCode         core.ChargePointErrorCode

	// hyde/lab registry extensions (served over REST — see state.go).
	// State survives disconnect (online=false) so history stays queryable.
	online             bool
	connectedAt        time.Time
	lastSeen           time.Time
	lastBoot           time.Time
	heartbeatIntervalS int
	bootVendor         string
	bootModel          string
	bootSerial         string
	bootFirmware       string
	config             []restConfigKey
	trace              []restTraceEntry
	faults             []restFault
}

func (cps *ChargePointState) getConnector(id int) *ConnectorInfo {
	ci, ok := cps.connectors[id]
	if !ok {
		ci = &ConnectorInfo{currentTransaction: -1}
		cps.connectors[id] = ci
	}
	return ci
}

// CentralSystemHandler contains some simple state that a central system may want to keep.
// In production this will typically be replaced by database/API calls.
type CentralSystemHandler struct {
	// mu guards chargePoints and every field of every ChargePointState:
	// OCPP callbacks write while the REST/SSE layer reads concurrently.
	mu           sync.RWMutex
	chargePoints map[string]*ChargePointState
}

// ------------- Core profile callbacks -------------

func (handler *CentralSystemHandler) OnAuthorize(chargePointId string, request *core.AuthorizeRequest) (confirmation *core.AuthorizeConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	ctx, span := startCPSpan(chargePointId, request.GetFeatureName())
	defer span.End()
	handler.update(chargePointId, func(st *ChargePointState) {
		st.pushTrace("in", request.GetFeatureName(), "idTag "+request.IdTag)
	})
	logDefault(chargePointId, request.GetFeatureName()).WithContext(ctx).Infof("client authorized")
	resp := core.NewAuthorizationConfirmation(types.NewIdTagInfo(types.AuthorizationStatusAccepted))
	logFrame(chargePointId, "out", resp)
	return resp, nil
}

func (handler *CentralSystemHandler) OnBootNotification(chargePointId string, request *core.BootNotificationRequest) (confirmation *core.BootNotificationConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	ctx, span := startCPSpan(chargePointId, request.GetFeatureName())
	defer span.End()
	handler.update(chargePointId, func(st *ChargePointState) {
		st.lastBoot = time.Now()
		st.heartbeatIntervalS = heartbeatInterval
		st.bootVendor = request.ChargePointVendor
		st.bootModel = request.ChargePointModel
		st.bootSerial = request.ChargePointSerialNumber
		st.bootFirmware = request.FirmwareVersion
		st.pushTrace("in", request.GetFeatureName(),
			fmt.Sprintf("vendor %s · model %s · fw %s", request.ChargePointVendor, request.ChargePointModel, request.FirmwareVersion))
	})
	bus.Publish(Event{Type: "boot", Charger: chargePointId, Data: map[string]any{
		"vendor": request.ChargePointVendor, "model": request.ChargePointModel,
		"firmware": request.FirmwareVersion, "serial": request.ChargePointSerialNumber,
		"interval": heartbeatInterval,
	}})
	recordChargerInfo(chargePointId, request.ChargePointVendor, request.ChargePointModel, request.FirmwareVersion)
	logDefault(chargePointId, request.GetFeatureName()).WithContext(ctx).Infof("boot confirmed")
	resp := core.NewBootNotificationConfirmation(types.NewDateTime(time.Now()), heartbeatInterval, core.RegistrationStatusAccepted)
	logFrame(chargePointId, "out", resp)
	return resp, nil
}

func (handler *CentralSystemHandler) OnDataTransfer(chargePointId string, request *core.DataTransferRequest) (confirmation *core.DataTransferConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	logDefault(chargePointId, request.GetFeatureName()).Infof("received data %v", request.Data)
	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}

func (handler *CentralSystemHandler) OnHeartbeat(chargePointId string, request *core.HeartbeatRequest) (confirmation *core.HeartbeatConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	handler.update(chargePointId, nil) // liveness stamp
	bus.Publish(Event{Type: "heartbeat", Charger: chargePointId})
	logDefault(chargePointId, request.GetFeatureName()).Infof("heartbeat handled")
	return core.NewHeartbeatConfirmation(types.NewDateTime(time.Now())), nil
}

func (handler *CentralSystemHandler) OnMeterValues(chargePointId string, request *core.MeterValuesRequest) (confirmation *core.MeterValuesConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	// Surface the most recent sample on the trace + event stream.
	sampleVal, sampleUnit := "", ""
	if n := len(request.MeterValue); n > 0 {
		if s := request.MeterValue[n-1].SampledValue; len(s) > 0 {
			sampleVal = s[0].Value
			sampleUnit = string(s[0].Unit)
		}
	}
	// Resolve the connector's in-progress transaction while holding the lock —
	// the sim (like many chargers) omits transactionId on MeterValues.req, so
	// session attribution for the raw store comes from connector state. The
	// transaction's start time rides along: the FlexPole bundles the pending
	// clock-aligned buffer reading (stamped up to 15 min in the past) into the
	// first in-transaction MeterValues batch, and blanket-tagging it snapped
	// every session start back to the previous quarter-hour mark.
	txID := -1
	var txStart time.Time
	handler.update(chargePointId, func(st *ChargePointState) {
		if ci, ok := st.connectors[request.ConnectorId]; ok {
			txID = ci.currentTransaction
			if tx, ok := st.transactions[txID]; ok && tx.startTime != nil {
				txStart = tx.startTime.Time
			}
		}
		st.pushTrace("in", request.GetFeatureName(),
			fmt.Sprintf("connector %d · %s %s", request.ConnectorId, sampleVal, sampleUnit))
	})
	recordMeterValues(chargePointId, request)          // power / energy-register / SoC gauges by measurand (mode-labelled)
	tsdbEnqueue(chargePointId, txID, txStart, request) // raw per-session samples -> TimescaleDB (is_sim-flagged)
	bus.Publish(Event{Type: "meter", Charger: chargePointId, Data: map[string]any{
		"connectorId": request.ConnectorId, "value": sampleVal, "unit": sampleUnit,
	}})
	logDefault(chargePointId, request.GetFeatureName()).Infof("received meter values for connector %v. Meter values:\n", request.ConnectorId)
	for _, mv := range request.MeterValue {
		logDefault(chargePointId, request.GetFeatureName()).Printf("%v", mv)
	}
	return core.NewMeterValuesConfirmation(), nil
}

func (handler *CentralSystemHandler) OnStatusNotification(chargePointId string, request *core.StatusNotificationRequest) (confirmation *core.StatusNotificationConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	handler.update(chargePointId, func(st *ChargePointState) {
		st.errorCode = request.ErrorCode
		if request.ConnectorId > 0 {
			st.getConnector(request.ConnectorId).status = request.Status
		} else {
			st.status = request.Status
		}
		detail := fmt.Sprintf("connector %d · %s", request.ConnectorId, request.Status)
		if request.ErrorCode != core.NoError && request.ErrorCode != "" {
			detail += " · " + string(request.ErrorCode)
		}
		// Fault log: OCPP errorCode enum + free-form vendor fields — the
		// skeleton of a vendor fault timeline (vendorErrorCode carries the
		// OEM code, e.g. "0119F1").
		if faultCode, ok := faultClass(request); ok {
			info := request.Info
			if request.VendorErrorCode != "" {
				info = strings.TrimSpace(info + " [vendor " + request.VendorErrorCode + "]")
			}
			severity := "warning"
			if request.Status == core.ChargePointStatusFaulted {
				severity = "critical"
			}
			st.pushFault(faultCode, info, severity)
		}
		st.pushTrace("in", request.GetFeatureName(), detail)
	})
	bus.Publish(Event{Type: "status", Charger: chargePointId, Data: map[string]any{
		"connectorId": request.ConnectorId, "status": string(request.Status),
		"errorCode": string(request.ErrorCode), "vendorErrorCode": request.VendorErrorCode,
		"info": request.Info,
	}})
	// Faults must survive restarts: the registry's fault ring is in-memory and
	// dies with the process, and the info-level status line below never carries
	// the errorCode — so without this, Loki (the durable log) had no record of
	// a fault at all. Warn/error level also makes the operations dashboard's
	// errors panel pick it up.
	if faultCode, ok := faultClass(request); ok {
		detail := request.Info
		if request.VendorErrorCode != "" {
			detail = strings.TrimSpace(detail + " [vendor " + request.VendorErrorCode + "]")
		}
		entry := logDefault(chargePointId, request.GetFeatureName()).WithFields(logrus.Fields{
			"error_code":        faultCode,
			"vendor_error_code": request.VendorErrorCode,
			"connector":         request.ConnectorId,
		})
		line := fmt.Sprintf("charger fault: connector %d · %s · %s · %s", request.ConnectorId, request.Status, faultCode, detail)
		severity := "warning"
		if request.Status == core.ChargePointStatusFaulted {
			severity = "critical"
			entry.Error(line)
		} else {
			entry.Warn(line)
		}
		// Durable copy (survives restarts; re-seeds the REST fault ring at boot).
		go tsdbRecordFault(chargePointId, request.ConnectorId, string(request.Status), faultCode, request.VendorErrorCode, detail, severity)
	}
	if request.ConnectorId > 0 {
		logDefault(chargePointId, request.GetFeatureName()).Infof("connector %v updated status to %v", request.ConnectorId, request.Status)
	} else {
		logDefault(chargePointId, request.GetFeatureName()).Infof("all connectors updated status to %v", request.Status)
	}
	return core.NewStatusNotificationConfirmation(), nil
}

func (handler *CentralSystemHandler) OnStartTransaction(chargePointId string, request *core.StartTransactionRequest) (confirmation *core.StartTransactionConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	_, span := startCPSpan(chargePointId, request.GetFeatureName())
	defer span.End()
	handler.mu.Lock()
	info := handler.getOrCreate(chargePointId)
	info.lastSeen = time.Now()
	connector := info.getConnector(request.ConnectorId)
	if connector.currentTransaction >= 0 {
		handler.mu.Unlock()
		return nil, fmt.Errorf("connector %v is currently busy with another transaction", request.ConnectorId)
	}
	transaction := &TransactionInfo{}
	transaction.idTag = request.IdTag
	transaction.connectorId = request.ConnectorId
	transaction.startMeter = request.MeterStart
	transaction.startTime = request.Timestamp
	transaction.id = nextTransactionId
	nextTransactionId += 1
	connector.currentTransaction = transaction.id
	info.transactions[transaction.id] = transaction
	info.pushTrace("in", request.GetFeatureName(),
		fmt.Sprintf("tx %d · connector %d · idTag %s", transaction.id, transaction.connectorId, transaction.idTag))
	handler.mu.Unlock()
	bus.Publish(Event{Type: "tx.started", Charger: chargePointId, Data: map[string]any{
		"transactionId": transaction.id, "connectorId": transaction.connectorId,
		"idTag": transaction.idTag, "meterStart": transaction.startMeter,
	}})
	// TODO: check billable clients
	logDefault(chargePointId, request.GetFeatureName()).Infof("started transaction %v for connector %v", transaction.id, transaction.connectorId)
	resp := core.NewStartTransactionConfirmation(types.NewIdTagInfo(types.AuthorizationStatusAccepted), transaction.id)
	logFrame(chargePointId, "out", resp)
	return resp, nil
}

func (handler *CentralSystemHandler) OnStopTransaction(chargePointId string, request *core.StopTransactionRequest) (confirmation *core.StopTransactionConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	_, span := startCPSpan(chargePointId, request.GetFeatureName())
	defer span.End()
	handler.mu.Lock()
	info := handler.getOrCreate(chargePointId)
	info.lastSeen = time.Now()
	energyWh := 0
	transaction, ok := info.transactions[request.TransactionId]
	if ok {
		connector := info.getConnector(transaction.connectorId)
		connector.currentTransaction = -1
		transaction.endTime = request.Timestamp
		transaction.endMeter = request.MeterStop
		energyWh = request.MeterStop - transaction.startMeter
		// TODO: bill charging period to client
	}
	info.pushTrace("in", request.GetFeatureName(),
		fmt.Sprintf("tx %d · %s · %d Wh", request.TransactionId, request.Reason, energyWh))
	handler.mu.Unlock()
	bus.Publish(Event{Type: "tx.stopped", Charger: chargePointId, Data: map[string]any{
		"transactionId": request.TransactionId, "reason": string(request.Reason),
		"meterStop": request.MeterStop, "energyWh": energyWh,
	}})
	logDefault(chargePointId, request.GetFeatureName()).Infof("stopped transaction %v - %v", request.TransactionId, request.Reason)
	for _, mv := range request.TransactionData {
		logDefault(chargePointId, request.GetFeatureName()).Printf("%v", mv)
	}
	return core.NewStopTransactionConfirmation(), nil
}

// ------------- Firmware management profile callbacks -------------

func (handler *CentralSystemHandler) OnDiagnosticsStatusNotification(chargePointId string, request *firmware.DiagnosticsStatusNotificationRequest) (confirmation *firmware.DiagnosticsStatusNotificationConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	handler.update(chargePointId, func(st *ChargePointState) {
		st.diagnosticsStatus = request.Status
		st.pushTrace("in", request.GetFeatureName(), string(request.Status))
	})
	logDefault(chargePointId, request.GetFeatureName()).Infof("updated diagnostics status to %v", request.Status)
	return firmware.NewDiagnosticsStatusNotificationConfirmation(), nil
}

func (handler *CentralSystemHandler) OnFirmwareStatusNotification(chargePointId string, request *firmware.FirmwareStatusNotificationRequest) (confirmation *firmware.FirmwareStatusNotificationConfirmation, err error) {
	logFrame(chargePointId, "in", request)
	handler.update(chargePointId, func(st *ChargePointState) {
		st.firmwareStatus = request.Status
		st.pushTrace("in", request.GetFeatureName(), string(request.Status))
	})
	logDefault(chargePointId, request.GetFeatureName()).Infof("updated firmware status to %v", request.Status)
	return &firmware.FirmwareStatusNotificationConfirmation{}, nil
}

// No callbacks for Local Auth management, Reservation, Remote trigger or Smart Charging profile on central system

func (handler *CentralSystemHandler) OnSecurityEventNotification(chargingStationID string, request *security.SecurityEventNotificationRequest) (response *security.SecurityEventNotificationResponse, err error) {
	logFrame(chargingStationID, "in", request)
	logDefault(chargingStationID, request.GetFeatureName()).Infof("security event notification received")
	return security.NewSecurityEventNotificationResponse(), nil
}

func (handler *CentralSystemHandler) OnSignCertificate(chargingStationID string, request *security.SignCertificateRequest) (response *security.SignCertificateResponse, err error) {
	logFrame(chargingStationID, "in", request)
	logDefault(chargingStationID, request.GetFeatureName()).Infof("certificate signing request received")
	return security.NewSignCertificateResponse(types.GenericStatusAccepted), nil
}

func (handler *CentralSystemHandler) OnSignedFirmwareStatusNotification(chargingStationID string, request *securefirmware.SignedFirmwareStatusNotificationRequest) (response *securefirmware.SignedFirmwareStatusNotificationResponse, err error) {
	logFrame(chargingStationID, "in", request)
	logDefault(chargingStationID, request.GetFeatureName()).Infof("signed firmware status notification received")
	return securefirmware.NewFirmwareStatusNotificationResponse(), nil
}

func (handler *CentralSystemHandler) OnLogStatusNotification(chargingStationID string, request *logging.LogStatusNotificationRequest) (response *logging.LogStatusNotificationResponse, err error) {
	logFrame(chargingStationID, "in", request)
	logDefault(chargingStationID, request.GetFeatureName()).Infof("log status notification received")
	return logging.NewLogStatusNotificationResponse(), nil
}

// Utility functions

// faultClass reports whether a StatusNotification represents a fault, and the
// code to file it under: the OCPP errorCode when one is present, else the
// literal status "Faulted" — real chargers (the FlexPole mid-OTA included)
// report status=Faulted with errorCode=NoError, which must not slip through.
func faultClass(request *core.StatusNotificationRequest) (string, bool) {
	if request.ErrorCode != core.NoError && request.ErrorCode != "" {
		return string(request.ErrorCode), true
	}
	if request.Status == core.ChargePointStatusFaulted {
		return string(core.ChargePointStatusFaulted), true
	}
	return "", false
}

func logDefault(chargePointId string, feature string) *logrus.Entry {
	// mode rides along as a logrus field -> OTLP attribute -> Loki structured
	// metadata, so log panels can follow the same live/sim switch as metrics.
	return log.WithFields(logrus.Fields{"client": chargePointId, "message": feature, "mode": modeOf(chargePointId)})
}

// featureNamer is satisfied by every ocpp-go request and confirmation type.
type featureNamer interface{ GetFeatureName() string }

// maxPayloadBytes caps the `payload` field. A StopTransaction can carry
// hundreds of transactionData samples (~100 KB); the summary line already
// names what matters and the raw samples live in Timescale, so an oversized
// blob is clipped rather than shipped to Loki whole.
const maxPayloadBytes = 32 * 1024

// logFrame records one OCPP message. The log LINE is a human-readable
// diagnostic sentence — what happened, on which connector, with which code —
// so the Messages dashboard scans the way an operator reads a log, not as a
// wall of JSON. The COMPLETE object rides alongside in the `payload`
// structured field, one click away by expanding the line in Grafana. The
// action is `message`, direction is `dir`, live/sim is `mode` — all queryable.
func logFrame(chargePointId, dir string, msg featureNamer) {
	payload, err := json.Marshal(msg)
	if err != nil {
		payload = []byte(fmt.Sprintf("%+v", msg))
	}
	if len(payload) > maxPayloadBytes {
		payload = []byte(fmt.Sprintf("%s…[clipped, %d bytes total]", payload[:maxPayloadBytes], len(payload)))
	}
	logDefault(chargePointId, msg.GetFeatureName()).
		WithField("dir", dir).
		WithField("payload", string(payload)).
		Info(describeFrame(msg))
}

// describeFrame renders one OCPP message as a diagnostic sentence. Because
// the full object always ships in `payload`, this line is free to be prose:
// the thing an operator actually scans for. Message types without bespoke
// phrasing fall back to the action name.
func describeFrame(msg featureNamer) string {
	switch m := msg.(type) {

	// Connector + fault status — the line operators scan first.
	case *core.StatusNotificationRequest:
		where, when := connLabel(m.ConnectorId), stamp(m.Timestamp)
		if code, faulted := faultClass(m); faulted {
			s := "FAULT on " + where
			if when != "" {
				s += " at " + when
			}
			s += " — " + code
			if m.VendorErrorCode != "" {
				s += " (vendor " + m.VendorErrorCode + ")"
			}
			if m.Info != "" {
				s += ": " + m.Info
			}
			return s
		}
		s := fmt.Sprintf("%s is %s", where, m.Status)
		if when != "" {
			s += " (reported " + when + ")"
		}
		if m.Info != "" {
			s += " — " + m.Info
		}
		return s

	// Identity — vendor/model/firmware is the whole point of a boot.
	case *core.BootNotificationRequest:
		s := fmt.Sprintf("Booted: %s %s", m.ChargePointVendor, m.ChargePointModel)
		if m.ChargePointSerialNumber != "" {
			s += " (serial " + m.ChargePointSerialNumber + ")"
		}
		if m.FirmwareVersion != "" {
			s += " on firmware " + m.FirmwareVersion
		}
		if m.MeterType != "" {
			s += " · meter " + m.MeterType
		}
		return s
	case *core.BootNotificationConfirmation:
		return fmt.Sprintf("Boot %s — heartbeat every %ds (clock %s)", m.Status, m.Interval, stamp(m.CurrentTime))

	case *core.HeartbeatRequest:
		return "Heartbeat"

	case *core.AuthorizeRequest:
		return "Authorize requested for idTag " + m.IdTag
	case *core.AuthorizeConfirmation:
		return "Authorize " + idTagStatus(m.IdTagInfo)

	case *core.StartTransactionRequest:
		s := fmt.Sprintf("Start requested on %s — idTag %s, meter %d Wh",
			connLabel(m.ConnectorId), m.IdTag, m.MeterStart)
		if when := stamp(m.Timestamp); when != "" {
			s += " at " + when
		}
		return s
	case *core.StartTransactionConfirmation:
		return fmt.Sprintf("Transaction %d started (%s)", m.TransactionId, idTagStatus(m.IdTagInfo))

	case *core.StopTransactionRequest:
		s := fmt.Sprintf("Transaction %d stopped", m.TransactionId)
		if m.Reason != "" {
			s += " (" + string(m.Reason) + ")"
		}
		s += fmt.Sprintf(" — meter %d Wh", m.MeterStop)
		if n := len(m.TransactionData); n > 0 {
			s += fmt.Sprintf(", %d meter sample(s)", n)
			if hasSignedMeterData(m.TransactionData) {
				s += " incl. signed OCMF data"
			}
		}
		if when := stamp(m.Timestamp); when != "" {
			s += " at " + when
		}
		return s

	// Telemetry — summarise the newest sample instead of dumping the batch.
	case *core.MeterValuesRequest:
		s := "Meter values on " + connLabel(m.ConnectorId)
		if m.TransactionId != nil {
			s += fmt.Sprintf(" (tx %d)", *m.TransactionId)
		}
		if summary := summarizeSamples(m.MeterValue); summary != "" {
			s += " — " + summary
		}
		if n := len(m.MeterValue); n > 1 {
			s += fmt.Sprintf(" (+%d earlier sample(s))", n-1)
		}
		return s

	// Vendor channel — the FlexPole ships payment receipts through here.
	case *core.DataTransferRequest:
		s := "Vendor data from " + m.VendorId
		if m.MessageId != "" {
			s += ": " + m.MessageId
		}
		if extra := describeVendorData(m.Data); extra != "" {
			s += " — " + extra
		}
		return s

	case *firmware.DiagnosticsStatusNotificationRequest:
		return "Diagnostics upload: " + string(m.Status)
	case *firmware.FirmwareStatusNotificationRequest:
		return "Firmware update: " + string(m.Status)
	case *securefirmware.SignedFirmwareStatusNotificationRequest:
		return "Signed firmware update: " + string(m.Status)

	case *security.SecurityEventNotificationRequest:
		s := "Security event: " + m.Type
		if when := stamp(m.Timestamp); when != "" {
			s += " at " + when
		}
		if m.TechInfo != "" {
			s += " — " + m.TechInfo
		}
		return s
	case *security.SignCertificateRequest:
		return fmt.Sprintf("Certificate signing request (%d-byte CSR)", len(m.CSR))
	case *logging.LogStatusNotificationRequest:
		return fmt.Sprintf("Log upload: %s (request %d)", m.Status, m.RequestID)
	}
	return msg.GetFeatureName() + " received"
}

// connLabel names an OCPP connectorId — 0 addresses the station itself.
func connLabel(id int) string {
	if id == 0 {
		return "station"
	}
	return fmt.Sprintf("connector %d", id)
}

// stamp formats an OCPP DateTime; empty when absent so callers can omit it.
func stamp(t *types.DateTime) string {
	if t == nil || t.Time.IsZero() {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// idTagStatus reads an IdTagInfo status defensively.
func idTagStatus(i *types.IdTagInfo) string {
	if i == nil {
		return "(no idTagInfo)"
	}
	return string(i.Status)
}

// hasSignedMeterData reports whether any sample carries a signed (OCMF)
// reading — the Eichrecht artifact worth flagging on the line.
func hasSignedMeterData(mvs []types.MeterValue) bool {
	for _, mv := range mvs {
		for _, sv := range mv.SampledValue {
			if sv.Format == types.ValueFormatSignedData {
				return true
			}
		}
	}
	return false
}

// summarizeSamples renders the newest MeterValue's measurands compactly
// ("Power.Offered 150000 W @Outlet, Current.Offered 350 A @Outlet"), capped
// so a fat batch can't run away with the line. A signed blob is named, never
// inlined.
func summarizeSamples(mvs []types.MeterValue) string {
	if len(mvs) == 0 {
		return ""
	}
	last := mvs[len(mvs)-1]
	parts := make([]string, 0, len(last.SampledValue))
	for _, sv := range last.SampledValue {
		if len(parts) == 6 {
			parts = append(parts, "…")
			break
		}
		meas := string(sv.Measurand)
		if meas == "" {
			meas = "Energy.Active.Import.Register" // the OCPP default when omitted
		}
		if sv.Format == types.ValueFormatSignedData {
			parts = append(parts, meas+" (signed)")
			continue
		}
		p := meas + " " + sv.Value
		if sv.Unit != "" {
			p += " " + string(sv.Unit)
		}
		if sv.Location != "" {
			p += " @" + string(sv.Location)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// describeVendorData surfaces the money facts when the FlexPole ships a
// payment receipt as a vendor DataTransfer (messageId TransmitReceiptData).
// The fee arrives in minor units and the payload names no currency, so both
// forms are shown and none is invented.
func describeVendorData(data interface{}) string {
	raw, ok := data.(string)
	if !ok {
		return ""
	}
	var r struct {
		TransactionID int    `json:"transactionId"`
		ReceiptNr     string `json:"receiptNr"`
		Fee           int    `json:"fee"`
	}
	if json.Unmarshal([]byte(raw), &r) != nil {
		return ""
	}
	var parts []string
	if r.ReceiptNr != "" {
		parts = append(parts, "receipt "+r.ReceiptNr)
	}
	if r.TransactionID != 0 {
		parts = append(parts, fmt.Sprintf("tx %d", r.TransactionID))
	}
	if r.Fee != 0 {
		parts = append(parts, fmt.Sprintf("fee %.2f (minor %d)", float64(r.Fee)/100, r.Fee))
	}
	return strings.Join(parts, " · ")
}
