"use strict";
const $ = (id) => document.getElementById(id);
let info, token = "";
const sei = (wei) => (Number(BigInt(wei) / 10n ** 14n) / 1e4).toString();
const show = (text) => { $("status").textContent = text; };

async function load() {
  info = await (await fetch("/api/info")).json();
  document.title = `${info.chainName} faucet`;
  $("title").textContent = `${info.chainName} faucet`;
  $("meta").textContent = `${sei(info.amountWei)} SEI per address every ${info.addressCooldownSeconds / 3600}h · chain ${info.chainId} · faucet ${info.faucetAddress}`;
  $("add").hidden = !info.publicRpcUrl;
  const src = {
    turnstile: "https://challenges.cloudflare.com/turnstile/v0/api.js",
    hcaptcha: "https://js.hcaptcha.com/1/api.js",
  }[info.captchaProvider];
  if (!src) return;
  const div = document.createElement("div");
  div.className = info.captchaProvider === "turnstile" ? "cf-turnstile" : "h-captcha";
  div.dataset.sitekey = info.captchaSiteKey;
  div.dataset.callback = "onCaptcha";
  $("captcha").appendChild(div);
  const s = document.createElement("script");
  s.src = src;
  s.async = true;
  document.head.appendChild(s);
}

window.onCaptcha = (t) => { token = t; };

$("form").onsubmit = async (e) => {
  e.preventDefault();
  show("Submitting…");
  const r = await fetch("/api/claims", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ address: $("address").value.trim(), captchaToken: token }),
  });
  const body = await r.json();
  if (!r.ok) { show(body.message || body.error); return; }
  poll(body.id);
};

async function poll(id) {
  for (;;) {
    const c = await (await fetch(`/api/claims/${encodeURIComponent(id)}`)).json();
    show(`Status: ${c.status}` + (c.txHash ? `\nTx: ${c.txHash}` : "") + (c.error ? `\n${c.error}` : ""));
    if (c.status === "confirmed" || c.status === "failed") return;
    await new Promise((r) => setTimeout(r, 1000));
  }
}

$("add").onclick = async () => {
  if (!window.ethereum) { show("No browser wallet found"); return; }
  await window.ethereum.request({ method: "wallet_addEthereumChain", params: [{
    chainId: "0x" + Number(info.chainId).toString(16),
    chainName: info.chainName,
    nativeCurrency: { name: "SEI", symbol: "SEI", decimals: 18 },
    rpcUrls: [info.publicRpcUrl],
  }] });
};

load();
