package main

import (
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

// TestDescribeFrame pins the human-readable diagnostic line for every message
// type the FlexPole actually sends. The payloads below are lifted verbatim
// from the deployed charger's Loki stream, so this is a regression fence on
// real traffic, not invented shapes.
func TestDescribeFrame(t *testing.T) {
	at := func(s string) *types.DateTime {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("bad fixture timestamp %q: %v", s, err)
		}
		return types.NewDateTime(ts)
	}

	cases := []struct {
		name string
		msg  featureNamer
		want string
	}{
		{
			name: "status: charging connector",
			msg: &core.StatusNotificationRequest{
				ConnectorId: 1, ErrorCode: core.NoError, Status: core.ChargePointStatusCharging,
				Timestamp: at("2026-07-23T07:43:39Z"), VendorId: "ZD",
			},
			want: "connector 1 is Charging (reported 2026-07-23T07:43:39Z)",
		},
		{
			// connectorId 0 addresses the station itself, and `info` carries
			// the operator-visible detail.
			name: "status: station-wide with info",
			msg: &core.StatusNotificationRequest{
				ConnectorId: 0, ErrorCode: core.NoError, Status: core.ChargePointStatusAvailable,
				Info: "Maintenance LAN-port activated.", Timestamp: at("2026-07-23T04:53:34Z"), VendorId: "ZD",
			},
			want: "station is Available (reported 2026-07-23T04:53:34Z) — Maintenance LAN-port activated.",
		},
		{
			// The OTA fault signature seen live on 2026-07-23.
			name: "status: fault with vendor code",
			msg: &core.StatusNotificationRequest{
				ConnectorId: 1, ErrorCode: core.OtherError, Info: "Hflash is busy now",
				VendorErrorCode: "2", Status: core.ChargePointStatusFaulted, Timestamp: at("2026-07-23T02:52:00Z"),
			},
			want: "FAULT on connector 1 at 2026-07-23T02:52:00Z — OtherError (vendor 2): Hflash is busy now",
		},
		{
			// Faulted with errorCode NoError must still read as a fault.
			name: "status: faulted without error code",
			msg: &core.StatusNotificationRequest{
				ConnectorId: 2, ErrorCode: core.NoError, Status: core.ChargePointStatusFaulted,
				Timestamp: at("2026-07-23T03:19:56Z"),
			},
			want: "FAULT on connector 2 at 2026-07-23T03:19:56Z — Faulted",
		},
		{
			name: "boot: full identity",
			msg: &core.BootNotificationRequest{
				ChargePointVendor: "ZD", ChargePointModel: "FFCS",
				ChargePointSerialNumber: "A2111LHE0212606001", FirmwareVersion: "ZD-SW4.2.4.1",
				Iccid: "89460100175416217650", Imsi: "240017541621765",
				MeterSerialNumber: "1230601424,1231081118", MeterType: "LEM DCBM_v1,LEM DCBM_v1",
			},
			want: "Booted: ZD FFCS (serial A2111LHE0212606001) on firmware ZD-SW4.2.4.1 · meter LEM DCBM_v1,LEM DCBM_v1",
		},
		{
			name: "boot response",
			msg: &core.BootNotificationConfirmation{
				CurrentTime: at("2026-07-23T08:22:03Z"), Interval: 60, Status: core.RegistrationStatusAccepted,
			},
			want: "Boot Accepted — heartbeat every 60s (clock 2026-07-23T08:22:03Z)",
		},
		{
			name: "heartbeat is not an empty object",
			msg:  &core.HeartbeatRequest{},
			want: "Heartbeat",
		},
		{
			name: "diagnostics status",
			msg:  &firmware.DiagnosticsStatusNotificationRequest{Status: firmware.DiagnosticsStatusIdle},
			want: "Diagnostics upload: Idle",
		},
		{
			name: "meter values summarise the newest sample",
			msg: &core.MeterValuesRequest{
				ConnectorId: 1,
				MeterValue: []types.MeterValue{{
					Timestamp: at("2026-07-23T08:12:39Z"),
					SampledValue: []types.SampledValue{
						{Value: "150000.0", Context: types.ReadingContextSamplePeriodic, Measurand: types.MeasurandPowerOffered, Location: types.LocationOutlet, Unit: types.UnitOfMeasureW},
						{Value: "350.0", Context: types.ReadingContextSamplePeriodic, Measurand: types.MeasurandCurrentOffered, Location: types.LocationOutlet, Unit: types.UnitOfMeasureA},
					},
				}},
			},
			want: "Meter values on connector 1 — Power.Offered 150000.0 W @Outlet, Current.Offered 350.0 A @Outlet",
		},
		{
			name: "stop transaction names the signed data instead of dumping it",
			msg: &core.StopTransactionRequest{
				IdTag: "UNDEFINED", MeterStop: 1287200, TransactionId: 1011,
				Reason: core.ReasonLocal, Timestamp: at("2026-07-23T08:18:13Z"),
				TransactionData: []types.MeterValue{
					{Timestamp: at("2026-07-23T08:18:13Z"), SampledValue: []types.SampledValue{
						{Value: "OCMF|{...}|{...}", Format: types.ValueFormatSignedData, Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
					}},
					{Timestamp: at("2026-07-23T07:56:44Z"), SampledValue: []types.SampledValue{
						{Value: "150000.0", Measurand: types.MeasurandPowerOffered, Unit: types.UnitOfMeasureW},
					}},
				},
			},
			want: "Transaction 1011 stopped (Local) — meter 1287200 Wh, 2 meter sample(s) incl. signed OCMF data at 2026-07-23T08:18:13Z",
		},
		{
			// The FlexPole ships payment receipts through the vendor channel.
			name: "data transfer surfaces the receipt facts",
			msg: &core.DataTransferRequest{
				VendorId: "ZD", MessageId: "TransmitReceiptData",
				Data: `{"transactionId":1011,"receiptNr":"00000339","receiptTextBlock":"- Kundkvitto -","fee":12877,"meterSessionId":"aa6d16cf9d224e30489c75"}`,
			},
			want: "Vendor data from ZD: TransmitReceiptData — receipt 00000339 · tx 1011 · fee 128.77 (minor 12877)",
		},
		{
			name: "start transaction",
			msg: &core.StartTransactionRequest{
				ConnectorId: 1, IdTag: "UNDEFINED", MeterStart: 1261394, Timestamp: at("2026-07-23T07:43:39Z"),
			},
			want: "Start requested on connector 1 — idTag UNDEFINED, meter 1261394 Wh at 2026-07-23T07:43:39Z",
		},
		{
			name: "start transaction response",
			msg: &core.StartTransactionConfirmation{
				IdTagInfo: types.NewIdTagInfo(types.AuthorizationStatusAccepted), TransactionId: 1011,
			},
			want: "Transaction 1011 started (Accepted)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeFrame(tc.msg); got != tc.want {
				t.Errorf("describeFrame()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestDescribeFrame_UnknownTypeFallsBack proves an unhandled message still
// produces a usable line rather than an empty one.
func TestDescribeFrame_UnknownTypeFallsBack(t *testing.T) {
	if got := describeFrame(&core.ClearCacheConfirmation{}); got != "ClearCache received" {
		t.Errorf("fallback: got %q", got)
	}
}
