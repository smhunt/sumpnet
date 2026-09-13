# The bucket test: measuring a home's sump pump flow rate and pit area

Research report for sumpnet (Timberwalk pilot). Prepared 2026-09-12. Read-only with respect to the repo.

Evidence labels used throughout:
- **[sourced]** means the claim is stated in a cited source (strength noted in the Sources list).
- **[estimate]** means it is my own engineering calculation or judgement. The inputs are shown so it can be checked.

---

## 1. Answer in brief

- **The node has no optical monitor.** The §4 hardware table specifies a **JSN-SR04T waterproof ultrasonic** (sonar) level sensor. It also has an SCT-013 CT clamp on the pump cord, a float switch for high water, and a BME280. The ultrasonic sensor measures distance to the water surface, and the CT clamp shows exactly when the pump starts and stops. Together they can do everything the owner's "optical monitor" would have done, and they do it better than eyes and a stopwatch.
- **The idea is right in spirit, but "dump one bucket and time until shut-off" does not by itself measure pump rate.** A stopwatch run time converts to a flow rate only if you know how many litres the pump removed. The pump removes the bucket *plus* whatever water was already above the float's OFF level, plus inflow and drain-back. That extra volume is unknown, and for a typical 18" basin it can be anywhere from 0 to 2 buckets' worth. It is also possible for one 18.9 L bucket to fail to start the pump:
  - It usually won't start the pump from the OFF level in a 24" basin.
  - It won't in an 18" basin with a tethered, electronic or long-stroke vertical switch.
- **What a known volume of water really gives sumpnet is the pit's effective area (litres per mm of rise).** sumpnet does not need a stopwatch for pump rate. Every fPort 2 cycle already gives the drawdown speed in mm/s. Phase 4 already turns that into `pump_rate_lps = pit_area × median(drop ÷ run_s)`. The weak link is `pit_area_m2`, which is currently a nominal figure such as "18 inch = 0.164 m²". I estimate that nominal figure could be 10–40 % high:
  - The pump body, the switch, the discharge pipe and any backup pump take up space in the pit.
  - Many basins taper toward the bottom.

  A bucket calibration of effective area fixes the pump rate, `inflow_est_l` and the §9 `volume_l` floor all at once.
- **The fixed procedure** is to pour *measured* volumes, starting right after the pump shuts off, and read the rise in mm on the node. That gives the effective area. Then keep pouring until the pump starts and let the node time the run. That gives a measured pump rate, which cross-checks the learned one. Repeat three times.
- **Professionals use the drawdown test.** Utilities time a known drop in a wet well of known area and add the separately measured inflow rate [sourced: RCAP, PumpEd101, a utility start-up form]. Sump installers measure inflow by timing the level rise with the pump off [sourced: WATERPROOF! Magazine]. A bucket at the discharge outlet is a legitimate method for small flows, but a 19 L bucket fills in only 4–15 s at sump-pump rates, which makes it hard to time accurately [sourced: GTK, UGA extension].
- **Recommendation (proposal):** keep the learned pump rate, which follows pump wear, and let a bucket test set the **effective area**. This is the main change I'd suggest to what migration 0006 currently reserves: there, `bucket_test` is a *rate* that "takes precedence" over the learned rate.

---

## 2. What sensor the node has, and what "optical" would mean

From `prompt_plan.md` §4:

| Function | Part | Relevance to the bucket test |
|---|---|---|
| Pit level | **JSN-SR04T waterproof ultrasonic** | Measures sensor-to-water distance (`level_mm`), so a rise of the water is a *decrease* in `level_mm`. |
| Pump running | SCT-013 CT clamp on a plug-through splitter | Gives exact pump start and stop times, and so `run_s`. |
| High water | Float switch | Independent alarm only. |
| Ambient | BME280 | Temperature, which can be used to correct the speed of sound. |

Relevant JSN-SR04T characteristics (the vendor manual is a medium-strength source; the original datasheet link returned HTTP 403):
- **Range and blind zone:** about 25–450 cm, with a **25 cm blind zone** [sourced].
- **Accuracy:** about **±1 cm** typical [sourced].
- **Beam:** wide, about 45–75°. "Readings jump around over a water surface" and the beam "returns the nearest thing". The suggested remedies are median filtering, perpendicular aiming, keeping the beam clear of walls and obstacles, or a stilling tube [sourced].
- **Ping interval:** at least ~60 ms between pings, so roughly 15 Hz at most [sourced].
- **Temperature:** the speed of sound in air is about 331.3 + 0.606·T m/s [sourced: Wikipedia]. That is about **0.18 % per °C**. A fixed temperature error shifts both ends of a cycle, so it scales the measured *drop* by the same 0.18 %/°C; with the BME280 this is easy to correct [estimate].

**Would an optical (time-of-flight or lidar) sensor be better?** Not for this job.
- ST publishes an application note on liquid-level sensing with its VL53L4CD time-of-flight sensor (AN5851). I could not retrieve its text (the download timed out), so I make no claim about its content.
- Practitioner reports say time-of-flight sensors are unreliable on water surfaces because of reflections. One reader reports "for water the HC04 [ultrasonic] is way more better" [sourced, **weak**: a blog reader comment].
- Clear water, ripples, and condensation on the lens in a humid pit are the obvious failure modes [estimate].

Recommendation: stay ultrasonic.

---

## 3. The techniques

Notation:
- *A* = effective pit area at the working water level (m²)
- *Δh* = level change (mm)
- *t* = time (s)
- *Q_p* = pump flow (L/s)
- *Q_in* = inflow (L/s)

Useful unit: with *A* in m² and *Δh* in mm, *A* × *Δh* gives litres directly (1 m² × 1 mm = 1 L).

### 3.1 Pump-down (drawdown) timing — the professional standard

**Procedure** (lift-station practice) [sourced: RCAP, PumpEd101, HNWS form]:
1. **Measure inflow first.** With the pump off, time the rise *R* over a set interval. That rise rate times the wet-well area is the influent rate.
2. **Measure the drawdown.** Run the pump and time the fall *F* over a known distance. PumpEd101 prefers one foot "because there is very little change in flow for most pumps over such a small elevation change".
3. **Combine them.** "Add the influent rate to the calculated drawdown rate to calculate the gallons per minute pump rate" [sourced: RCAP].
4. **Repeat** several times. The draw-down method "can be very reliable as long as the data collected is accurate and the test is repeated several times" [sourced: PumpEd101].
5. **Compare with the curve.** Efficiency % = measured rate ÷ rated capacity × 100 [sourced: RCAP]. Test with the discharge pipe full, so the head matches normal operation [sourced: Romtec].

**Formula:**

> *Q_p* = *A* × (*Δh_down* / *t_down*) + *Q_in*,  where *Q_in* = *A* × (*Δh_up* / *t_up*) measured with the pump off.

RCAP's version for a circular well of diameter *D* in feet: influent = 0.785·D²·R·7.48 gpm; drawdown = 0.785·D²·F·7.48 gpm; pump rate = drawdown + influent.

The utility start-up form (HNWS Form 3.5 Part 4) computes the flow as "gallons per inch × drawdown inches ÷ drawdown minutes". It also records discharge pressure to estimate the total dynamic head (TDH) [sourced]. PumpEd101 warns that a leaking check valve made a station look 12 % slow (350 vs 400 gpm) when the head was actually normal. That is why flow and head should both be measured [sourced].

**Relevance to sumpnet:** this *is* what an fPort 2 cycle reports (`level_start_mm`, `level_end_mm`, `run_s`). The only missing input is *A*.

### 3.2 Fill-rate timing (pour or natural inflow, pump off)

Installers use the natural-inflow version to size pumps and basins. Run the pump down to its shut-off level, then "wait for one minute with the pump off, and then measure how far the water rose during that minute". Rules of thumb: in an 18" basin, 1 inch ≈ 1 gallon; in a 24" basin, 1 inch ≈ 2 gallons [sourced: WATERPROOF!]. My check: 1.10 gal/in and 1.96 gal/in [estimate]. More than 30 gpm of inflow calls for a 24" basin [sourced].

With a *known poured volume V* instead of natural inflow, the same measurement gives the area:

> *A* = (*V* − *Q_in*·*t*) / *Δh*

This is the calibration sumpnet actually needs.

### 3.3 Bucket at the discharge outlet

Catch the whole discharge in a container of known volume and time the fill: *Q_p* = *V* / *t_fill* [sourced: UGA extension, GTK]. Recommendations and limits:
- **Repeat.** UGA says at least 3 runs; GTK says 5–7, and the spread between runs indicates the accuracy [sourced].
- **Flow limit.** The method suits flows up to about 5–6 L/s (GTK) or 5–15 L/s (UGA). "Accuracy can suffer if the filling happens too fast, especially if it takes only some seconds" [sourced: GTK].
- **Sump-pump fill times.** Sump pumps deliver about 1.2–4.5 L/s at real heads (§7), so an 18.9 L bucket fills in about 4–16 s. Human reaction time of ±0.2–0.3 s at each end gives about ±3–8 % per trial [estimate].
- **Practical obstacles.** The outlet has to be accessible and not buried, connected to a storm lateral, or iced. Splashing and overflow losses bias the result low [estimate]. I found no source on whether Timberwalk discharges to the surface or to a storm connection; that is **to confirm with the owner**.
- **Source quality.** Consumer sites describe timing a 1-gallon bucket at the discharge [**weak**: sumppumpgurus]. At 30–70 gpm a 1-gallon bucket fills in about 1–2 s, which is not usable.

### 3.4 Which one professionals use

- **Wastewater and lift-station operators:** drawdown with an inflow correction, repeated, often with a pressure reading (RCAP, PumpEd101, HNWS). PumpEd101 calls drawdown "the standard pump test technique" for small stations without flow meters [sourced].
- **Residential sump installers:** a fill-rate (inflow) measurement for sizing, then the pump is picked from its curve [sourced: WATERPROOF!]. I found no evidence that installers routinely measure a residential pump's actual flow.
- **Homeowner guidance** only checks that the pump works. Public Safety Canada: "Pour water into the sump pit to confirm that the pump activates and directs the water out… Perform a full pump test once a year" [sourced]. An Edmonton restoration firm says pour 15–20 L [**weak**].

---

## 4. Does one 5-gallon (18.9 L) bucket trigger a cycle?

### 4.1 Rise per bucket

| Basin | Nominal area | Rise per 18.9 L (nominal area) | Rise with pump, switch and pipe displacement [estimate] |
|---|---|---|---|
| 18" (457 mm) | π/4 × 0.4572² = **0.1642 m²** | 18.93 / 0.1642 = **115 mm (4.5")** | net area ≈ 0.80 × nominal → **144 mm** |
| 24" (610 mm) | π/4 × 0.6096² = **0.2919 m²** | 18.93 / 0.2919 = **65 mm (2.6")** | net area ≈ 0.88 × nominal → **74 mm** |

Displacement estimate: a 1/3 hp cast-iron pump is about 10–11.5" tall on a ~9¾" base (Wayne's manual gives 11-1/2" × 9-3/4") [sourced]. Its motor housing, vertical switch and 1-1/2" riser together occupy roughly 0.02–0.04 m² in the band between the OFF and ON levels. That is about 12–25 % of an 18" basin and 7–14 % of a 24" basin. A second (backup) pump in the same basin roughly doubles it [estimate].

Tapered basins make the effective area smaller still. Tapered-wall 18" × 24" basins are sold [sourced, **weak**: retailer listings]. One is listed at "21 gallons to lid", against 26.4 gal for a straight 18" × 24" cylinder. If that shortfall were all taper, the bottom would be about 14" across, giving roughly 0.11–0.12 m² in the 3–9" band, 25–35 % below nominal [estimate, weak inference]. The Jackel SF22A lists only an 18" ID, 24" height and 22 gal [sourced], so a straight wall can't be ruled out either.

**Conclusion [estimate]:** the true litres per mm in a pilot home could be anywhere from about 0.6× to 1.0× the §9 nominal figure. That uncertainty flows 1:1 into `pump_rate_lps`, `inflow_est_l` and `volume_l`.

### 4.2 Float on/off differentials (manufacturer data)

| Switch | ON / OFF | Differential | Source |
|---|---|---|---|
| Liberty 257 (vertical magnetic, fixed) | 7" / 4" | 3.0" = 76 mm | Liberty manual, Table 1 [sourced] |
| Zoeller M53 (integral vertical float) | 7-1/4" / 3" | 4.25" = 108 mm | Zoeller FM2778 [sourced] |
| Wayne CDU series, vertical | ~9" / ~4" | 5.0" = 127 mm | Wayne manual [sourced] |
| Liberty 287 / 297 (vertical) | 9.5" / 4" | 5.5" = 140 mm | Liberty Table 1 [sourced] |
| Wayne CDU series, tethered | ~13" / ~7" | 6.0" = 152 mm | Wayne manual [sourced] |
| Ion electronic switch | 6" range | 152 mm | retailer listing [**weak**] |
| Zoeller M98 (integral vertical float) | 9-1/2" / 3" | 6.5" = 165 mm | Zoeller FM2779 [sourced] |
| Liberty FL-series (tethered wide-angle) | 12–15" / 5–8" | 7" = 178 mm | Liberty Table 1 [sourced] |
| LevelGuard electronic | min. differential 6" (150 mm); "effective differential 8" to 10" (200–254 mm)" | 150–254 mm | LevelGuard product overview [sourced] |
| Generic tethered, adjustable | pumping range 7–36" | tether length sets it | rcworst [**weak**]; Boshart: "increasing the tether length increases the pumping range" [sourced] |

### 4.3 Buckets needed to go from the OFF level to the ON level

The volume between the OFF and ON levels is area × differential. The table gives it on the nominal area, then with the §4.1 net-area factors (0.80 for 18", 0.88 for 24") [estimate].

| Switch (differential) | 18": litres, nominal / net | 18": buckets (net) | 24": litres, nominal / net | 24": buckets (net) |
|---|---|---|---|---|
| Liberty 257 (76 mm) | 12.5 / 10.0 L | **0.53** | 22.2 / 19.6 L | **1.03** |
| Zoeller M53 (108 mm) | 17.7 / 14.2 L | **0.75** | 31.5 / 27.7 L | **1.46** |
| Wayne vertical (127 mm) | 20.8 / 16.7 L | **0.88** | 37.1 / 32.6 L | **1.72** |
| Wayne tethered / Ion (152 mm) | 25.0 / 20.0 L | **1.06** | 44.5 / 39.1 L | **2.07** |
| Zoeller M98 (165 mm) | 27.1 / 21.7 L | **1.15** | 48.2 / 42.4 L | **2.24** |
| Liberty FL tethered (178 mm) | 29.2 / 23.4 L | **1.23** | 51.9 / 45.7 L | **2.41** |
| LevelGuard at 10" (254 mm) | 41.7 / 33.4 L | **1.76** | 74.1 / 65.2 L | **3.45** |

**Reading the table:**
- **From the OFF level,** one bucket starts the pump only in an 18" basin with a short-stroke vertical float (Liberty 257, Zoeller M53, Wayne vertical). In a 24" basin, or with any tethered or electronic switch, it takes about 1.1–3.5 buckets.
- **From somewhere between OFF and ON** (the usual case, since the owner doesn't know where the level is), one bucket often starts the pump. But then the pump removes **the bucket plus 0–100 % of the differential volume that was already there.** That extra is 0–23 L in an 18" basin and 0–46 L in a 24" basin with a common switch, i.e. 0–2.4 buckets.

### 4.4 What "dump a bucket and time until shut-off" actually measures

Water balance from the moment of pouring to shut-off:

> *Q_p* × *t_run* = *V_bucket* + *A*·(*h_0* − *h_off*) + *Q_in*·*t_total* + *V_pipe_refill*

- *h_0* is the level when you started pouring.
- *V_pipe_refill* is the discharge pipe the pump has to refill if it drained back (§5.2).

The owner's shortcut *Q_p* ≈ *V_bucket* / *t_run* is correct only if all of these hold:
1. The level starts exactly at the OFF level (*h_0* = *h_off*).
2. The bucket alone is enough to start the pump.
3. Inflow and pipe refill are negligible.
4. The water poured after the pump starts is counted correctly.

Otherwise the stopwatch time is just the run time for the level band that happened to be full: the **ON-to-OFF band plus whatever was poured above it**, at the pump's net rate. The node already records exactly that, every cycle, as `run_s`. With the unknown *A*·(*h_0* − *h_off*) term, the shortcut **underestimates** *Q_p*. The error ranges from 0 up to about 55 % (18" M53: 14 L extra on top of 18.9 L) or more (24" basin) [estimate].

### 4.5 How to fix the procedure

1. **Start at the OFF level.** Wait for the pump to finish a cycle, or pour until it runs once and wait until it stops. Then wait about 2 minutes for drain-back and ripples to settle.
2. **Pour measured volumes that stay below the ON level,** and read the rise of each. This gives the effective area *A* = *V* / *Δh* directly. For example:
   - 18" basin: 4 L jugs (about 24–30 mm each), 2 or 3 of them.
   - 24" basin: a half or full bucket (about 33–74 mm).
3. **Then pour until the pump starts, and stop pouring at once.** Everything poured since the OFF level is now above the OFF level. The pump returns the level to OFF, so the volume it removed = total poured + inflow + pipe refill. That gives *Q_p* = (*V_total* + *Q_in*·*t* + *V_pipe*) / *t_run*. The node computes the same thing independently as *A* × drawdown slope + inflow.
4. **Or time only the pump-down** (the drawdown method). That needs *A*, which step 2 provided.
5. **Repeat three times** and accept the result if the runs agree within about ±10 %.

---

## 5. Error budget

| Error source | Mechanism | Magnitude | Mitigation |
|---|---|---|---|
| **Inflow during the test** | Water entering the pit during a pour or run adds to the rise and slows the drawdown. | Dry weather: 0.1–1 % of pump rate [estimate from the simulator's 1–12 cycles/day × 15–25 L; ADR 0007 says "well under 1 %"]. Wet weather: 10–100 % (WATERPROOF! cites > 30 gpm inflow, comparable to pump output) [sourced range + estimate]. | Test only after 72 h dry. Measure the pre-pour fill slope and add it (RCAP rule). The node can log the slope. |
| **Check-valve backflow / pipe drain-back** | After the pump stops, water above the pump drains back into the pit, and the pump must refill the pipe on the next run. | Sch 40 1-1/2" (ID 1.610" = 40.9 mm) holds **1.31 L/m**; 1-1/4" (ID 1.380" = 35.1 mm) holds **0.97 L/m** [sourced IDs; volumes estimated]. **Good check valve** (~0.3 m of pipe below it): ~0.4 L plus closing leakage ≈ 0.4–0.7 L = **2–4 %** of a 14–18 L cycle. **Missing or failed valve** (3 m riser): 2.9–3.9 L; with a 3 m back-sloping run, up to 7.9 L = **20–45 %** of an 18" cycle [estimate]. Boshart: "Gravity then causes the water that is in the discharge pipe to drain back into the pit" [sourced]. PumpEd101: a leaking check valve looked like a 12 % flow shortfall [sourced]. | Measure `level_end` at the instant the pump stops, before drain-back. Log drain-back (rise in the 60 s after stop minus the fill trend) as a check-valve health signal. |
| **Anti-airlock hole** | Liberty requires a 1/8" hole above the pump when a check valve is used ("water spray from this hole is normal") [sourced]; that water recirculates into the pit. | About 0.04 L/s at ~3.5 m head = **1–3 %** of pump rate [estimate: orifice, Cd 0.6]. | None needed. The pit-drop method measures net flow, which is what storm accounting wants. |
| **Non-cylindrical basin and displacement** | The pump body, switch, riser, a backup pump and a tapered wall all make the effective area smaller than nominal. | **−10 % to −40 %** vs the §9 nominal area [estimate, §4.1]. Flows 1:1 into `pump_rate_lps`, `inflow_est_l` and `volume_l`. | **Bucket calibration of effective area (L/mm)** — the main point of this report. |
| **Pump curve vs TDH** | Flow drops as head rises. Nameplate "max" flows are at 0–5 ft of head. | Zoeller M53: 43 / 34 / 19 gpm at 5 / 10 / 15 ft (2.71 / 2.15 / 1.20 L/s); M98: 72 / 61 / 45 / 25 gpm at 5 / 10 / 15 / 20 ft [sourced]. A typical basement (about 10 ft static lift plus 1–3 ft friction ≈ 11–13 ft TDH) gives the M53 **25 gpm (1.58 L/s), 42 % below its "43 gpm max"**; the M98 51 gpm (3.24 L/s, −29 %); the Wayne CDU790 2,556 GPH (2.69 L/s, −44 % vs its 0 ft figure) [estimate: interpolated; friction by Hazen-Williams, C = 150, 1-1/2" pipe at 1.6–2 L/s ≈ 0.04–0.06 m per m]. | Never use nameplate flow. Calibrate per home. Plausibility-check against the curve at estimated TDH (§7). |
| **Head change within one cycle** | The level falls 76–254 mm during a run, so static head rises by the same amount. | M53 slope ≈ 0.62 L/s per m, so over 108 mm the flow changes about 4 % [estimate]. PumpEd101: "very little change in flow… over such a small elevation change" [sourced]. | Use a regression slope (average rate) and treat it as the cycle-mean rate. |
| **Pump decline (wear, clogging, air lock)** | Impeller or volute wear, a clogged intake screen, sediment, a partly stuck check valve. | Industrial pumps: efficiency "can degrade as much as 10–25 % before it is replaced" [sourced, medium: Ipieca via search summary]. About 10 % loss after 64 h in abrasive sand-water tests [sourced, but not sump-specific]. **No good residential-sump data found** (gap). | The learned dry-weather drawdown rate (mm/s) follows decline automatically; alert when it drops below about 80 % of the calibrated baseline (proposal). |
| **Sensor resolution** | JSN-SR04T ±1 cm per reading, wide beam, surface ripple [sourced]. | Single readings at each end of a 108 mm drop: ±10–20 % worst case. A 1 Hz regression over an 8 s run: slope standard error ≈ 0.77 mm/s on ~13.5 mm/s = **±6 %**; at 5 Hz, ±2.5 %; at 10 Hz, ±1.8 % [estimate: σ = 5 mm, least squares]. Pour step with 10 s medians at 5 Hz: ±1–2 mm on a 25–75 mm step = ±2–6 % [estimate]. | Sample at 5–10 Hz during runs and pours, use medians, aim perpendicular away from the float and pipe, correct for temperature. |
| **Timing resolution** | `run_s` in fPort 2 is whole seconds. Real runs are short: 14 L at 1.6–2.8 L/s = **5–9 s** [estimate]. | ±0.5 s quantization = **±6–10 % per cycle**. If firmware *truncates*, pump rate is biased **+6–10 %**. A human stopwatch adds ±0.2–0.3 s at each end = ±3–6 % [estimate]. | Round to nearest, not truncate. Use 100 ms resolution for calibration records. Median over many cycles. |
| **Bucket volume** | A "5-gallon" bucket's actual fill level is uncertain. | ±5–10 % if filled "to about the top" [estimate]. | Weigh it (1 kg ≈ 1 L; a bathroom scale gives ±1–3 % on 19 kg) or fill with a 4 L jug [estimate]. |
| **Pouring after the pump starts** | Water poured after the start isn't in the measured rise. | At about 2 L/s pouring and a 1 s reaction, ≈ 2 L on 15–25 L = **8–13 %** [estimate]. | Pour the last portion in small (≤ 2 L) amounts; the node detects the start from the CT clamp. |
| **Speed of sound** | Temperature and humidity change the sound speed. | 0.18 %/°C scale error on the drop [estimate from the sourced formula]. | Correct with the BME280. |

**Combined uncertainty [estimate, root-sum-square]:**

| Setup | Effective area | Pump rate |
|---|---|---|
| Node-assisted, dry weather, working check valve, 3 repeats | about **±3–5 %** | about **±4–6 %** |
| Stopwatch only, owner pouring, no node | — | about **±10–20 %** |

---

## 6. Automating it on the node

### 6.1 What fPort 2 already gives, and what it cannot

- **Drawdown rate (mm/s): yes.** (`level_end_mm − level_start_mm`) ÷ `run_s` per cycle. Phase 4 already takes the median over dry-weather cycles (`Baseflow.PumpMMPerS`). In dry weather, inflow during a run is under 1 % (ADR 0007), so *Q_p* ≈ *A* × median(drop / run_s). The inflow term (*A* × fill rate) becomes significant only in wet weather or for very wet homes. That makes **fPort 2 sufficient for the pump rate in mm/s, subject to these firmware conditions** (Phase 7, not yet written):
  - `level_start_mm` must be a median of samples taken in the ≤ 2 s before the current rises. The 15-min heartbeat reading is useless for this.
  - `level_end_mm` must be a median of samples taken right at the stop, before drain-back.
  - `run_s` must be rounded, not truncated. It is coarse for real 5–15 s runs (§5).
  - The level must be sampled at 5–10 Hz while current is on. A 1 Hz ring buffer when idle on mains is enough to capture the pre-start level [estimate].
- **Effective area (L/mm): no.** Nothing in the telemetry converts mm to litres; only a known volume does. So the bucket test is fundamentally an **area calibration**.

### 6.2 Zero-firmware path (v0): heartbeat-bracketed pour plus a dashboard form

This needs no new uplinks. It can ship with Phase 5.
1. **Plan the pour.** The dashboard shows the last cycle time, the latest heartbeat `level_mm`, and the home's typical ON level (median `level_start_mm` of recent cycles). From these it gives a safe pour volume, using a conservative 0.6 × nominal area so the pour does not start the pump.
2. **Pour and record.** The owner pours that measured volume just after a heartbeat and enters the volume and the time.
3. **Compute the area.** The server takes the rise between the bracketing heartbeats, `level_before − level_after`, and subtracts the learned dry-weather inflow over the 15 min: *A* = *V* / (rise − inflow mm).
   - Inflow error: at 1–12 cycles/day the inflow over 15 min is 0.16–3 L, 1–15 % of 18.9 L before correction, and small after correcting with the learned baseflow [estimate].
   - Single-ping heartbeat noise (±10 mm on 30–150 mm) is ±7–30 % per pour. The firmware heartbeat should therefore be a median of many pings, and the test should be repeated 3 times.
4. **Optional run check.** The owner then pours until the pump starts. The following fPort 2 cycle gives drop and `run_s`, and the server cross-checks *A* × drop / run_s against the volume balance.

### 6.3 Firmware calibration mode (v1): proposal

**Entering it.** LoRaWAN Class A downlinks only arrive after an uplink, which can mean up to 15 min of latency [estimate; standard LoRaWAN behaviour, not researched here]. Two options:
- a **button press on the node** (the Heltec/LILYGO boards have a user button — **verify** for the chosen board), with LED feedback; or
- the next-uplink downlink from a dashboard "Start calibration" session.

The session lasts about 30 min, then the node returns to normal mode.

**Sampling while in calibration mode** [estimate]:

| Signal | When | Rate | Processing |
|---|---|---|---|
| Ultrasonic level | Idle | 2 Hz | Rolling 5 s median |
| Ultrasonic level | During a pour or run, and for 60 s after a stop | 10 Hz (JSN-SR04T allows ~15 Hz at 60 ms spacing) | Rolling 5 s median |
| CT current | Throughout | 100 ms RMS windows (6 mains cycles) | Run time to ±0.1 s |
| BME280 temperature | Throughout | — | Sound-speed correction |

Normal mode can keep a 1 Hz level ring buffer on mains and drop to heartbeat-only on battery [estimate].

**Detection logic** [estimate]:
- **Pour step:** pump current off, level rising faster than 1 mm/s for at least 2 s, total rise at least 15 mm, then settled (5 s standard deviation below 3 mm) for 10 s. Record 10 s medians before and after, and the pre-step fill slope. Mark the step invalid if the pump started during it.
- **Run:** current above threshold. Fit a least-squares slope to the 5 s median series from 1 s after start to 0.5 s before stop, skipping the start transient. Record the settled pre-start level, the level at stop, and drain-back (rise over 60 s after stop minus the pre-run fill slope × 60 s).

**Server-side maths:**
- *A_eff* = Σ*V_i* / Σ(*rise_i* − *fill_slope* × *t_i*), using the volumes the owner enters.
- *Q_p* = *A_eff* × (*drawdown_slope* + *fill_slope*).
- Cross-check: *A_eff* × (*level_before_first_pour* − *level_at_stop*) ≈ Σ*V* + inflow + pipe refill.

**Uplink layout (proposal; fPort number left to the owner).** Every record fits in 11 bytes so it can go at US915 DR0 (progress.md: "All ports ≤ 11 B (US915 DR0 limit)"). All fields are little-endian. The first byte packs the record type and a session sequence number.

*Record type 1 — pour step (11 B)*

| Field | Type | Unit |
|---|---|---|
| type_seq | uint8 | high nibble = 1, low nibble = session seq |
| start_offset_s | uint16 | seconds before transmit |
| level_before_mm | uint16 | 10 s median, temperature-corrected |
| level_after_mm | uint16 | 10 s median after settling |
| fill_slope_um_s | int16 | µm/s before the step (positive = rising water) |
| settle_s | uint8 | seconds from step start to settled |
| flags | uint8 | bit0 pump_ran_during_step, bit1 unsettled, bit2 sensor_fault, bit3 mains_lost |

*Record type 2 — calibration run (11 B)*

| Field | Type | Unit |
|---|---|---|
| type_seq | uint8 | high nibble = 2 |
| start_offset_s | uint16 | seconds before transmit |
| run_ds | uint16 | 0.1 s |
| level_start_mm | uint16 | settled median before start |
| drawdown_cmm_s | uint16 | 0.01 mm/s regression slope (up to 655 mm/s) |
| drainback_mm | uint8 | rise in 60 s after stop minus trend (saturates at 255) |
| mean_current_da | uint8 | 0.1 A, steady-state mean excluding a 0.5 s inrush window (max 25.5 A) |

The level at stop is derivable as `level_start_mm + drawdown × run` for a cross-check.

*Record type 3 — session summary (11 B)*

| Field | Type | Unit |
|---|---|---|
| type_seq | uint8 | high nibble = 3 |
| session_s | uint16 | seconds since calibration mode started |
| fill_slope_um_s | int16 | quiescent inflow slope over the session |
| n_steps | uint8 | |
| n_runs | uint8 | |
| temp_c | int16 | 0.01 °C (sound-speed correction audit) |
| level_noise_dmm | uint8 | 0.1 mm, standard deviation of quiescent readings |
| flags | uint8 | bit0 aborted, bit1 timeout, bit2 sensor_fault |

The golden byte vectors in `internal/codec` would cover these the same way as fPorts 1–5.

### 6.4 What the CT clamp can and cannot add

- **Run time (strong).** The clamp is the most accurate timer available. It removes the stopwatch from the procedure entirely.
- **Head (weak).** For radial centrifugal pumps, "as water flow increases, the pump's power consumption also increases" [sourced: Wilo]. Higher head or a clog therefore lowers the current, but only slightly. Small single-phase motors have a large magnetising current. The DOE says that below about 50 % load "current measurements are not a useful indicator of load" [sourced; three-phase guidance whose principle applies]. Sump motors at part head sit in that region. So current **cannot** give TDH or flow without a per-model calibration curve [estimate].
- **Pump health (useful as a per-home trend)** [estimate]:
  - Mean running current down about 10 % together with drawdown mm/s down about 20 % suggests intake clog, air lock or wear.
  - Current up more than 15 % with normal drawdown suggests bearing or motor trouble.
  - Current normal with near-zero drawdown is the existing §10 dry-run rule (failed impeller, stuck check valve, frozen discharge).
  - `peak_current_da` as currently specified is probably dominated by start inrush (induction-motor starting current is several times running current; not researched here). **Define it in firmware** as the maximum of 100 ms RMS windows after a 0.5 s blanking period, or add a steady mean-current field (proposal).

---

## 7. Typical residential pump rates (sanity bounds)

Flow in L/s (US gpm or GPH converted). Sources are manufacturer data sheets or manuals except where marked.

| Pump | hp | 0 ft | 5 ft | 10 ft | 15 ft | 20 ft | Source |
|---|---|---|---|---|---|---|---|
| Superior 92250 (thermoplastic, tethered) | 1/4 | ~1.89 (1,800 GPH max) | — | **1.26** (1,200 GPH) | — | — | retailer listing [**weak**] |
| Wayne WST33 (thermoplastic) | 1/3 | 3.15 | 2.74 | 2.24 | 1.48 | 0.42 | Wayne manual |
| Zoeller M53 / M57 (cast iron) | 3/10 | — | 2.71 | 2.15 | 1.20 | shut-off 19.25 ft | Zoeller FM2778 |
| Wayne SPF33 | 1/3 | 3.94 | 3.47 | 2.86 | 2.02 | — | Wayne manual |
| Wayne CDU790 | 1/3 | 4.84 | 4.01 | 3.22 | 2.33 | 1.26 | Wayne manual |
| Liberty 257 | 1/3 | 50 gpm max (3.15) | — | — | — | shut-off 18–23 ft | retailer listings [**weak**] |
| Wayne SPF50 | 1/2 | 4.52 | 4.10 | 3.53 | 2.84 | 1.32 | Wayne manual |
| Wayne CDU800 | 1/2 | 5.36 | 4.73 | 4.04 | 3.22 | 2.15 | Wayne manual |
| Zoeller M98 | 1/2 | — | 4.54 | 3.85 | 2.84 | 1.58 (shut-off 23 ft) | Zoeller FM2779 |

**Plausibility bounds for a calibrated primary pump at 5–15 ft [estimate from the table]:**
- 1/4 hp: about 0.8–1.9 L/s.
- 1/3 hp: about 1.2–4.0 L/s.
- 1/2 hp: about 2.3–4.7 L/s.
- **Flag results outside 0.6–5.0 L/s.**
- **Flag an effective area outside 0.5–1.05 × the nominal basin area.** An area above nominal means the poured volume was overstated or inflow was ignored.
- **Flag a measured rate below 50 % of the curve at the estimated TDH** as a pump or check-valve fault.
- **Flag a rate above 110 % of the curve** as a probable area or volume error.

**Finding for the simulator [estimate, proposal]:**
- `internal/sim/home.go` draws `PumpLPS = unif(0.5, 1.2)` L/s. That range covers only 1/4 hp or degraded pumps; common 1/3–1/2 hp pumps at 10–13 ft deliver about 1.6–3.8 L/s.
- Simulated cycles (0.164 m² × 150 mm ≈ 25 L) therefore run about 20–50 s. Real ones likely run 5–15 s.
- Consequences:
  - The Phase 4 "up to ~2.6× understatement" (inflow nearing pump capacity) may occur less often in real homes.
  - The integer `run_s` quantization will matter more in reality than the e2e showed.
- This is worth a note under §14's existing "calibrate the simulator" question.

---

## 8. Homeowner procedure (plain language)

### 8.1 Safety first

- **Do not reach into the pit or touch the pump, float or water** while the pump is plugged in.
  - Liberty: "Do not handle or unplug the pump with wet hands, when standing on damp surface, or in water."
  - Wayne: "ALWAYS DISCONNECT THE PUMP from power supply before installing, servicing or making any adjustments" and "DO NOT WALK on the floor when water is present until all power is turned off" [sourced].
  - This test needs none of those actions: you only pour water in.
- **Check the outlet** is a grounded receptacle and that nothing has been bypassed. Wayne's manual says "FOR ADDED SAFETY the receptacle must be protected with a ground fault circuit interrupter (GFCI)" [sourced]. The Ontario position is more nuanced:
  - ESA Bulletin 26-26-6 says the OESC "does not require a designated receptacle or a dedicated branch circuit for a sump pump" [sourced].
  - I did not verify ESA's exact GFCI wording; the 2021 CE Code GFCI changes were seen only in secondary summaries.
  - **Do not have owners add or swap receptacles.** That is electrician (ESA) work. Leave the sumpnet splitter and CT clamp in place.
  - Side note: if a GFCI trips, the pump stops. The node's `mains_ok` only sees that if the node is powered from the same protected receptacle (proposal to check in the Phase 7 install guide).
- **Keep children and pets away** and put the pit lid back afterwards.
- **Pour clean tap water gently against the pit wall,** not onto the float or the pump, so sediment isn't stirred up and the float isn't knocked [estimate].
- **Winter:** don't test when the discharge line or outlet might be frozen or snow-covered.
  - Extra water with a blocked outlet can make the pump dead-head or overflow.
  - Liberty: "Do not allow pump to freeze" [sourced].
  - Freezing at the outlet or in the pipe is a common Ontario failure [sourced, **weak**: contractor pages].
  - Test on a day above freezing after checking that the outlet is clear.
- **Weather:** test after at least 3 dry days, never during rain or snowmelt.

### 8.2 Guided test with the sumpnet node (recommended)

1. **Measure your water.** Use 4 L jugs, or weigh the bucket: set a bathroom scale to zero with the empty bucket, then 1 kg of water is 1 L. Don't trust "5 gallon" markings.
2. In the dashboard, open **Pump calibration**, read the safety checklist, and press **Start**. If your node has a button, press it until the light blinks.
3. **Wait until the pump has just finished running.** If you're not sure, pour water slowly until it starts, let it stop, then wait 2 minutes.
4. **Pour the amount the dashboard suggests.** In an 18" pit, one 4 L jug at a time; in a 24" pit, half a bucket. Pour over about 10 seconds, against the wall. Wait 1 minute. Type in the litres you poured.
5. **Repeat step 4** until the pump starts. **Stop pouring the moment you hear it start,** and enter how much you actually poured.
6. **Let the pump finish.** Wait 2 minutes. That's one round.
7. **Do three rounds.** The dashboard shows your pit size (litres per centimetre), your pump's flow rate, and a check-valve result. It asks you to repeat if the rounds disagree by more than 10 %.

### 8.3 Stopwatch-only version (no node, or as a cross-check)

Do steps 1 and 3 above, then:
1. Pour measured water until the pump starts. Pour the last part slowly, 1–2 L at a time, and note the total litres poured.
2. With a phone stopwatch, time from the moment the pump starts to the moment it stops.
3. Pump rate ≈ litres poured ÷ seconds. Do it three times and average.
4. Expect about ±10–20 % [estimate]. If your check valve is missing or leaks, this method reads low (§5).

### 8.4 How often

- **At installation or commissioning** of the node.
- **After any change** to the pump, float or switch, check valve, discharge piping or basin.
- **Once a year** in spring, before the thaw, matching Public Safety Canada's "full pump test once a year" [sourced].
- **When the platform asks.** The learned dry-weather drawdown rate drifting more than about 15 % from the calibrated baseline is the trigger (proposal).

### 8.5 How the result reaches the platform

- **v0:** a dashboard form (volumes and times), with the maths on the server from heartbeats and fPort 2 cycles (§6.2).
- **v1:** node calibration mode with automatic step and run detection (§6.3). The owner still enters the litres, because the node cannot know them.
- **Storage and privacy:** results are owner-only data, like all per-house data under ADR 0005 privacy rules. Only derived storm volumes enter the k ≥ 3 segment aggregates.

---

## 9. Recommendations for sumpnet (all are proposals for the owner)

**P1 — Calibrate area, not rate (key proposal).**
- Treat a bucket test primarily as a measurement of `homes.pit_area_m2` (effective litres per mm).
- Keep `pump_rate_lps` = calibrated area × learned dry-weather median drawdown (mm/s). The learned drawdown keeps following wear and clogging, which ADR 0007 already flags as the weakness of a frozen rate.
- Store the bucket-measured run rate as a *validation* value.
- Migration 0006 (phase-4 worktree) currently reserves `pump_rate_source = 'bucket_test'` as a rate that "takes precedence once recorded". The owner should decide whether `bucket_test` should mean "rate computed with the bucket-calibrated area" instead.

**P2 — §4 Hardware.** State explicitly that level sensing is **ultrasonic (JSN-SR04T), not optical**, with installation constraints:
- the sensor face at least 30 cm above the float-high level (blind zone 25 cm);
- aimed perpendicular, with the beam clear of the float and discharge riser;
- the CT clamp on the primary pump cord only;
- the node's mains detection on the pump's receptacle;
- note that time-of-flight sensors are not recommended for water surfaces.

**P3 — §5 payloads.**
- (a) Define `level_start_mm` and `level_end_mm` as settled medians (before the current rises; at the stop, before drain-back).
- (b) Specify that `run_s` rounds to nearest, and consider a future `run_ds` field. That would be a breaking layout change, so it is the owner's call.
- (c) Define `peak_current_da` so it excludes inrush, or add a mean-current field.
- (d) Add calibration record types 1, 2 and 3 (§6.3) on an fPort the owner assigns.

**P4 — §9 data model.**
- Add `homes.pit_area_source` (`nominal` | `bucket_test`) and `pit_area_calibrated_at`.
- Add a `bucket_tests` table: home_id, performed_at, method `form` | `node`, per-pour volume_l / rise_mm / fill_slope, run_ds, drawdown_mm_s, drainback_mm, result_area_m2, result_pump_lps, accepted, reject_reason.
- Update the §9 volume formula text: "`pit_area_m2` is nominal until a bucket test calibrates it."

**P5 — §10 analytics.** Add these definitions:
- **Effective pit area:** *A* = Σ*V* / Σ(rise − fill trend).
- **Measured pump rate:** *A* × (drawdown + fill slope).
- **Drain-back:** rise within 60 s of stop minus trend. Raise a WARNING `check_valve` if it exceeds about 20 % of the cycle drop (threshold to be tuned).
- **Pump degradation:** 7-day median dry-weather drawdown below 80 % of the calibrated baseline raises a WARNING (threshold to be tuned).
- **Plausibility bounds from §7:** rate 0.6–5.0 L/s; area 0.5–1.05 × nominal.
- Acceptance: three rounds within ±10 %.

**P6 — Firmware (Phase 7).**
- Keep a 1 Hz level ring buffer on mains; sample 5–10 Hz during runs and for 60 s after each stop.
- Measure CT current in 100 ms RMS windows.
- Correct the speed of sound with BME280 temperature.
- Add a calibration mode (button or downlink) with the step and run detectors in §6.3.
- Round, don't truncate.

**P7 — Dashboard (Phase 5).** A "Pump calibration" wizard:
- safety checklist (§8.1) and weather gate (72 h dry, above freezing);
- the suggested safe pour volume from the last heartbeat and typical ON level;
- litres entry per pour, and the stopwatch fallback form;
- results with plausibility flags and the pump-curve comparison (optional: owner selects the pump model and an estimated lift);
- a reminder when drift is detected.

**P8 — Simulator.**
- Revisit `PumpLPS` (0.5–1.2 L/s against the 1.3–4 L/s real curves).
- Consider per-home TDH and pump-curve parameters.
- Add displacement, taper and drain-back parameters, so the calibration maths and the check-valve rule can be tested against ground truth.
- Record this under the §14 simulator-calibration question.

**P9 — Open questions to add to §14.**
- Do Timberwalk homes discharge to the surface or to a storm lateral? This decides whether a discharge-bucket check is ever possible.
- Is node-button calibration acceptable for owners, or should it be dashboard-only?
- Which fPort should calibration records use?

---

## 10. Sources

Strength: **S** = manufacturer, government or utility primary document; **M** = trade or engineering publication, extension service or vendor technical note; **W** = retailer, contractor blog, forum or search snippet (use with caution).

1. Zoeller, *Mighty-Mate Models 53/55/57/59 Technical Data Sheet* FM2778: on/off 7-1/4"/3"; 43/34/19 gpm at 5/10/15 ft; shut-off 19.25 ft. **S.** https://library.coburns.com/specs/CATALOG_Zoeller_53-0001.pdf
2. Zoeller, *Model 98 Technical Data Sheet* FM2779: on/off 9-1/2"/3"; 72/61/45/25 gpm at 5/10/15/20 ft; shut-off 23 ft. **S.** https://assets.unilogcorp.com/2/ITEM/DOC/Zoeller_98-0001_Specification_Sheet.pdf ; product page https://zoellerpumps.com/product/model-98-sump-pump/
3. Wayne, *Sump Pump Operating Instructions and Parts Manual* (CDU1000/980E/800/790, SPF33/50, WST33): GPH table; vertical ON ~9"/OFF ~4"; tether ~13"/~7"; GFCI and disconnect warnings. **S.** https://www.waynepumps.com/wp-content/uploads/woocommerce_uploads/2016/05/Sump_600002W-001-F_Web-3p6foe.pdf
4. Liberty Pumps, *Installation Manual 7035000*: Table 1 factory float settings (257: 4"/7"; 287: 4"/9.5"; FL-series 5–8"/12–15"); check valve and anti-airlock hole; "Do not allow pump to freeze"; handling warnings. **S.** https://www.libertypumps.com/Portals/0/Files/Install%20Manuals/English/7035000_EN.pdf
5. LevelGuard, *Electronic Sump Pump Switch Z24800A1Z product overview*: minimum differential 6" (150 mm); effective 8–10". **S.** https://s3.amazonaws.com/s3.supplyhouse.com/product_files/Level-Guard-Z24800A1Z-Product-Overview.pdf
6. Ion Digital Level Switch, 6" range (retailer). **W.** https://www.sumpdirect.com/ion-digital-level-switch-6-range-20-cord-in-006-020-10pa-b/
7. RCAP, *Wastewater Maintenance: Drawdown Pump Test*: influent + drawdown = pump rate; efficiency vs rated. **S/M** (US federally funded utility technical assistance). https://www.rcap.org/wastewater-maintenance-drawdown-pump-test/
8. J. Evans (PumpTech / Pumps & Systems), *Wastewater Pump Draw Down Calculator*: drawdown as standard test; repeat; leaking check valve example; correct for piping volume in small wells. **M.** http://pumped101.com/Draw%20Down.pdf
9. Romtec Utilities, *Draw Down Testing on New Pump Stations*: fill the force main first for consistent head. **M** (vendor). https://romtecutilities.com/draw-down-testing-on-new-pump-stations/
10. HNWS Engineering Guidelines, *Form 3.5 LS Start-Up Part 4 – Draw Down Test* (rev. 2021-05-24): gallons per inch × drawdown ÷ minutes; TDH from pressure. **S** (utility form; "HNWS" is not expanded in the document). https://cms9files1.revize.com/hnws/Engineer/Part%205/Part%205%20-%20Form%203.6.4%20LS%20Startup_Draw%20Down%20(05-24-21).pdf
11. WATERPROOF! Magazine, *Sizing Up a Sump Pump* (2013): 60 s rise test; 18" ≈ 1 gal/in, 24" ≈ 2 gal/in. **M.** https://www.waterproofmag.com/2013/04/sizing-up-a-sump-pump/
12. Public Safety Canada, *Sump pumps*: pour water to confirm activation; full test once a year; discharge ≥ 1.5 m from foundation. **S.** https://www.canada.ca/en/services/policing/emergencies/preparedness/get-prepared/hazards-emergencies/floods/flood-risk/prevent-flood-damage/sump-pumps.html
13. University of Georgia CAES, *The Bucket Method* (C1331-01): at least 3 trials; suited to about 5–15 L/s. **S/M** (extension). https://fieldreport.caes.uga.edu/publications/C1331-01/the-bucket-method/
14. Geological Survey of Finland, Mine Closure Wiki, *Bucket method*: about 5–6 L/s limit; accuracy suffers when filling takes only seconds; 5–7 repeats. **M.** https://mineclosure.gtk.fi/bucket-method/
15. Boshart, *Why are Sump Pump Check Valves Important?* (drain-back) **M**, https://blog.boshart.com/why-are-sump-pump-check-valves-important ; *Setting the Tether Point and Tether Length* **M**, https://support.boshart.com/setting-the-tether-point-and-tether-length-on-a-sump-pump-float-switch
16. Schedule 40 PVC dimensions (1-1/4" ID 1.380"; 1-1/2" ID 1.610"; ASTM D1785 values). **M.** https://pexuniverse.com/pvc-pipe-dimensions-specs ; https://www.pipeflowcalculations.com/tables/pvc-schedule-40.xhtml
17. ShillehTek, *JSN-SR04T Manual*: 25 cm blind zone; ±1 cm; 45–75° beam; ~60 ms ping spacing; water-surface jitter; stilling tube. **W/M** (vendor). https://shillehtek.com/blogs/shillehtek-product-manuals/jsn-sr04t-waterproof-ultrasonic-distance-sensor-arduino-esp32-manual . Original datasheet (not retrievable, HTTP 403): https://www.makerguides.com/wp-content/uploads/2019/02/JSN-SR04T-Datasheet.pdf
18. Wikipedia, *Speed of sound*: c ≈ 331.3 + 0.606·θ m/s. **M.** https://en.wikipedia.org/wiki/Speed_of_sound
19. Wilo, *Getting the Most Out of Your Pumps*: power rises with flow; head falls as flow rises. **S/M.** https://wilo.com/us/en_us/Training/On-Demand-Resources/Pump-Basics/Getting-the-Most-Out-of-Your-Pumps/
20. US DOE Motor Challenge, *Determining Electric Motor Load and Efficiency*: current is a poor load indicator below about 50 % load. **S** (mirror copy). https://irrigationtoolbox.com/ReferenceDocuments/DOE/Determining%20Electric%20Motor%20Load%20and%20Efficiency.pdf
21. Pump degradation: Ipieca, *Energy efficiency compendium – Pumps* (2022), "10–25 %" seen only in a search summary, **M/W**, https://www.ipieca.org/resources/energy-efficiency-compendium/pumps-2022 ; ASME *J. Energy Resour. Technol.* 143(8):082104 (ESP in sand-water, ~10 % after 64 h; abstract via search summary), **M**, https://asmedigitalcollection.asme.org/energyresources/article-abstract/143/8/082104/1088997/Experimental-Study-on-Deteriorated-Performance
22. Electrical Safety Authority (Ontario), *Bulletin 26-26-6*: no designated receptacle or dedicated circuit required for a sump pump. **S** (GFCI wording not verified). https://esasafe.com/assets/files/esasafe/pdf/Electrical_Safety_Products/Bulletins/26-26-6.pdf
23. Canadian Electrical Wholesaler / Guillevin, *New Rules Around GFCIs* (2021 CE Code 26-704, 26-712; seen via search summary only). **M/W.** https://www.canadianelectricalwholesaler.ca/guillevin-code-series-gfcis/
24. Time-of-flight on water: Roger Frost blog, reader comment on reflections, **W**, https://www.rogerfrost.com/water-or-oil-tank-level-mounting-a-vl53l0x-time-of-flight-sensor/ ; ST AN5851 (VL53L4CD liquid level; **not retrieved**), https://www.st.com/resource/en/application_note/an5851-water-and-liquid-level-monitoring-using-vl53l4cd-timeofflight-high-accuracy-proximity-sensor-stmicroelectronics.pdf
25. Basins: Jackel SF22A (18" ID, 24" H, 22 gal), **S**, https://jackel.com/sf22a.html ; tapered 18" × 24" basins (Menards, Prinsco "21 gallons to lid"), **W**, https://www.menards.com/main/plumbing/rough-plumbing/sewage-tanks-septic-tanks/sewage-sump-tanks/18-x-24-corrugated-tapered-wall-sump-basin/su1824-bm/p-1444451527499-c-8598.htm , https://www.landmsupply.com/prinsco-18-x24-tapered-sump-basin
26. Tethered float pumping range 7–36", typical ON/OFF pairs. **W.** https://rcworst.com/blogs/news/how-to-select-the-right-pump-switch-for-the-job
27. Superior Pump 92250, 1/4 hp, "1,200 GPH @ 10 ft" (retailer; the manufacturer page returned 404). **W.** https://www.homedepot.com/p/Superior-Pump-1-4-HP-Submersible-Thermoplastic-Sump-Pump-92250/204610103
28. Liberty 257 retail listing (50 gpm max, 1/3 hp). **W.** https://www.pumpproducts.com/liberty-257-1-3-hp-automatic-submersible-sump-pump-w-vertical-magnetic-float-switch-50-gpm-115v-1-phase-10-ft-cord.html
29. Discharge freezing (contractor pages). **W.** https://www.omnibasementsystems.com/basement-waterproofing/products/iceguard.html ; https://dkiburlington.ca/blog/2025-12-02-protecting-sump-pump-discharge-lines-during-freeze-thaw-cycles.html
30. Unified Restoration (Edmonton), pour 15–20 L; test twice a year. **W.** https://unifiedrestore.ca/resources/resources-sump-pump-maintenance-edmonton/
31. Sump Pump Gurus, flow tests (1-gallon discharge timing). **W.** https://sumppumpgurus.com/how-to-test-sump-pump-flow-rate/

**Repo context read (not modified):**
- `/Users/seanhunt/Code/sumpnet/prompt_plan.md` §4, §5, §9, §10, §14; `CLAUDE.md`; `progress.md`.
- `/Users/seanhunt/Code/sumpnet-worktrees/phase-4/`:
  - `docs/adr/0007-storm-analytics.md`
  - `migrations/0006_pump_rate.up.sql`
  - `prompt_plan.md`
  - `internal/sim/home.go` (PumpLPS 0.5–1.2 L/s; ON−OFF 130–170 mm; pit area 0.148–0.180 or 0.27–0.31 m²)
