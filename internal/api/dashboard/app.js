/* -------------------------------------------------------------------

                      🗲 Storm Software - Mindctl

 This code was released as part of the Mindctl project. Mindctl
 is maintained by Storm Software under the Apache-2.0 license, and is
 free for commercial and private use. For more information, please visit
 our licensing page at https://stormsoftware.com/licenses/projects/mindctl.

 Website:                  https://stormsoftware.com
 Repository:               https://github.com/storm-software/mindctl
 Documentation:            https://docs.stormsoftware.com/projects/mindctl
 Contact:                  https://stormsoftware.com/contact

 SPDX-License-Identifier:  Apache-2.0

 ------------------------------------------------------------------- */

"use strict";

const TOKEN_KEY = "mindctl.management-token";
const RANGES = {
  "24h": { range: "24h", bucket: "1h", rangeMs: 24 * 3600e3, bucketMs: 3600e3 },
  "7d": {
    range: "168h",
    bucket: "6h",
    rangeMs: 7 * 24 * 3600e3,
    bucketMs: 6 * 3600e3
  },
  "30d": {
    range: "720h",
    bucket: "24h",
    rangeMs: 30 * 24 * 3600e3,
    bucketMs: 24 * 3600e3
  },
  "90d": {
    range: "2160h",
    bucket: "24h",
    rangeMs: 90 * 24 * 3600e3,
    bucketMs: 24 * 3600e3
  }
};
const TABS = ["usage", "accounts", "models", "config"];
const SERIES_SLOTS = 8;

const state = {
  token: readToken(),
  range: "24h",
  metric: "attempts",
  models: null,
  report: null
};

const $ = id => document.getElementById(id);
const integer = new Intl.NumberFormat();
const compact = new Intl.NumberFormat(undefined, {
  notation: "compact",
  maximumFractionDigits: 1
});

function readToken() {
  try {
    return localStorage.getItem(TOKEN_KEY) || "";
  } catch {
    return "";
  }
}
function writeToken(value) {
  try {
    value
      ? localStorage.setItem(TOKEN_KEY, value)
      : localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* per-tab only */
  }
}

// el builds DOM from text only, so API values are never parsed as HTML.
function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : String(child));
  }
  return node;
}
function svg(tag, attrs = {}) {
  const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [key, value] of Object.entries(attrs))
    node.setAttribute(key, value);
  return node;
}

async function api(path, options = {}) {
  const response = await fetch(`/v8/management/${path}`, {
    ...options,
    headers: {
      Authorization: `Bearer ${state.token}`,
      ...(options.headers || {})
    }
  });
  if (response.status === 401) {
    signOut("The management token was rejected.");
    throw new Error("unauthorized");
  }
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    try {
      message = (await response.json()).error.message || message;
    } catch {
      /* keep status text */
    }
    throw new Error(message);
  }
  return response;
}

function signOut(message = "") {
  state.token = "";
  state.models = null;
  writeToken("");
  $("login-error").textContent = message;
  route();
}

// ---------- formatting ----------

function usd(value) {
  const sign = value < 0 ? "-" : "";
  const amount = Math.abs(value);

  return `${sign}$${
    amount !== 0 && amount < 1 ? amount.toFixed(4) : amount.toFixed(2)
  }`;
}
function duration(ms) {
  if (!ms) return "—";
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.round(ms)} ms`;
}
function percent(part, whole) {
  return whole > 0 ? `${((part / whole) * 100).toFixed(1)}%` : "—";
}
function metricText(metric, value) {
  if (metric === "cost") return usd(value);
  return value >= 10000 ? compact.format(value) : integer.format(value);
}
function bucketLabel(ms, bucketMs, long) {
  const date = new Date(ms);
  if (bucketMs >= 24 * 3600e3) {
    return date.toLocaleDateString(
      undefined,
      long
        ? { weekday: "short", month: "short", day: "numeric" }
        : { month: "short", day: "numeric" }
    );
  }
  const time = date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit"
  });
  if (!long && bucketMs < 6 * 3600e3) return time;
  const day = date.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric"
  });
  if (!long) return date.getHours() === 0 ? day : time;
  const end = new Date(ms + bucketMs).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit"
  });

  return `${day}, ${time}–${end}`;
}

// ---------- provider colors ----------

// Providers keep their catalog order, so a color follows its provider across
// ranges instead of its rank. Past eight slots, providers fold into "other".
function providerOrder(extra = []) {
  const order = [];
  for (const model of state.models || [])
    if (!order.includes(model.Provider)) order.push(model.Provider);
  for (const provider of [...new Set(extra)].sort())
    if (!order.includes(provider)) order.push(provider);
  return order;
}
function seriesClass(provider, order) {
  const index = order.indexOf(provider);

  return index >= 0 && index < SERIES_SLOTS ? `s${index + 1}` : "s-other";
}
function seriesKey(provider, order) {
  const index = order.indexOf(provider);

  return index >= 0 && index < SERIES_SLOTS ? provider : "other";
}
function swatch(provider, order) {
  return el("span", {
    class: `swatch ${seriesClass(provider, order)}`,
    "aria-hidden": "true"
  });
}

// ---------- aggregation ----------

function blank() {
  return {
    attempts: 0,
    auto: 0,
    succeeded: 0,
    failed: 0,
    completed: 0,
    latency: 0,
    input: 0,
    cached: 0,
    written: 0,
    output: 0,
    tokens: 0,
    cost: 0,
    baseline: 0,
    compression: 0,
    providers: new Set()
  };
}
function add(sum, row) {
  sum.attempts += row.Attempts;
  sum.auto += row.AutoAttempts;
  if (row.Status === "succeeded") sum.succeeded += row.Attempts;
  if (row.Status === "failed") sum.failed += row.Attempts;
  sum.completed += row.Completed;
  sum.latency += row.LatencyMillis;
  sum.input += row.InputTokens;
  sum.cached += row.CachedInputTokens;
  sum.written += row.CacheWriteInputTokens;
  sum.output += row.OutputTokens;
  sum.tokens += row.TotalTokens;
  sum.cost += row.ActualCost;
  sum.baseline += row.BaselineCost;
  sum.compression += row.CompressionSavings;
  sum.providers.add(row.Provider);
  return sum;
}
const saved = sum => sum.baseline - sum.cost + sum.compression;
function groupBy(rows, key) {
  const groups = new Map();
  for (const row of rows) {
    const id = key(row);
    if (!groups.has(id)) groups.set(id, { row, sum: blank() });
    add(groups.get(id).sum, row);
  }
  return [...groups.values()];
}

// ---------- usage tab ----------

async function loadUsage() {
  const range = RANGES[state.range];
  const offset = -new Date().getTimezoneOffset() * 60;
  try {
    if (!state.models)
      state.models = (await (await api("models")).json()).models;
    state.report = await (
      await api(
        `usage?range=${range.range}&bucket=${range.bucket}&offset=${offset}`
      )
    ).json();
    state.loadedAt = Date.now();
    $("usage-error").textContent = "";
    renderUsage();
  } catch (error) {
    if (error.message !== "unauthorized")
      $("usage-error").textContent = `Could not load usage: ${error.message}`;
  }
}

function renderUsage() {
  const { Rows: rows, Recent: recent } = state.report;
  const order = providerOrder(rows.map(row => row.Provider));
  const total = rows.reduce(add, blank());
  $("updated").textContent = `Updated ${new Date(
    state.loadedAt
  ).toLocaleTimeString()} · refreshes every 30 s`;
  renderKpis(total);
  renderChart(rows, order);
  renderByModel(rows, order);
  renderByAccount(rows, order);
  renderRecent(recent, order);
}

function renderKpis(total) {
  const tile = (label, value, sub) =>
    el(
      "div",
      { class: "kpi" },
      el("div", { class: "kpi-label" }, label),
      el("div", { class: "kpi-value" }, value),
      el("div", { class: "kpi-sub" }, sub)
    );
  const finished = total.succeeded + total.failed;
  const savings = saved(total);
  $("kpis").replaceChildren(
    tile(
      "Requests",
      integer.format(total.attempts),
      `${integer.format(total.auto)} auto-routed`
    ),
    tile(
      "Success rate",
      percent(total.succeeded, finished),
      `${integer.format(total.failed)} failed`
    ),
    tile(
      "Tokens",
      compact.format(total.tokens),
      `${compact.format(total.input)} in · ${compact.format(total.output)} out · ${compact.format(total.cached)} cached`
    ),
    tile("Cost", usd(total.cost), "estimated from configured prices"),
    tile(
      "Saved",
      usd(savings),
      `${percent(savings, total.baseline + total.compression)} vs baseline`
    ),
    tile(
      "Avg latency",
      duration(total.completed ? total.latency / total.completed : 0),
      "per completed request"
    )
  );
}

function renderChart(rows, order) {
  const container = $("chart");
  const range = RANGES[state.range];
  const metric = state.metric;
  const offsetMs = -new Date().getTimezoneOffset() * 60e3;
  // Matches the server's bucket alignment so empty windows still render.
  const now = Date.now();
  const first =
    Math.floor((now - range.rangeMs + offsetMs) / range.bucketMs) *
      range.bucketMs -
    offsetMs;
  const buckets = [];
  for (let start = first; start <= now; start += range.bucketMs)
    buckets.push({ start, values: new Map(), total: 0 });
  const index = new Map(buckets.map(bucket => [bucket.start, bucket]));
  const keys = [];
  for (const row of rows) {
    const bucket = index.get(Date.parse(row.Bucket));
    if (!bucket) continue;
    const key = seriesKey(row.Provider, order);
    if (!keys.includes(key)) keys.push(key);
    const value =
      metric === "attempts"
        ? row.Attempts
        : metric === "tokens"
          ? row.TotalTokens
          : row.ActualCost;
    bucket.values.set(key, (bucket.values.get(key) || 0) + value);
    bucket.total += value;
  }
  // Stack in catalog order, "other" last.
  keys.sort(
    (a, b) =>
      (a === "other") - (b === "other") || order.indexOf(a) - order.indexOf(b)
  );
  const label = key => (key === "other" ? "Other" : key);
  const keyClass = key =>
    key === "other" ? "s-other" : seriesClass(key, order);

  $("legend").replaceChildren(
    ...(keys.length > 1
      ? keys.map(key =>
          el(
            "span",
            {},
            el("span", {
              class: `swatch ${keyClass(key)}`,
              "aria-hidden": "true"
            }),
            label(key)
          )
        )
      : [])
  );

  const width = Math.max(container.clientWidth, 280);
  const height = 220;
  const margin = { top: 8, right: 4, bottom: 22, left: 52 };
  const plotWidth = width - margin.left - margin.right;
  const plotHeight = height - margin.top - margin.bottom;
  const max = Math.max(0, ...buckets.map(bucket => bucket.total));
  const step = niceStep(max / 4 || 1);
  const top = Math.max(step, Math.ceil(max / step) * step);
  const y = value => margin.top + plotHeight - (value / top) * plotHeight;

  const root = svg("svg", {
    viewBox: `0 0 ${width} ${height}`,
    role: "img",
    "aria-label": `${$("metric").querySelector("[aria-pressed=true]").textContent} per ${range.bucket} bucket, stacked by provider. See the By model table for values.`
  });
  for (let tick = 0; tick <= top + step / 2; tick += step) {
    const ty = y(tick);
    root.append(
      svg("line", {
        class: tick === 0 ? "baseline" : "grid",
        x1: margin.left,
        x2: width - margin.right,
        y1: ty,
        y2: ty
      })
    );
    const text = svg("text", {
      x: margin.left - 8,
      y: ty + 4,
      "text-anchor": "end"
    });
    text.textContent = metricText(metric, tick);
    root.append(text);
  }

  const band = plotWidth / buckets.length;
  const barWidth = Math.max(1, Math.min(28, band * 0.72));
  const labelEvery = Math.ceil(
    buckets.length / Math.max(2, Math.floor(plotWidth / 72))
  );
  const tooltip = el("div", { class: "tooltip", hidden: true });

  buckets.forEach((bucket, position) => {
    const x = margin.left + position * band + (band - barWidth) / 2;
    let base = margin.top + plotHeight;
    const present = keys.filter(key => bucket.values.get(key) > 0);
    present.forEach((key, stackIndex) => {
      const full = (bucket.values.get(key) / top) * plotHeight;
      const isTop = stackIndex === present.length - 1;
      // A 2px surface gap separates stacked segments.
      const h = Math.max(isTop ? full : full - 2, 1);
      const yTop = base - h;
      root.append(
        isTop
          ? roundedTop(x, yTop, barWidth, h, keyClass(key))
          : svg("rect", {
              class: keyClass(key),
              x,
              y: yTop,
              width: barWidth,
              height: h
            })
      );
      base -= full;
    });
    if (position % labelEvery === 0) {
      const text = svg("text", {
        x: x + barWidth / 2,
        y: height - 6,
        "text-anchor": "middle"
      });
      text.textContent = bucketLabel(bucket.start, range.bucketMs, false);
      root.append(text);
    }
    const hit = svg("rect", {
      class: "hit",
      x: margin.left + position * band,
      y: margin.top,
      width: band,
      height: plotHeight
    });
    hit.addEventListener("pointerenter", () => {
      tooltip.replaceChildren(
        el(
          "div",
          { class: "tooltip-title" },
          bucketLabel(bucket.start, range.bucketMs, true)
        ),
        ...[...present].reverse().map(key =>
          el(
            "div",
            { class: "tooltip-row" },
            el("span", {
              class: `swatch ${keyClass(key)}`,
              "aria-hidden": "true"
            }),
            el("span", {}, label(key)),
            el("span", {}, metricText(metric, bucket.values.get(key)))
          )
        ),
        el(
          "div",
          { class: "tooltip-row tooltip-total" },
          el("span", {}, "Total"),
          el("span", {}, metricText(metric, bucket.total))
        )
      );
      tooltip.hidden = false;
      // Beside the bar, flipping left near the right edge, so it never hides the bar.
      const scale = container.clientWidth / width;
      const right = (margin.left + (position + 1) * band) * scale + 8;
      const left =
        (margin.left + position * band) * scale - 8 - tooltip.offsetWidth;
      tooltip.style.left = `${Math.max(
        0,
        right + tooltip.offsetWidth <= container.clientWidth ? right : left
      )}px`;
      tooltip.style.top = "0px";
    });
    hit.addEventListener("pointerleave", () => {
      tooltip.hidden = true;
    });
    root.insertBefore(hit, root.firstChild);
  });

  if (max === 0) {
    const text = svg("text", {
      class: "empty",
      x: margin.left + plotWidth / 2,
      y: margin.top + plotHeight / 2,
      "text-anchor": "middle"
    });
    text.textContent = "No requests in this range";
    root.append(text);
  }
  container.replaceChildren(root, tooltip);
}

function niceStep(raw) {
  const power = 10 ** Math.floor(Math.log10(raw));
  const fraction = raw / power;

  return (
    (fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10) * power
  );
}

function roundedTop(x, y, w, h, className) {
  const r = Math.min(4, w / 2, h);

  return svg("path", {
    class: className,
    d: `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`
  });
}

function table(headers, rows, emptyText) {
  return el(
    "table",
    {},
    el(
      "thead",
      {},
      el(
        "tr",
        {},
        headers.map(([text, numeric]) =>
          el("th", { class: numeric ? "num" : undefined, scope: "col" }, text)
        )
      )
    ),
    el(
      "tbody",
      {},
      rows.length
        ? rows.map(cells =>
            el(
              "tr",
              {},
              cells.map((cell, index) =>
                el("td", { class: headers[index][1] ? "num" : undefined }, cell)
              )
            )
          )
        : el(
            "tr",
            {},
            el("td", { class: "empty-row", colspan: headers.length }, emptyText)
          )
    )
  );
}

function modelCell(provider, model, order) {
  return el(
    "span",
    { class: "cell" },
    swatch(provider, order),
    `${provider}/${model}`
  );
}

function renderByModel(rows, order) {
  const groups = groupBy(
    rows,
    row => `${row.Provider}\u0000${row.ModelID}`
  ).sort((a, b) => b.sum.cost - a.sum.cost || b.sum.attempts - a.sum.attempts);
  $("by-model").replaceChildren(
    table(
      [
        ["Model"],
        ["Requests", 1],
        ["Success", 1],
        ["Avg latency", 1],
        ["Input", 1],
        ["Cached", 1],
        ["Output", 1],
        ["Cost", 1],
        ["Saved", 1]
      ],
      groups.map(({ row, sum }) => [
        modelCell(row.Provider, row.ModelID, order),
        integer.format(sum.attempts),
        percent(sum.succeeded, sum.succeeded + sum.failed),
        duration(sum.completed ? sum.latency / sum.completed : 0),
        integer.format(sum.input),
        integer.format(sum.cached),
        integer.format(sum.output),
        usd(sum.cost),
        usd(saved(sum))
      ]),
      "No requests in this range"
    )
  );
}

function renderByAccount(rows, order) {
  const groups = groupBy(rows, row => row.AccountID).sort(
    (a, b) => b.sum.attempts - a.sum.attempts
  );
  $("by-account").replaceChildren(
    table(
      [
        ["Account"],
        ["Providers"],
        ["Requests", 1],
        ["Failed", 1],
        ["Tokens", 1],
        ["Cost", 1]
      ],
      groups.map(({ row, sum }) => [
        row.AccountID || el("span", { class: "muted" }, "unassigned"),
        el(
          "span",
          { class: "cell" },
          [...sum.providers].map(provider =>
            el(
              "span",
              { class: "cell" },
              swatch(provider, order),
              `${provider} `
            )
          )
        ),
        integer.format(sum.attempts),
        integer.format(sum.failed),
        integer.format(sum.tokens),
        usd(sum.cost)
      ]),
      "No requests in this range"
    )
  );
}

function statusBadge(status) {
  const tone =
    status === "succeeded"
      ? "good"
      : status === "failed"
        ? "critical"
        : "neutral";

  return el(
    "span",
    { class: `status ${tone}` },
    status === "started" ? "in flight" : status
  );
}

function renderRecent(recent, order) {
  $("recent").replaceChildren(
    table(
      [
        ["Time"],
        ["Model"],
        ["Tier"],
        ["Routing"],
        ["Account"],
        ["Status"],
        ["Latency", 1],
        ["Tokens", 1],
        ["Cost", 1]
      ],
      recent.map(attempt => [
        new Date(attempt.CreatedAt).toLocaleString(undefined, {
          month: "short",
          day: "numeric",
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit"
        }),
        modelCell(attempt.Provider, attempt.ModelID, order),
        attempt.Tier,
        attempt.Auto ? "auto" : "explicit",
        attempt.AccountID || el("span", { class: "muted" }, "—"),
        statusBadge(attempt.Status),
        duration(attempt.LatencyMillis),
        integer.format(attempt.TotalTokens),
        usd(attempt.ActualCost)
      ]),
      "No requests in this range"
    )
  );
}

// ---------- accounts, models, config ----------

async function loadAccounts() {
  try {
    const [credentials, quota] = await Promise.all([
      api("credentials").then(r => r.json()),
      api("quota").then(r => r.json())
    ]);
    const cooling = new Map(
      quota.accounts
        .filter(entry => entry.Cooling)
        .map(entry => [entry.ID, entry.Until])
    );
    const order = providerOrder(
      credentials.credentials.map(entry => entry.Provider)
    );
    const action = (label, path, body) =>
      el(
        "button",
        {
          type: "button",
          onclick: async event => {
            event.currentTarget.disabled = true;
            try {
              await api(path, {
                method: "POST",
                ...(body
                  ? {
                      body: JSON.stringify(body),
                      headers: { "Content-Type": "application/json" }
                    }
                  : {})
              });
              $("accounts-error").textContent = "";
            } catch (error) {
              if (error.message !== "unauthorized")
                $("accounts-error").textContent =
                  `${label} failed: ${error.message}`;
            }
            loadAccounts();
          }
        },
        label
      );
    $("credentials").replaceChildren(
      table(
        [
          ["Account"],
          ["Provider"],
          ["Kind"],
          ["Protocol"],
          ["Status"],
          ["Actions"]
        ],
        credentials.credentials.map(entry => {
          const id = encodeURIComponent(entry.ID);
          const until = cooling.get(entry.ID);
          const refreshable =
            (entry.Kind === "oauth" || entry.Kind === "service_account") &&
            !entry.ID.startsWith("config:");

          return [
            entry.ID,
            el(
              "span",
              { class: "cell" },
              swatch(entry.Provider, order),
              entry.Provider
            ),
            entry.Kind,
            entry.Protocol || "—",
            !entry.Enabled
              ? el("span", { class: "status neutral" }, "disabled")
              : until
                ? el(
                    "span",
                    { class: "status critical" },
                    `cooling until ${new Date(until).toLocaleTimeString()}`
                  )
                : el("span", { class: "status good" }, "ready"),
            el(
              "span",
              { class: "actions" },
              action(
                entry.Enabled ? "Disable" : "Enable",
                `credentials/${id}/${entry.Enabled ? "disable" : "enable"}`
              ),
              refreshable &&
                action("Refresh token", `credentials/${id}/refresh`),
              until &&
                action("Reset cooldown", "cooldown/reset", {
                  account_id: entry.ID
                })
            )
          ];
        }),
        "No credentials configured"
      )
    );
  } catch (error) {
    if (error.message !== "unauthorized")
      $("accounts-error").textContent =
        `Could not load credentials: ${error.message}`;
  }
}

async function loadModels() {
  try {
    state.models = (await (await api("models")).json()).models;
    const order = providerOrder();
    $("catalog").replaceChildren(
      table(
        [["Model"], ["Provider"], ["Tier"]],
        state.models.map(model => [
          model.ID,
          el(
            "span",
            { class: "cell" },
            swatch(model.Provider, order),
            model.Provider
          ),
          model.Tier
        ]),
        "No models configured"
      )
    );
  } catch (error) {
    if (error.message !== "unauthorized")
      $("models-error").textContent = `Could not load models: ${error.message}`;
  }
}

async function loadConfig() {
  try {
    $("config-yaml").textContent = await (await api("config")).text();
  } catch (error) {
    if (error.message !== "unauthorized")
      $("config-error").textContent = `Could not load config: ${error.message}`;
  }
}

// ---------- routing ----------

const loaders = {
  usage: loadUsage,
  accounts: loadAccounts,
  models: loadModels,
  config: loadConfig
};

function currentTab() {
  const tab = location.hash.slice(1);

  return TABS.includes(tab) ? tab : "usage";
}

function route() {
  const signedIn = Boolean(state.token);
  $("login").hidden = signedIn;
  $("signout").hidden = !signedIn;
  const tab = currentTab();
  for (const name of TABS) $(name).hidden = !signedIn || name !== tab;
  for (const link of document.querySelectorAll(".tabs a")) {
    if (link.getAttribute("href") === `#${tab}`)
      link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  if (signedIn) loaders[tab]();
  else $("token").focus();
}

function bindSegmented(id, key, onChange) {
  const group = $(id);
  const sync = () =>
    group
      .querySelectorAll("button")
      .forEach(button =>
        button.setAttribute(
          "aria-pressed",
          String(button.dataset.value === state[key])
        )
      );
  group.addEventListener("click", event => {
    const button = event.target.closest("button");
    if (!button) return;
    state[key] = button.dataset.value;
    sync();
    onChange();
  });
  sync();
}

bindSegmented("range", "range", loadUsage);
bindSegmented("metric", "metric", () => state.report && renderUsage());

$("login").addEventListener("submit", async event => {
  event.preventDefault();
  state.token = $("token").value.trim();
  $("token").value = "";
  try {
    await api("models");
    writeToken(state.token);
    $("login-error").textContent = "";
    route();
  } catch (error) {
    if (error.message !== "unauthorized")
      $("login-error").textContent =
        `Could not reach mindctl: ${error.message}`;
  }
});
$("signout").addEventListener("click", () => signOut());
window.addEventListener("hashchange", route);
new ResizeObserver(
  () =>
    state.report &&
    currentTab() === "usage" &&
    renderChart(
      state.report.Rows,
      providerOrder(state.report.Rows.map(row => row.Provider))
    )
).observe($("chart"));
setInterval(() => {
  if (
    state.token &&
    currentTab() === "usage" &&
    document.visibilityState === "visible"
  )
    loadUsage();
}, 30000);

route();
