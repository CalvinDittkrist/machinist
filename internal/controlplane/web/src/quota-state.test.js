import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { formatPercent, quotaAssessmentRows, quotaWaitSummary, quotaWindowRows, runQuotaKind, workerQuotaSummary } from "./quota-state.js";

const now = new Date("2026-09-07T12:00:00Z");

const waitingRun = {
  state: "queued",
  provider: "claude",
  quota_wait: {
    code: "insufficient_quota",
    reason: "the claude session window has 12% remaining; this run needs 20% (the configured minimum reserve); the window resets at 2026-09-07T15:00:00Z",
    since: "2026-09-07T11:58:30Z",
    next_check_at: "2026-09-07T12:00:45Z",
    resets_at: "2026-09-07T15:00:00Z",
    observed_at: "2026-09-07T11:59:50Z",
    windows: [
      { window_id: "five_hour", kind: "session", label: "session", remaining_percent: 12, reserved_percent: 0, available_percent: 12, required_percent: 20, basis: "minimum_reserve", sufficient: false, resets_at: "2026-09-07T15:00:00Z" },
      { window_id: "seven_day", kind: "weekly", label: "week", remaining_percent: 80, reserved_percent: 20, available_percent: 60, required_percent: 14, basis: "history", samples: 3, sufficient: true, resets_at: "2026-09-13T10:00:00Z" },
    ],
  },
};

test("waiting runs are summarised with reason, next check, and reset", () => {
  assert.equal(runQuotaKind(waitingRun), "waiting");
  const summary = quotaWaitSummary(waitingRun, now);
  assert.equal(summary.label, "Waiting for quota");
  assert.equal(summary.title, "Insufficient quota");
  assert.match(summary.reason, /session window has 12% remaining/);
  assert.equal(summary.nextCheck, "in 45s");
  assert.equal(summary.waitingFor, "1m 30s");
  assert.equal(summary.resetsAt, "2026-09-07T15:00:00Z");
});

test("wait summaries distinguish evidence problems from insufficient headroom", () => {
  const cases = [
    ["no_observation", "No quota evidence"],
    ["auth_required", "Provider sign-in required"],
    ["provider_unavailable", "Provider quota unavailable"],
    ["observation_error", "Quota check failed"],
    ["stale_observation", "Quota evidence stale"],
    ["awaiting_reset_observation", "Waiting for post-reset evidence"],
    ["mystery", "Waiting for quota"],
  ];
  for (const [code, title] of cases) {
    const summary = quotaWaitSummary({ state: "queued", quota_wait: { code, reason: "r", next_check_at: "2026-09-07T11:59:00Z" } }, now);
    assert.equal(summary.title, title, code);
    assert.equal(summary.nextCheck, "now");
  }
});

test("assessment rows describe each binding window", () => {
  const rows = quotaAssessmentRows(waitingRun.quota_wait.windows);
  assert.deepEqual(rows[0], { id: "five_hour", label: "session", remaining: "12%", reserved: "0%", available: "12%", required: "20%", basis: "Minimum reserve", sufficient: false, resetsAt: "2026-09-07T15:00:00Z" });
  assert.equal(rows[1].basis, "History (3 runs) + safety reserve");
  assert.equal(rows[1].sufficient, true);
});

test("usage rows expose before, after, consumption, and measurement quality", () => {
  const measuredRun = { state: "succeeded", quota_usage: { before_at: "2026-09-07T11:00:00Z", after_at: "2026-09-07T11:30:00Z", overlapping: true, windows: [
    { window_id: "five_hour", kind: "session", label: "session", before_percent: 60, after_percent: 48.5, consumed_percent: 11.5, quality: "overlapping" },
    { window_id: "seven_day", kind: "weekly", label: "week", before_percent: 80, quality: "missing_after" },
    { window_id: "model:fable", kind: "model", label: "Fable week", before_percent: 80, after_percent: 99, quality: "reset" },
  ] } };
  assert.equal(runQuotaKind(measuredRun), "measured");
  const rows = quotaWindowRows(measuredRun.quota_usage);
  assert.deepEqual(rows[0], { id: "five_hour", label: "session", before: "60%", after: "48.5%", consumed: "11.5%", quality: "Overlapping account activity", reliable: false });
  assert.deepEqual(rows[1], { id: "seven_day", label: "week", before: "80%", after: "—", consumed: "—", quality: "No post-run observation", reliable: false });
  assert.equal(rows[2].quality, "Window reset during run");
  assert.equal(quotaWindowRows({ windows: [{ window_id: "x", label: "x", before_percent: 20, after_percent: 15, consumed_percent: 5, quality: "measured" }] })[0].reliable, true);
});

test("runs without quota data report no quota kind", () => {
  assert.equal(runQuotaKind({ state: "queued" }), "none");
  assert.equal(runQuotaKind({ state: "running", quota_reservation: { five_hour: 20 } }), "reserved");
  assert.equal(runQuotaKind({ state: "succeeded", quota_assessment: [{ window_id: "five_hour" }] }), "admitted");
});

test("worker quota observations are summarised per provider", () => {
  const fresh = workerQuotaSummary({ provider: "claude", status: "fresh", plan: "max", observed_at: "2026-09-07T11:59:30Z", windows: [
    { id: "five_hour", kind: "session", label: "session", percent_remaining: 73 },
    { id: "seven_day", kind: "weekly", label: "week", percent_remaining: 87 },
    { id: "model:fable", kind: "model", label: "Fable week", percent_remaining: 76 },
  ] }, now);
  assert.deepEqual(fresh, { provider: "claude", tone: "success", label: "Fresh", detail: "session 73% · week 87% · Fable week 76%", plan: "max", observed: "30s ago" });
  const signIn = workerQuotaSummary({ provider: "copilot", status: "auth_required", error: "GitHub Copilot sign-in required", observed_at: "2026-09-07T11:59:30Z" }, now);
  assert.deepEqual(signIn, { provider: "copilot", tone: "danger", label: "Sign-in required", detail: "GitHub Copilot sign-in required", plan: "", observed: "30s ago" });
  assert.equal(workerQuotaSummary({ provider: "codex", status: "stale", observed_at: "2026-09-07T11:59:30Z" }, now).tone, "warning");
  assert.equal(workerQuotaSummary({ provider: "codex", status: "error", error: "quota-axi not found", observed_at: "2026-09-07T11:59:30Z" }, now).label, "Check failed");
  assert.equal(workerQuotaSummary({ provider: "codex", status: "unavailable", observed_at: "2026-09-07T11:59:30Z" }, now).label, "Unavailable");
});

test("percentages format compactly", () => {
  assert.equal(formatPercent(12), "12%");
  assert.equal(formatPercent(11.5), "11.5%");
  assert.equal(formatPercent(11.25), "11.3%");
  assert.equal(formatPercent(undefined), "—");
  assert.equal(formatPercent(null), "—");
});

test("dashboard surfaces quota waiting, admission, usage, and worker evidence", async () => {
  const main = await readFile(new URL("./main.jsx", import.meta.url), "utf8");
  const catalog = await readFile(new URL("./catalog.jsx", import.meta.url), "utf8");
  assert.match(main, /quotaWaitSummary\(/);
  assert.match(main, /QuotaSection/);
  assert.match(main, /quotaWindowRows\(/);
  assert.match(main, /quotaAssessmentRows\(/);
  assert.match(catalog, /workerQuotaSummary\(/);
});
