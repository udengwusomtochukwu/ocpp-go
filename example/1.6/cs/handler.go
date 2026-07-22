package main

import (
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
	ctx, span := startCPSpan(chargePointId, request.GetFeatureName())
	defer span.End()
	handler.update(chargePointId, func(st *ChargePointState) {
		st.pushTrace("in", request.GetFeatureName(), "idTag "+request.IdTag)
	})
	logDefault(chargePointId, request.GetFeatureName()).WithContext(ctx).Infof("client authorized")
	return core.NewAuthorizationConfirmation(types.NewIdTagInfo(types.AuthorizationStatusAccepted)), nil
}

func (handler *CentralSystemHandler) OnBootNotification(chargePointId string, request *core.BootNotificationRequest) (confirmation *core.BootNotificationConfirmation, err error) {
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
	return core.NewBootNotificationConfirmation(types.NewDateTime(time.Now()), heartbeatInterval, core.RegistrationStatusAccepted), nil
}

func (handler *CentralSystemHandler) OnDataTransfer(chargePointId string, request *core.DataTransferRequest) (confirmation *core.DataTransferConfirmation, err error) {
	logDefault(chargePointId, request.GetFeatureName()).Infof("received data %v", request.Data)
	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}

func (handler *CentralSystemHandler) OnHeartbeat(chargePointId string, request *core.HeartbeatRequest) (confirmation *core.HeartbeatConfirmation, err error) {
	handler.update(chargePointId, nil) // liveness stamp
	bus.Publish(Event{Type: "heartbeat", Charger: chargePointId})
	logDefault(chargePointId, request.GetFeatureName()).Infof("heartbeat handled")
	return core.NewHeartbeatConfirmation(types.NewDateTime(time.Now())), nil
}

func (handler *CentralSystemHandler) OnMeterValues(chargePointId string, request *core.MeterValuesRequest) (confirmation *core.MeterValuesConfirmation, err error) {
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
	// session attribution for the raw store comes from connector state.
	txID := -1
	handler.update(chargePointId, func(st *ChargePointState) {
		if ci, ok := st.connectors[request.ConnectorId]; ok {
			txID = ci.currentTransaction
		}
		st.pushTrace("in", request.GetFeatureName(),
			fmt.Sprintf("connector %d · %s %s", request.ConnectorId, sampleVal, sampleUnit))
	})
	recordMeterValues(chargePointId, request) // power / energy-register / SoC gauges by measurand (mode-labelled)
	tsdbEnqueue(chargePointId, txID, request) // raw per-session samples -> TimescaleDB (is_sim-flagged)
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
	handler.update(chargePointId, func(st *ChargePointState) {
		st.errorCode = request.ErrorCode
		if request.ConnectorId > 0 {
			st.getConnector(request.ConnectorId).status = request.Status
		} else {
			st.status = request.Status
		}
		detail := fmt.Sprintf("connector %d · %s", request.ConnectorId, request.Status)
		if request.ErrorCode != core.NoError {
			detail += " · " + string(request.ErrorCode)
			// Fault log: OCPP errorCode enum + free-form vendor fields — the
			// skeleton of a vendor fault timeline (vendorErrorCode carries the
			// OEM code, e.g. "0119F1").
			info := request.Info
			if request.VendorErrorCode != "" {
				info = strings.TrimSpace(info + " [vendor " + request.VendorErrorCode + "]")
			}
			severity := "warning"
			if request.Status == core.ChargePointStatusFaulted {
				severity = "critical"
			}
			st.pushFault(string(request.ErrorCode), info, severity)
		}
		st.pushTrace("in", request.GetFeatureName(), detail)
	})
	bus.Publish(Event{Type: "status", Charger: chargePointId, Data: map[string]any{
		"connectorId": request.ConnectorId, "status": string(request.Status),
		"errorCode": string(request.ErrorCode), "vendorErrorCode": request.VendorErrorCode,
		"info": request.Info,
	}})
	if request.ConnectorId > 0 {
		logDefault(chargePointId, request.GetFeatureName()).Infof("connector %v updated status to %v", request.ConnectorId, request.Status)
	} else {
		logDefault(chargePointId, request.GetFeatureName()).Infof("all connectors updated status to %v", request.Status)
	}
	return core.NewStatusNotificationConfirmation(), nil
}

func (handler *CentralSystemHandler) OnStartTransaction(chargePointId string, request *core.StartTransactionRequest) (confirmation *core.StartTransactionConfirmation, err error) {
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
	return core.NewStartTransactionConfirmation(types.NewIdTagInfo(types.AuthorizationStatusAccepted), transaction.id), nil
}

func (handler *CentralSystemHandler) OnStopTransaction(chargePointId string, request *core.StopTransactionRequest) (confirmation *core.StopTransactionConfirmation, err error) {
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
	handler.update(chargePointId, func(st *ChargePointState) {
		st.diagnosticsStatus = request.Status
		st.pushTrace("in", request.GetFeatureName(), string(request.Status))
	})
	logDefault(chargePointId, request.GetFeatureName()).Infof("updated diagnostics status to %v", request.Status)
	return firmware.NewDiagnosticsStatusNotificationConfirmation(), nil
}

func (handler *CentralSystemHandler) OnFirmwareStatusNotification(chargePointId string, request *firmware.FirmwareStatusNotificationRequest) (confirmation *firmware.FirmwareStatusNotificationConfirmation, err error) {
	handler.update(chargePointId, func(st *ChargePointState) {
		st.firmwareStatus = request.Status
		st.pushTrace("in", request.GetFeatureName(), string(request.Status))
	})
	logDefault(chargePointId, request.GetFeatureName()).Infof("updated firmware status to %v", request.Status)
	return &firmware.FirmwareStatusNotificationConfirmation{}, nil
}

// No callbacks for Local Auth management, Reservation, Remote trigger or Smart Charging profile on central system

func (handler *CentralSystemHandler) OnSecurityEventNotification(chargingStationID string, request *security.SecurityEventNotificationRequest) (response *security.SecurityEventNotificationResponse, err error) {
	logDefault(chargingStationID, request.GetFeatureName()).Infof("security event notification received")
	return security.NewSecurityEventNotificationResponse(), nil
}

func (handler *CentralSystemHandler) OnSignCertificate(chargingStationID string, request *security.SignCertificateRequest) (response *security.SignCertificateResponse, err error) {
	logDefault(chargingStationID, request.GetFeatureName()).Infof("certificate signing request received")
	return security.NewSignCertificateResponse(types.GenericStatusAccepted), nil
}

func (handler *CentralSystemHandler) OnSignedFirmwareStatusNotification(chargingStationID string, request *securefirmware.SignedFirmwareStatusNotificationRequest) (response *securefirmware.SignedFirmwareStatusNotificationResponse, err error) {
	logDefault(chargingStationID, request.GetFeatureName()).Infof("signed firmware status notification received")
	return securefirmware.NewFirmwareStatusNotificationResponse(), nil
}

func (handler *CentralSystemHandler) OnLogStatusNotification(chargingStationID string, request *logging.LogStatusNotificationRequest) (response *logging.LogStatusNotificationResponse, err error) {
	logDefault(chargingStationID, request.GetFeatureName()).Infof("log status notification received")
	return logging.NewLogStatusNotificationResponse(), nil
}

// Utility functions

func logDefault(chargePointId string, feature string) *logrus.Entry {
	// mode rides along as a logrus field -> OTLP attribute -> Loki structured
	// metadata, so log panels can follow the same live/sim switch as metrics.
	return log.WithFields(logrus.Fields{"client": chargePointId, "message": feature, "mode": modeOf(chargePointId)})
}
