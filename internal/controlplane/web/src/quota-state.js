const zeroTime = "0001-01-01T00:00:00Z";

const waitTitles = {
  insufficient_quota: "Insufficient quota",
  no_observation: "No quota evidence",
  auth_required: "Provider sign-in required",
  provider_unavailable: "Provider quota unavailable",
  observation_error: "Quota check failed",
  stale_observation: "Quota evidence stale",
  awaiting_reset_observation: "Waiting for post-reset evidence",
};

const qualityLabels = {
  measured: "Measured",
  overlapping: "Overlapping account activity",
  reset: "Window reset during run",
  missing_before: "No pre-run observation",
  missing_after: "No post-run observation",
};

const statusPresentation = {
  fresh: { tone: "success", label: "Fresh" },
  stale: { tone: "warning", label: "Stale" },
  auth_required: { tone: "danger", label: "Sign-in required" },
  unavailable: { tone: "warning", label: "Unavailable" },
  error: { tone: "danger", label: "Check failed" },
};

export function runQuotaKind(run) {
  if (!run) return "none";
  if (run.quota_wait && run.state === "queued") return "waiting";
  if (run.quota_usage?.windows?.length) return "measured";
  if (run.quota_reservation && Object.keys(run.quota_reservation).length) return "reserved";
  if (run.quota_assessment?.length) return "admitted";
  return "none";
}

export function quotaWaitSummary(run, now = new Date()) {
  const wait = run?.quota_wait || {};
  return {
    label: "Waiting for quota",
    title: waitTitles[wait.code] || "Waiting for quota",
    code: wait.code || "",
    reason: wait.reason || "",
    nextCheck: relativeFuture(wait.next_check_at, now),
    waitingFor: elapsedSince(wait.since, now),
    resetsAt: validDate(wait.resets_at) ? wait.resets_at : "",
    observedAt: validDate(wait.observed_at) ? wait.observed_at : "",
  };
}

export function quotaAssessmentRows(windows = []) {
  return windows.map((window) => ({
    id: window.window_id,
    label: window.label || window.window_id,
    remaining: formatPercent(window.remaining_percent),
    reserved: formatPercent(window.reserved_percent),
    available: formatPercent(window.available_percent),
    required: formatPercent(window.required_percent),
    basis: window.basis === "history" ? `History (${window.samples || 0} runs) + safety reserve` : "Minimum reserve",
    sufficient: Boolean(window.sufficient),
    resetsAt: validDate(window.resets_at) ? window.resets_at : "",
  }));
}

export function quotaWindowRows(usage) {
  return (usage?.windows || []).map((window) => ({
    id: window.window_id,
    label: window.label || window.window_id,
    before: formatPercent(window.before_percent),
    after: formatPercent(window.after_percent),
    consumed: formatPercent(window.consumed_percent),
    quality: qualityLabels[window.quality] || String(window.quality || "unknown").replaceAll("_", " "),
    reliable: window.quality === "measured",
  }));
}

export function workerQuotaSummary(observation, now = new Date()) {
  const presentation = statusPresentation[observation?.status] || { tone: "neutral", label: String(observation?.status || "unknown") };
  let detail = observation?.error || "";
  if (observation?.status === "fresh") {
    detail = (observation.windows || []).map((window) => `${window.label || window.id} ${formatPercent(window.percent_remaining)}`).join(" · ") || "No windows reported";
  }
  return {
    provider: observation?.provider || "unknown",
    tone: presentation.tone,
    label: presentation.label,
    detail,
    plan: observation?.plan || "",
    observed: elapsedSince(observation?.observed_at, now) === "" ? "unknown" : `${elapsedSince(observation?.observed_at, now)} ago`,
  };
}

export function formatPercent(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  const rounded = Math.round(value * 10) / 10;
  return `${Number.isInteger(rounded) ? rounded : rounded.toFixed(1)}%`;
}

function relativeFuture(value, now) {
  if (!validDate(value)) return "";
  const seconds = Math.floor((Date.parse(value) - now.getTime()) / 1000);
  if (seconds <= 0) return "now";
  return `in ${formatSeconds(seconds)}`;
}

function elapsedSince(value, now) {
  if (!validDate(value)) return "";
  return formatSeconds(Math.max(0, Math.floor((now.getTime() - Date.parse(value)) / 1000)));
}

function formatSeconds(seconds) {
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  if (minutes < 60) return remainder ? `${minutes}m ${remainder}s` : `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

function validDate(value) {
  return Boolean(value && value !== zeroTime && Number.isFinite(Date.parse(value)));
}
