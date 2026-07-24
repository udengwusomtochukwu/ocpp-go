# PEAK250 hardware notes (vendor-doc extract + live calibration)

Dashboard-relevant facts about the PEAK250 (vendor platform: Elli FlexPole,
ZD hardware `AFC-200-LH-DE`, model FFCS), extracted 2026-07-23 from the two
vendor documents — *Initial Setup 01.2025* and *Workshop Manual 01.2025* —
and cross-checked against the lab's live data. Sources for every claim below
are those documents unless marked **measured**.

## Power & grid feed (Initial Setup, spec sheets)

- **`VWGC.ChargingStationCurrentImport` = 100000 + watts, read from the AC
  meter** — CONFIRMED by ZD email 2026-07-23 (was empirically decoded, now
  official). This is true grid-side AC power, i.e. the exact quantity a demand
  charge (effektavgift) bills on. The `Target` key is amps (63 = our feed).
  Stored raw in `meter_samples` (connector 0, `location=Inlet`); decoded to
  watts at read time everywhere.
- Two independent DC charging points (Gun A/B = OCPP connector 1/2), **one
  CCS Type 2 Combo connector each** → 2 OCPI EVSEs × 1 connector.
- **Dynamic current distribution (DPD)** — output scales with the mains feed:
  none → 100 kW/side (200 total) · 16 A → 120/220 · 32 A → 130/230 ·
  **63 A (ours) → 150 kW per side, 250 kW total**. Both guns cannot draw
  150 kW at once. Feed configurable in 1 A steps from SW 4.1.x.
- DC output: max 350 A @ 430 V / 188 A @ 800 V, 200–920 V DC. Efficiency max
  91 % at rated power. Feed 400 V 50 Hz; our 63 A ⇒ 43.6 kW ceiling
  (√3 · 400 · 63) — **measured** peak grid draw so far: 40.1 kW.
- ISO 15118 + DIN 70121 supported, but **Plug & Charge not available**.

## Buffer battery (Initial Setup, spec; calibration **measured**)

- **193.5 kWh nominal, 160 kWh maximum usable, SoC window 5–95 %.**
- Reported `SoC location=Body` reaches 100 %, consistent with the value being
  **normalized over the usable window** (pack SoC would cap at 95).
- **Calibration (2026-07-23, measured inlet vs ΔBody-SoC):** the one clean
  pure-refill hour so far (03:00–04:00 UTC, no deliveries, no post-session
  thermal load) gives **1.81 kWh grid-side per SoC %** ≈ 181 kWh/100 % —
  at ~90 % charge efficiency that is ≈ 163 kWh battery-side, matching the
  spec's 160 kWh usable. Post-session windows read higher (4–5 kWh/%) —
  polluted by thermal-management draw after heavy sessions, not by charging.
- Consequences for derived panels: the historical **218 kWh constant
  overstates by ~20 %**. "Boost energy available now" uses **160 kWh
  (spec usable)** as of 2026-07-23; the ΔSoC grid-draw *estimate* panels keep
  218 + an on-panel caveat until more clean refill windows land — the
  **measured inlet line is authoritative** for anything after 2026-07-22.
- After battery replacement or long storage the SoC may be wrong; the unit
  needs ~8 h of charging to recalibrate (Workshop Manual).
- **Forced SOC recalibration** is system-initiated (also schedulable via
  `VWGC.ForcedSocRecalibrationMode*`); the unit is **unavailable for charging**
  while it runs and the LEDs flash red. Explains idle `Unavailable` statuses.

## Status LEDs (Initial Setup §12.2 — four corner LEDs)

| Colour | Mode | Meaning |
|---|---|---|
| Blue | flashing | Starting up |
| Blue | continuous | Buffer charging / ready to charge a vehicle |
| Green | flashing | Charging a vehicle |
| Green | continuous | Available (idle / discharge complete) |
| Red | flashing | Forced SOC recalibration or not available (incl. outside operating hours) |
| Red | continuous | Not available — charging not possible |

No OCPP message carries LED state, but every input to this table exists in
our data (connector status, active tx, measured grid intake, recalibration
window) — an EVCMS `ledState` can be derived. Doc notes colours may vary by
firmware.

## Payments & receipts (Initial Setup — PosCtrlr / DataTransfer spec)

- **`PosCtrlr.PricePerKwh` and `PosCtrlr.PreAuthorizationAmount` are in minor
  currency units** ("150 equals 150 Eurocents"), officially confirming the
  lab's /100 conversion (499 → 4.99 SEK/kWh, 50000 → 500 SEK).
- Payment flow = pre-authorisation blocking; receipt `DataTransfer` carries
  `receiptNo` (0000–9999), `unitPrice` (minor units), totals — and when the
  backend Accepts it, the charger **expects a JSON response** with invoice
  fields: `invoiceNo`, `taxIdNo`, `nameOfIssuingEntity`, `addrOfIssuer`,
  `desc`, `discount` (0–1), `taxRate`. Our CS currently Accepts without these
  — the printed/stored receipt uses charger defaults. Wire them when the
  EVCMS receipt/invoice feed lands.
- Payment terminal on our unit: CCV IM15 (per `GlobalConfig`).

## Physical & environment (Initial Setup spec)

- 2260 × 1340 × 1100 mm, **2650 kg**, IP54 (battery IP67), IK10.
- Operating −25…50 °C; **no derating only up to 35 °C** at the DC output —
  expect summer derating; it has active thermal management (the post-session
  grid draw the calibration excludes).
- Doors are part of the safety loop — opening one causes a safety shutdown
  (another benign source of `Unavailable`).

## Still unanswered (ask ZD)

- ✅ **RESOLVED 2026-07-23:** `VWGC.ChargingStationCurrentImport` = 100000 +
  watts from the AC meter (see Power & grid feed above).
- Whether reported Body SoC is usable-window-normalized (calibration says
  yes; get it confirmed).
- Why the 4.2.4.1 OTA reset `VWGC.ChargeGunPlugCycleCounters` (91,54 → 0,0).
