// Link / status window. Talks only to the tray process (typed commands),
// which talks to the local Varde service.
const { invoke } = window.__TAURI__.core;
const { listen } = window.__TAURI__.event;
const $ = (id) => document.getElementById(id);

const LINKED = ["connecting", "online", "offline"];
const opened = Date.now();
let defaults = { url: "https://varde.games", auto: false, relink: false };
let current = null;
let seen = false;
let started = false; // this window asked for a code
let closeTimer = null;

function show(view) {
  for (const s of document.querySelectorAll("main > section")) s.hidden = s.id !== `v-${view}`;
}

function formError(msg) {
  $("form-error").textContent = msg || "";
  $("form-error").hidden = !msg;
}

function describe(s) {
  if (s.group_name && s.node_name) return `Linked to ${s.group_name} as ${s.node_name}.`;
  if (s.group_name) return `Linked to ${s.group_name}.`;
  return "Linked. Connecting to Varde…";
}

function render(s) {
  current = s;
  if (!s) {
    // the tray connects within moments of starting
    if (!seen && Date.now() - opened < 3000) return setTimeout(() => render(current), 500);
    return show("noagent");
  }
  seen = true;
  if (s.state === "linking" && s.user_code) {
    started = true;
    $("code").textContent = s.user_code;
    tickExpiry();
    return show("code");
  }
  if (started && LINKED.includes(s.state)) {
    $("done-text").textContent = describe(s);
    if (s.state === "online" && !closeTimer) closeTimer = setTimeout(close, 4000);
    return show("done");
  }
  if (s.state === "not_linked" || (defaults.relink && LINKED.includes(s.state))) {
    formError(s.link_error);
    return show("form");
  }
  if (LINKED.includes(s.state)) {
    $("linked-text").textContent = describe(s);
    return show("linked");
  }
  show("noagent");
}

function tickExpiry() {
  const s = current;
  if (!s || s.state !== "linking" || !s.expires_at_unix_ms) return;
  const left = Math.max(0, Math.round((s.expires_at_unix_ms - Date.now()) / 1000));
  const m = Math.floor(left / 60);
  const sec = String(left % 60).padStart(2, "0");
  $("expires").textContent = `Code expires in ${m}:${sec}`;
}
setInterval(tickExpiry, 1000);

async function link(url) {
  formError("");
  $("link-btn").disabled = true;
  try {
    started = true;
    render(await invoke("start_link", { url }));
  } catch (e) {
    started = false;
    formError(String(e));
    show("form");
  } finally {
    $("link-btn").disabled = false;
  }
}

function close() {
  invoke("close_window");
}

$("link-form").addEventListener("submit", (e) => {
  e.preventDefault();
  link($("url").value.trim());
});
$("copy").addEventListener("click", async () => {
  const code = $("code").textContent;
  try {
    await navigator.clipboard.writeText(code);
  } catch {
    const r = document.createRange();
    r.selectNodeContents($("code"));
    getSelection().removeAllRanges();
    getSelection().addRange(r);
    document.execCommand("copy");
  }
  $("copy").textContent = "Copied";
  setTimeout(() => ($("copy").textContent = "Copy code"), 1500);
});
$("reopen").addEventListener("click", () => current && invoke("open_url", { url: current.link_url }));
$("cancel").addEventListener("click", async () => {
  started = false;
  await invoke("cancel_link").catch(() => {});
  show("form");
});
$("open-dash").addEventListener("click", () => current && invoke("open_url", { url: current.control_plane_url }));
for (const b of document.querySelectorAll("button.close")) b.addEventListener("click", close);

(async () => {
  defaults = await invoke("link_defaults");
  $("url").value = defaults.url;
  if (defaults.relink) {
    $("form-title").textContent = "Re-link this PC";
    $("relink-note").hidden = false;
  }
  await listen("status", (e) => render(e.payload));
  const s = await invoke("get_status");
  render(s);
  if (defaults.auto && s && s.state === "not_linked") link(defaults.url);
})();
