#!/usr/bin/env python
"""Incident report -> Word (.docx) in the HYDE house style (matches the
Charger Connection Guide: HYDE wordmark, centered caps title, lettered
sections, clean grey-header tables, black-on-white)."""
from docx import Document
from docx.shared import Pt, RGBColor, Inches
from docx.enum.text import WD_ALIGN_PARAGRAPH
from docx.enum.table import WD_TABLE_ALIGNMENT
from docx.oxml.ns import qn
from docx.oxml import OxmlElement

GREY   = RGBColor(0x8C, 0x8C, 0x8C)
DKGREY = RGBColor(0x59, 0x59, 0x59)
BLACK  = RGBColor(0x1A, 0x1A, 0x1A)
RED    = RGBColor(0xB0, 0x2A, 0x2A)
HDR_FILL = "ECECEC"
BORDER  = "C7C7C7"
BODY_FONT = "Calibri"

doc = Document()
# page + default font
for s in ("Normal",):
    st = doc.styles[s]; st.font.name = BODY_FONT; st.font.size = Pt(10.5); st.font.color.rgb = BLACK
sec = doc.sections[0]
sec.top_margin = Inches(0.9); sec.bottom_margin = Inches(0.9)
sec.left_margin = Inches(1.0); sec.right_margin = Inches(1.0)

def sp(p, before=0, after=6, line=1.28):
    pf = p.paragraph_format
    pf.space_before = Pt(before); pf.space_after = Pt(after); pf.line_spacing = line

def run(p, text, *, bold=False, size=10.5, color=BLACK, font=BODY_FONT, caps=False, spacing=None):
    r = p.add_run(text.upper() if caps else text)
    r.bold = bold; r.font.size = Pt(size); r.font.color.rgb = color; r.font.name = font
    if spacing is not None:  # expanded letter-spacing, twentieths of a pt
        rPr = r._element.get_or_add_rPr(); el = OxmlElement('w:spacing'); el.set(qn('w:val'), str(spacing)); rPr.append(el)
    return r

def set_cell_bg(cell, hexc):
    tcPr = cell._tc.get_or_add_tcPr(); shd = OxmlElement('w:shd')
    shd.set(qn('w:val'), 'clear'); shd.set(qn('w:color'), 'auto'); shd.set(qn('w:fill'), hexc); tcPr.append(shd)

def table_borders(table, color=BORDER, sz="4"):
    tblPr = table._tbl.tblPr; b = OxmlElement('w:tblBorders')
    for edge in ('top','left','bottom','right','insideH','insideV'):
        e = OxmlElement(f'w:{edge}')
        e.set(qn('w:val'),'single'); e.set(qn('w:sz'),sz); e.set(qn('w:space'),'0'); e.set(qn('w:color'),color); b.append(e)
    tblPr.append(b)

def cell_text(cell, text, *, bold=False, size=9.5, color=BLACK, mono=False):
    cell.text = ""; p = cell.paragraphs[0]; sp(p, 1, 1, 1.15)
    run(p, text, bold=bold, size=size, color=color, font=("Consolas" if mono else BODY_FONT))

def section(letter, title):
    p = doc.add_paragraph(); sp(p, 16, 7)
    run(p, letter + "  ", bold=True, size=12, color=GREY)
    run(p, title, bold=True, size=12, color=BLACK)

def body(text, after=6):
    p = doc.add_paragraph(); sp(p, 0, after); run(p, text); return p

def bullet(lead, rest):
    p = doc.add_paragraph(style="List Bullet"); sp(p, 0, 3, 1.2)
    if lead: run(p, lead, bold=True)
    run(p, rest)

def labeled(lead, rest):
    p = doc.add_paragraph(); sp(p, 4, 4)
    run(p, lead + "  ", bold=True); run(p, rest)

# ---- wordmark ----
p = doc.add_paragraph(); sp(p, 0, 2)
run(p, "HYDE", bold=True, size=26, color=GREY, font="Century Gothic", spacing=80)

# ---- title ----
p = doc.add_paragraph(); p.alignment = WD_ALIGN_PARAGRAPH.CENTER; sp(p, 22, 3)
run(p, "PEAK250 CHARGER — INCIDENT REPORT", bold=True, size=14, color=BLACK)
p = doc.add_paragraph(); p.alignment = WD_ALIGN_PARAGRAPH.CENTER; sp(p, 0, 14)
run(p, "Unit A2111LHE0212606001  ·  Firmware ZD-SW4.2.4.1  ·  2026-07-26  ·  Severity: Critical", size=9.5, color=DKGREY)

body("On 2026-07-26 the PEAK250 charger raised the same station-level hardware fault — "
     "HCU Precharge Module Fault_0x27 (vendor code 012900) — three times within 41 minutes, "
     "each while a customer was charging. Service was restored by a remote hard reset, but the fault "
     "is a degrading hardware condition that recurs on charging and requires vendor inspection. Times "
     "are shown in UTC with site-local CEST (UTC+2) in parentheses.", after=4)

# ---- A Summary ----
section("A", "Summary")
for lead, rest in [
    ("Three occurrences — ", "09:14:47, 09:27:27 and 09:55:08 UTC (11:14, 11:27, 11:55 CEST); the same fault each time."),
    ("Station-level — ", "reported on OCPP connector 0; it forces both guns to Faulted and aborts the active Gun 1 (CCS) session."),
    ("During charging — ", "all three fired mid-session at 52–75 kW, not at plug-in, and under normal load (no overload)."),
    ("Accelerating onset — ", "time-to-fault fell 2 min 40 s → 40 s → 30 s across the three attempts — a degrading-component signature."),
    ("Separate, isolated — ", "a SECC sequence error (vendor 163200) occurred at 06:06 UTC on Gun 2 and did not recur."),
]:
    bullet(lead, rest)

# ---- B Timeline ----
section("B", "Incident timeline")
body("Reconstructed from the central-system durable fault log, the TimescaleDB meter store, and Loki.", after=6)
rows = [
    ("Time (UTC / CEST)", "Session", "Charging before fault", "Fault (vendor code)", "Outcome"),
    ("09:14:47\n11:14:47", "tx 1021, Gun 1", "2 min 40 s · peak 75 kW · SoC 38→42%", "HCU Prechrg Module Fault_0x27 (012900)", "Both guns Faulted; session stopped reason “Other”; ~11 min recovery"),
    ("09:27:27\n11:27:27", "tx 1022, Gun 1", "~40 s · peak 80 kW · same EV (SoC 42%)", "HCU Prechrg Module Fault_0x27 (012900)", "Both guns Faulted; session stopped reason “Other”"),
    ("09:55:08\n11:55:08", "tx 1023, Gun 1", "~30 s · peak 52 kW", "HCU Prechrg Module Fault_0x27 (012900)", "Both guns Faulted; new customer session aborted"),
    ("06:06:45\n08:06:45", "— (Gun 2 idle)", "n/a", "SECC detect the sequence error (163200)", "Isolated; did not recur"),
]
t = doc.add_table(rows=len(rows), cols=5); t.alignment = WD_TABLE_ALIGNMENT.CENTER; table_borders(t)
t.columns[0].width = Inches(0.95); t.columns[1].width = Inches(0.95); t.columns[2].width = Inches(1.7); t.columns[3].width = Inches(1.6); t.columns[4].width = Inches(1.9)
for j, h in enumerate(rows[0]):
    c = t.rows[0].cells[j]; set_cell_bg(c, HDR_FILL); cell_text(c, h, bold=True, size=8.5, color=DKGREY)
for i in range(1, len(rows)):
    for j, val in enumerate(rows[i]):
        cell_text(t.rows[i].cells[j], val, size=9, mono=(j == 0))
body("For contrast, earlier sessions the same morning completed normally — tx 1019 (Gun 1, 08:09 CEST, "
     "147 kW peak, ~35 min) and tx 1020 (Gun 2, 10:53 CEST). The fault is intermittent, not a hard failure.", after=4)

# ---- C Root cause ----
section("C", "Root-cause analysis")
body("Four independent observations point to a defective, degrading component on the shared DC power stage.", after=6)
for lead, rest in [
    ("It downs both guns → shared component.", "Reported on connector 0 (the station), it forces both charging points to Faulted. In the battery-buffered design both guns draw from one shared DC stage — consistent with the precharge module sitting on that shared bus."),
    ("It fires during charging, not at plug-in.", "Precharge normally runs once at session start. Here it faulted 2.5 minutes in, with 62 kW already flowing — suggesting continuous precharge-circuit monitoring, or a mid-session precharge re-run (e.g. on buffer↔grid source switching)."),
    ("It occurs under normal conditions.", "At fault time the car drew 62–75 kW, buffer SoC was 94–100%, grid infeed ~40 kW — within rating. Nothing was overloaded, which argues for a hardware defect, not an operating-condition trip."),
    ("Onset accelerates on each attempt.", "Time-to-fault fell 2 min 40 s → 40 s → 30 s — the classic signature of thermal or wear-driven degradation, not a one-off transient."),
]:
    labeled(lead, rest)
p = doc.add_paragraph(); sp(p, 6, 4)
run(p, "Working hypothesis: ", bold=True, color=RED)
run(p, "a failing precharge module (contactor / resistor / DC-link monitoring) on the shared DC stage, "
       "degrading under repeated load. The meaning of sub-code 0x27 / vendor 012900 requires ZD's confirmation.")

# ---- D Customer impact ----
section("D", "Customer impact")
body("Three charging attempts were aborted mid-session. Signed OCMF meter data exists for each, so billing is defensible.", after=6)
bullet("tx 1021 — ", "2.37 kWh delivered before abort; receipt #00000368.")
bullet("tx 1022 — ", "0.20 kWh delivered (same driver retried); receipt #00000370.")
bullet("tx 1023 — ", "a new customer, ~30 s of charging before abort.")
body("Each session ended with StopTransaction reason “Other” (fault-terminated), not “Local”. Affected "
     "drivers should be refunded to actual energy delivered and their pre-authorization holds released.", after=4)

# ---- E Response ----
section("E", "Response and remediation")
labeled("Remote hard reset — service restored.", "Issued from our backend at 09:59:43 UTC and accepted; the unit power-cycled and returned with both guns Available and no fault at 10:04 UTC (~5 min). A soft reset was avoided — it restarts only the application, not the hardware fault.")
labeled("Remote take-out-of-service capability added.", "The incident exposed that we could not remotely fence off faulty hardware. We wired the OCPP ChangeAvailability operation into the backend (POST /chargers/{id}/commands/change-availability, Operative / Inoperative), so a faulting unit can now be marked Inoperative remotely.")
labeled("Recommended: take the unit out of service.", "The reset clears the fault state but not the cause. Until ZD inspects, the unit (or at least Gun 1) should be set Inoperative so arriving customers see “unavailable” rather than a failed charge and a pre-auth hold.")

# ---- F Vendor ----
section("F", "For the vendor (ZD Energy)")
body("Fault codes observed on unit A2111LHE0212606001, firmware ZD-SW4.2.4.1.", after=6)
fr = [
    ("Vendor code", "Text", "OCPP errorCode", "Occurrences"),
    ("012900", "HCU Prechrg Module Fault_0x27", "OtherError", "3× (09:14, 09:27, 09:55 UTC), all mid-session on Gun 1"),
    ("163200", "SECC detect the sequence error", "OtherError", "1× (06:06 UTC), Gun 2, isolated"),
]
t2 = doc.add_table(rows=len(fr), cols=4); t2.alignment = WD_TABLE_ALIGNMENT.CENTER; table_borders(t2)
t2.columns[0].width = Inches(0.9); t2.columns[1].width = Inches(2.5); t2.columns[2].width = Inches(1.1); t2.columns[3].width = Inches(2.5)
for j, h in enumerate(fr[0]):
    c = t2.rows[0].cells[j]; set_cell_bg(c, HDR_FILL); cell_text(c, h, bold=True, size=8.5, color=DKGREY)
for i in range(1, len(fr)):
    for j, val in enumerate(fr[i]):
        cell_text(t2.rows[i].cells[j], val, size=9, mono=(j == 0))
p = doc.add_paragraph(); sp(p, 10, 4); run(p, "Questions for ZD:", bold=True)
for q in [
    "What does 012900 / HCU Prechrg Module Fault_0x27 indicate specifically — which component (precharge contactor, resistor, DC-link monitoring)?",
    "Why would a precharge fault trigger minutes into an active session, long after precharge completes? Is precharge re-run mid-session (e.g. on buffer↔grid source switching)?",
    "Why does the fault take both charging points down — is the precharge module shared across the two DC outputs?",
    "Does the accelerating onset (2 min 40 s → 40 s → 30 s) indicate a degrading / thermal condition versus a hard failure? Is the unit safe to keep in service?",
    "Recommended action — inspection or part replacement?",
    "Separately: is 163200 (SECC sequence error) on Gun 2 related, or an independent vehicle-handshake issue?",
]:
    bullet("", q)

# ---- footer ----
p = doc.add_paragraph(); sp(p, 20, 0)
pf = p.paragraph_format
run(p, "hydecharge-ocpi · ocpp lab  —  Sources: central-system durable fault log · TimescaleDB meter store · Loki  —  generated 2026-07-26",
    size=8, color=GREY)

out = r"C:\Users\user\Documents\ocpp-go\deploy\ocpp-lab\incidents\2026-07-26-precharge-fault.docx"
doc.save(out)
print("saved:", out)
