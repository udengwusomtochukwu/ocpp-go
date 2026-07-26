# HYDE dashboard — improvement notes

Running list of dashboard ideas surfaced while reading the OCPP 1.6J spec
against our code. Each item is written to be turnkey later. Status legend:
💡 idea · 🔨 ready to build · ✅ done.

---

## 1. Charger supported features (advertised profiles) 🔨

**What:** show, at any time, which OCPP 1.6 feature profiles the charger
advertises — Core, FirmwareManagement, LocalAuthListManagement, Reservation,
RemoteTrigger, SmartCharging.

**Why:** nice protocol-truth info; tells operators/roaming partners what the
unit can actually do. Our PEAK250 advertises *Core, FirmwareManagement,
LocalAuthListManagement, Reservation, RemoteTrigger* — notably **not**
SmartCharging, even though it exposes smart-charging config keys. That gap is
exactly the kind of thing this panel makes visible.

**Data path (already captured):** the charger reports it in the
`SupportedFeatureProfiles` config key. On every connect the CS runs a
full-config prefetch (`syncGetConfiguration(id, nil, …)`,
central_system_sim.go:250) → stored in `st.config` → served in
`GET /api/chargers/{id}` under `config`. So the data is in the CS on every
connect; it is simply not exported as a metric.

**Implementation sketch:**
- In the prefetch path (or a small helper), parse `SupportedFeatureProfiles`
  (comma-separated) and emit a per-profile info gauge:
  `ocpp_charger_profile{profile="Core", mode="live"} 1` — one series per
  advertised profile. Zero-on-change like `recordChargerInfo` so a firmware
  update that changes the set doesn't leave stale series. Cardinality is
  ≤7 profiles × 2 modes = ≤14 series — safe.
- Grafana: a "Supported features" panel on Operations (or the Home Right-now
  row) — a stat row or bar gauge, one cell per profile, green = advertised.
- **Bonus (capability vs. usage):** overlay what we actually *exercise* from
  `ocpp_command_results_total` (feature → profile) to show advertised vs used
  side by side. This is the conformance strip from note #4.
- Alternative without a new metric: a Grafana **Infinity** datasource could
  read the REST `config` array directly — but Infinity isn't installed
  (we have prometheus/postgres/loki/jaeger), so the metric route is cleaner.

---

## 2. Control-plane coverage panel 💡

**What:** a live matrix of which CS-initiated OCPP ops have been exercised and
their accept rate. **Why:** shows the backend's control surface at a glance and
proves remote control works. **Data:** already emitted as
`ocpp_command_results_total{status}` — add the feature name as a label and
group by it. Gate the demo routine (note #6) first so phantom
ReserveNow/CancelReservation stop polluting the counts.

## 3. Firmware-management lifecycle timeline 💡

**What:** a state-timeline of the OTA lifecycle — Downloading → Downloaded →
Installing → Installed → reboot → new version. **Why:** we handle both
`FirmwareStatusNotification` and `DiagnosticsStatusNotification` but only show
the current version string; the real OTA we ran (4.2.4 → 4.2.4.1) is invisible
as a process. **Data:** the `FirmwareStatusNotification` events are already on
the bus and in Loki (and the OCPP Messages dashboard); a dedicated
state-timeline panel keyed on the status field turns them into a story.

## 4. Capability / conformance strip 💡

**What:** advertised profiles (note #1) vs. exercised ops (note #2), one
compact panel. **Why:** it's the protocol-truth view this whole analysis keeps
producing — what the charger *claims*, what we *use*, and where they diverge
(e.g. SmartCharging not advertised but config keys present).

## 5. Frame the Messages `dir` filter as initiator direction 💡

**What:** on HYDE · OCPP Messages, label the existing `dir=in/out` filter as
"CP-initiated (in) vs CS-initiated (out)". **Why:** that *is* what it is —
every OCPP message has a fixed initiator, and the filter splits the protocol
along that axis. Makes the message log a teaching tool. Docs-only change.

---

## 6. Correctness: gate the leftover demo routine 🔨

**Not a dashboard panel, but it pollutes dashboard truth.**
`central_system_sim.go:246` runs `go exampleRoutine(id, handler)` on every
charge-point connect — upstream ocpp-go sample code that fires a scripted
sequence at whatever connects: ReserveNow(connector 1, idTag `l33t`, id 42) →
CancelReservation → GetLocalListVersion → ChangeConfiguration
(`MeterValueSampleInterval=10`) → TriggerMessage(Heartbeat) →
TriggerMessage(DiagnosticsStatusNotification). Against the real PEAK250 this
reserves a connector and rewrites the meter-sample interval on **every**
reconnect, and puts synthetic reservations into the message log and command
stats. Fix: guard `if !isLive(id) { go exampleRoutine(id, handler) }` (we
already have the allowlist) or delete it. Do this before notes #1–#2 so the
new capability/coverage panels aren't reading demo noise.
