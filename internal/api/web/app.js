"use strict";
const $ = (id) => document.getElementById(id);
let info, widget, token = "", polling = 0;
const sei = (wei) => (Number(BigInt(wei) / 10n ** 14n) / 1e4).toString();
function show(text, kind = "info", hash = "") {
  const el = $("status");
  el.textContent = text;
  el.dataset.kind = kind;
  if (hash) {
    const h = document.createElement("span");
    h.className = "mono";
    h.textContent = hash;
    el.appendChild(h);
  }
}

async function load() {
  info = await (await fetch("/api/info")).json();
  document.title = `${info.chainName} faucet`;
  $("title").textContent = `${info.chainName} faucet`;
  $("network").textContent = info.chainName;
  $("amount").textContent = `${sei(info.amountWei)} SEI`;
  $("cooldown").textContent = `1 claim per address every ${info.addressCooldownSeconds / 3600}h`;
  $("chain").textContent = info.chainId;
  $("faucet").textContent = info.faucetAddress;
  $("details").hidden = false;
  $("add").textContent = `Add ${info.chainName} to wallet`;
  $("add").hidden = !info.publicRpcUrl;
  const src = {
    turnstile: "https://challenges.cloudflare.com/turnstile/v0/api.js",
    hcaptcha: "https://js.hcaptcha.com/1/api.js",
  }[info.captchaProvider];
  if (!src) return;
  widget = document.createElement("div");
  widget.className = info.captchaProvider === "turnstile" ? "cf-turnstile" : "h-captcha";
  widget.dataset.sitekey = info.captchaSiteKey;
  widget.dataset.callback = "onCaptcha";
  widget.dataset.expiredCallback = "onCaptchaExpired";
  $("captcha").appendChild(widget);
  const s = document.createElement("script");
  s.src = src;
  s.async = true;
  document.head.appendChild(s);
}

window.onCaptcha = (t) => { token = t; };
window.onCaptchaExpired = () => { token = ""; };
$("address").oninvalid = () => show("");

// Captcha tokens are single-use, so every submit needs a fresh one.
function resetCaptcha() {
  token = "";
  if (info.captchaProvider === "turnstile") window.turnstile?.reset(widget);
  if (info.captchaProvider === "hcaptcha") window.hcaptcha?.reset();
}

$("form").onsubmit = async (e) => {
  e.preventDefault();
  const id = ++polling;
  show("Submitting…");
  try {
    const r = await fetch("/api/claims", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ address: $("address").value.trim(), captchaToken: token }),
    });
    const body = await r.json();
    if (id !== polling) return;
    if (!r.ok) { show(body.message || body.error, "error"); return; }
    poll(id, body.id);
  } catch {
    if (id === polling) show("Faucet unreachable, try again shortly", "error");
  } finally {
    resetCaptcha();
  }
};

async function poll(id, claim) {
  while (id === polling) {
    try {
      const r = await fetch(`/api/claims/${encodeURIComponent(claim)}`);
      if (id !== polling) return;
      if (r.status === 404) { show("Claim not found", "error"); return; }
      if (r.ok) {
        const c = await r.json();
        const kind = { confirmed: "ok", failed: "error" }[c.status] || "info";
        const text = { queued: "Queued, sending shortly…", sent: "Sent, waiting for confirmation…",
          confirmed: `Sent ${sei(c.amountWei)} SEI`, failed: `Failed: ${c.error}` }[c.status] || `Status: ${c.status}`;
        show(text, kind, c.txHash ? `Tx ${c.txHash}` : "");
        if (c.status === "confirmed" || c.status === "failed") return;
      }
    } catch {
      // Transient network or proxy error: keep polling.
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
}

$("add").onclick = async () => {
  if (!window.ethereum) { show("No browser wallet found", "error"); return; }
  await window.ethereum.request({ method: "wallet_addEthereumChain", params: [{
    chainId: "0x" + Number(info.chainId).toString(16),
    chainName: info.chainName,
    nativeCurrency: { name: "SEI", symbol: "SEI", decimals: 18 },
    rpcUrls: [info.publicRpcUrl],
  }] });
};

load();
