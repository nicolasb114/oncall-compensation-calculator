# On-Call Compensation Calculator — Product Requirements Document

**Status:** v0.6, approved for the mockup phase. No code is built until the mockups are approved.
**Owner:** nbriceno
**Delivery:** a single self-contained Go executable (decided).
**Guiding principle:** flexibility and usability. Every rule is adjustable, every rule has a sensible default, and a user can get a result without configuring anything.

---

## 1. Problem

Customers ask PagerDuty how many hours their people were on call, and how those hours break down, so they can compensate responders. Answering takes manual work. Every customer has different rules (business hours, countries, holidays, rates), so each report is manual work and hard to audit.

## 2. Goal

One executable that pulls on-call and engaged time from PagerDuty, classifies every hour under adjustable rules **in each responder's own time zone**, optionally prices it using the customer's compensation approach, and produces a report in whatever format the audience needs.

### Non-goals (v1)
- Not a payroll system. Output is a report or estimate that the customer feeds into their own payroll.
- No hosted service and no multi-user accounts.
- Read-only use of the PagerDuty API. Nothing is written to PagerDuty.

## 3. Users

| Persona | Need |
|---|---|
| **Account Owner of a PagerDuty account** (primary) | Runs the tool on their own account with their own read-only API key. Produces on-call and compensation reports for their organization, and adjusts the rules themselves. |
| **PagerDuty CSM / SE / TAM** (secondary) | Demo the tool and help a customer set it up, connected to their own sandbox account. |
| **Customer manager or ops lead** | Understand who carried on-call load and why the numbers are what they are. |
| **Customer payroll or HR** | A machine-readable file with unambiguous categories and, if wanted, amounts. |

## 4. Functional requirements

### 4.1 Connection
- FR-1: Connect with a PagerDuty REST API token (read-only). **The token is stored**, encrypted, in the operating system's credential store (macOS Keychain, Windows Credential Manager, Linux Secret Service). The user can view which connections exist, rename them, and delete them. The token is never shown in full and never written to logs or exports.
- FR-2: One account is the normal case. Saving more than one connection is an optional extra. Profiles, filters and the token are private to the person using the tool and are not shared between colleagues. Profile import and export exists only as a backup and for moving to a new computer.
- FR-2a: First-run guidance explains how the Account Owner creates a read-only REST API key and which access the tool needs.
- FR-3: Validate the token on save and give a plain explanation if it is invalid or lacks access.
- FR-4: **Reliable API use.** The tool makes many API calls, so it must be gentle and resilient:
  - **Pagination:** every list and report response is followed until it is complete. A response shorter than expected is never assumed to be the full result. The tool follows `more`/offset or cursor values until the API says there is no more, and checks the totals it can.
  - **Pacing:** calls are spaced out with a configurable delay and a cap on concurrent requests, kept well under PagerDuty's published limits. The pace adjusts automatically when the API pushes back.
  - **Retries:** failed calls (rate limit 429, server errors 5xx, timeouts, dropped connections) are retried automatically with increasing wait times and some randomness, honoring the wait time the API returns. After the final attempt the tool does not silently skip data: it marks the affected people and days as incomplete and the report says so.
  - **Visibility and control:** a progress view shows what is being fetched, any waits or retries, and lets the user pause, cancel or resume without starting over.
  - **Settings:** delay between calls, maximum retries and request timeout are adjustable in an Advanced section, with safe defaults.

### 4.2 Choosing who and when
- FR-6: Pick the **reporting period** with presets (last week, last month, last quarter, custom) and an single **reporting time zone** that sets where the period starts and ends. Classification of each hour still uses the responder's own zone. Every report screen and export shows a small, subtle disclaimer in red text, for example: "Period boundaries use [zone]; hours are classified in each responder's local time."
- FR-7: Filter responders by any combination of: **team, schedule, escalation policy, service, individual user, role, time zone or country**, and free-text search. CSV import of user IDs is available as an extra option. Saved filters can be reused.
- FR-8: Show a preview of who matches the filter before running, with counts.

### 4.3 Time classification (the core)

Every on-call second is placed in **exactly one** bucket, so the buckets always sum to the total on-call time.

| Bucket | Meaning |
|---|---|
| **Business hours** | Weekday, not a holiday, inside the business window. |
| **Non-business hours** | Weekday, not a holiday, outside the business window and not night. |
| **Night** | Weekday, inside the night window. |
| **Weekend** | Weekend day, not night. |
| **Weekend night** | Weekend day, inside the night window. |
| **Holiday** | A holiday, any hour. Optional setting: split into holiday and holiday night. |

Reported next to the buckets as **separate metrics**, not additional buckets:
- **Engaged time:** time actively working incidents, shown in total and per bucket.
- **Level 1 on-call** versus **level 2 and above on-call:** hours are reported separately for first-level responders and for second level and higher. Each level can have its own rate.

Rules:
- FR-9: **Per-responder time zone.** Business hours, night hours, weekend days and holidays are all evaluated in the responder's own time zone. The default is read from the `time_zone` field (IANA format, for example `America/Lima`) of each user via the PagerDuty Get User API. It can be overridden per person, per team or per group, so the same period is classified correctly for a team spread across several countries.
- FR-10: **Business hours and night windows are fully definable**, per profile: start and end times, which weekdays are business days, a different window per weekday, and an optional break. Each window can be tied to a time zone, either "the responder's" (default) or a fixed one.
- FR-11: **Weekend** is configurable (Sat–Sun, Fri–Sat, or any set of days), because it differs by country.
- FR-12: **Holidays:** a holiday calendar per country or region, manual dates, import from CSV or ICS, and a built-in public holiday list for convenience. Each responder uses the calendar for their country. Half-day holidays are supported. A holiday is one bucket for all 24 hours by default, with an optional setting to split out a separate holiday night. A per-run toggle chooses what holidays do: (a) count as a holiday bucket, (b) count as a normal day, or (c) are excluded from the report.
- FR-13: **Precedence is configurable.** The default is Holiday, then Weekend (with its night variant), then Night, then Non-business, then Business. Edge cases such as a night that crosses midnight into a holiday follow this order, and the report states which rule applied.
- FR-14: Daylight saving time and 23- or 25-hour days are handled correctly.
- FR-15: **Profiles:** a profile is a named bundle of the rules above plus pricing. A report assigns profiles to groups of responders, for example "EMEA support" and "US SRE", and totals them together.
- FR-16: Overlapping on-call time is never counted twice within the same level. The user can choose how a responder who is on call at two levels at once is counted.

### 4.4 Output mode and compensation

At the start of a report the user chooses the **output mode**:

- **Hours only:** the full breakdown with no money.
- **Hours and compensation:** the same breakdown plus amounts.

Compensation is configured per profile, using either of two pricing styles. The user picks the one that fits the customer:

- **Style A, a rate per bucket:** an explicit amount per hour for business, non-business, night, weekend, weekend night, holiday, engaged, and so on.
- **Style B, a base rate plus a surcharge:** one normal hourly rate, plus a **percentage surplus** for each bucket (for example night +25%, weekend +50%, weekend night +75%, holiday +100%). The tool shows the resulting effective rate for each bucket so the user can see the figure before running.

Both styles share these options, all optional:
- FR-16a: **Worked example (must be supported out of the box):** base rate 5 per hour on business days, +25% outside business hours, +50% at night, with percentages applied to the base value. This is Style B with surcharges of 0%, 25% and 50%.
- FR-16b: **Stacking rule for combined cases** (weekend night, holiday night): the user chooses *add the surcharges together* (default), *highest surcharge wins*, or *enter a custom value*. The effective rate is always displayed before running.
- FR-17: A separate rate or surcharge for **engaged time**, with an optional minimum paid per call-out.
- FR-18: A separate rate for **level 1** and **level 2 and above**.
- FR-19: A flat stipend per shift or per period, in place of or in addition to hourly pay.
- FR-20: Caps and floors per person per period, tiers (the rate changes after N hours), and conversion to time off in lieu.
- FR-21: Currency, number format, and rounding (none, nearest 15 minutes, round up).
- FR-22: Per-person overrides.
- FR-23: Every priced output is labelled as an estimate and prints the rates used.
- FR-23a: **Estimate breakdown.** Compensation is shown per hour type (business, non-business, night, weekend, weekend night, holiday, engaged), per person, ending in a **Total estimate** per person and for the whole report. The same breakdown is available in every export.
- FR-24: A **what-if** view: change a rate or rule and see the totals and the difference from before, instantly, without refetching data.

### 4.5 Reports and export
- FR-25: **On-screen:** summary table with one row per responder and a column per bucket, engaged, level split, and cost if enabled. A drill-down shows a day-by-day view and a visual timeline of one person's on-call time, colored by bucket.
- FR-26: **Exports, all available, chosen per run:** CSV, XLSX, JSON, PDF, and a self-contained HTML report. Customer-facing PDF and HTML carry a clean, branded layout. CSV and JSON are structured for payroll and for loading into other tools.
- FR-27: Every export embeds the **rules used**: period, time zones, windows, holidays, precedence, rates and data timestamp. A report can be reproduced and audited.
- FR-28: Optional anonymization of names in exports.
- FR-29: **Saved reports.** A saved report keeps the people filters, rules, pricing and export choices. It is separate from the API connection, which is stored once. To rerun a report the user opens it from the Home screen, picks new dates and runs, with no need to re-enter the API key or rebuild anything. Reports can be duplicated and edited. Profiles and reports can be exported and imported as files for backup or moving to another computer.
- FR-30: Offline snapshot: save the raw data fetched, so rules and prices can be changed later without calling the API again.

### 4.6 Interface
- FR-31: The single binary opens a **local web UI** in the browser (served from assets embedded in the binary, bound to localhost only). The same engine is available as a **command-line mode** for scripting and scheduled runs, using saved profiles and filters.
- FR-32: A guided flow of five steps: **Connect, Who and when, Rules, Review, Export.** Experienced users can jump to any step.
- FR-33: A **live timeline preview** shows how a sample week is classified for a chosen responder as they edit rules, in the responder's time zone. This is the main tool for building trust in the rules.
- FR-34: Plain-language descriptions and inline help for every setting. Clear error messages that say what to do next.

## 5. Non-functional requirements

- NFR-1: **One executable with no dependencies**, for macOS (Apple silicon and Intel), Windows and Linux.
- NFR-2: **Security and privacy:** token stored only in the OS credential store, data stays on the local machine, no telemetry, web server on localhost only with a per-session access token.
- NFR-3: **Accuracy:** the classification engine is tested against DST changes, midnight crossings, shifts spanning a holiday, leap days, time zones with half-hour offsets, and overlapping schedules. A test enforces that buckets sum to the total.
- NFR-4: **Performance:** a 30-day report for 50 responders completes in under 2 minutes including rate-limit waits.
- NFR-5: **Accessibility:** WCAG 2.1 AA, keyboard navigation, light and dark themes, responsive layout.
- NFR-6: **Design:** guided by PagerDuty's slide template, not copied from it. Palette: near-black text `#171817`, white, brand green `#06AC38`, dark green `#048A24`, deep green `#005A2F`, lime accent `#ADF07E`, teal `#117170`, navy `#082B56`, light gray `#E6E6E6`, alert red `#F82431` (also used for the small red disclaimer). Typography: Helvetica Neue (Light for large headings), with Arial as the fallback. These are taken from the template and will be confirmed in the mockups.

## 6. Data sources and technical risks

| Topic | Note |
|---|---|
| On-call and engaged time | PagerDuty Analytics responder metrics provide on-call and engaged seconds per responder for a given window. |
| Level 1 versus level 2+ | Analytics totals do not split by escalation level. The on-call and schedule APIs expose the escalation level of each on-call period. The build will combine the two sources, and **this must be validated against a real account first.** |
| Time zone and DST | Windows are cut in each responder's zone and queried in small blocks. Needs a dedicated test suite. |
| Plan entitlements | Analytics may not be available on every plan. The tool detects this and explains it. |
| Customer rules more complex than the model | The rule engine is data-driven with profiles and overrides, and raw classified hours are always exportable. |
| A number mistaken for payroll truth | Estimate labelling and printed rules on every report. |

## 7. Success criteria

- From launching the binary to a first report takes less than 5 minutes for a new user.
- A customer with staff in several countries gets correct per-person classification with no manual workarounds.
- The customer can answer "how was this number derived" from the report alone.
- The classified hours for a real account agree with a manual check of a sample of days.

## 8. Milestones (after green light)

1. **Mockups, no code:** reviewed and approved by you.
2. Classification engine and tests.
3. PagerDuty data connector with pagination, pacing, retries, level split and offline snapshot.
4. Local web UI built to the approved mockups.
5. Pricing, what-if, exports.
6. Packaging for all platforms, documentation.

## 9. Open questions

See the chat message.

---

## Appendix: v1 build notes (what was built and how)

- **On-call hours** come from the On-Calls API (exact periods with escalation level, merged so overlaps are never double counted). **Engaged time** comes from Analytics, queried per classified window and attributed to the window in which the incident falls. It is only queried for windows where the person was on call, and it is optional because it adds API calls.
- **Public holidays** are loaded per country from the free Nager.Date service (only year and country code are sent) and cached; countries it does not cover (for example India) show a note and can use manual dates.
- **Not in v1:** minimum payout per call-out, tiers, time off in lieu, raw-data snapshot files (use the JSON export), profile import/export files, and per-person price overrides. The what-if view re-prices the fetched data without new API calls.
