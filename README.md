# On-Call Compensation Calculator

A single Go executable that pulls on-call time from PagerDuty, classifies every hour
(business, non-business, night, weekend, weekend night, holiday) in each responder's own time
zone, optionally prices it, and exports PDF, HTML, CSV, XLSX or JSON. See `PDR.md`.

## Run
    make build && ./oncallcalc          # opens the local web UI (localhost only)
    ./oncallcalc list
    ./oncallcalc run --report "Monthly payroll" --from 2026-09-01 --to 2026-09-30 --format pdf,csv --out ./out

## Build for all platforms
    make dist                           # macOS (arm64, amd64), Windows, Linux into dist/

## Test
    make test

**Use an Account API key** (PagerDuty → Integrations → API Access Keys, read-only), not a User API token.

API keys are stored in the OS credential store. Saved reports live in the user config folder.
`ONCALL_TEST_*` environment variables exist only for testing against a fake API.
