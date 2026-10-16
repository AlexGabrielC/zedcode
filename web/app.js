"use strict";
// Chiffrement de bout en bout : la clé AES-256 est dans le fragment d'URL
// (#...). Le navigateur ne l'envoie jamais au serveur, qui ne voit que du
// texte chiffré.
(() => {
  const $ = (id) => document.getElementById(id);
  const editor = $("editor");
  const enc = new TextEncoder();
  const dec = new TextDecoder();

  const clientId = b64url(crypto.getRandomValues(new Uint8Array(9)));
  let roomId = null;
  let key = null;
  let version = -1;
  let dirty = false;
  let sending = false;
  let sendTimer = null;
  let queue = Promise.resolve();

  function b64url(bytes) {
    let s = "";
    for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function unb64url(str) {
    const pad = "===".slice((str.length + 3) % 4);
    const s = atob(str.replace(/-/g, "+").replace(/_/g, "/") + pad);
    const out = new Uint8Array(s.length);
    for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
    return out;
  }

  async function encrypt(text) {
    const iv = crypto.getRandomValues(new Uint8Array(12));
    const ct = new Uint8Array(await crypto.subtle.encrypt(
      { name: "AES-GCM", iv, additionalData: enc.encode(roomId) }, key, enc.encode(text)));
    const out = new Uint8Array(iv.length + ct.length);
    out.set(iv);
    out.set(ct, iv.length);
    return b64url(out);
  }

  async function decrypt(data) {
    const raw = unb64url(data);
    const pt = await crypto.subtle.decrypt(
      { name: "AES-GCM", iv: raw.subarray(0, 12), additionalData: enc.encode(roomId) },
      key, raw.subarray(12));
    return dec.decode(pt);
  }

  function setStatus(text, kind = "") {
    const el = $("status");
    el.textContent = text;
    el.className = "status " + kind;
  }

  function fatal(msg) {
    const el = $("fatal");
    el.textContent = msg;
    el.hidden = false;
    editor.hidden = true;
  }

  const hasPending = () => dirty || sending || sendTimer !== null;

  function applyRemote(text) {
    if (text === editor.value) return;
    const { selectionStart: s, selectionEnd: e, scrollTop } = editor;
    editor.value = text;
    editor.setSelectionRange(Math.min(s, text.length), Math.min(e, text.length));
    editor.scrollTop = scrollTop;
  }

  async function onDoc(ev) {
    if (ev.v === version) return;
    version = ev.v;
    // Nos propres échos, ou une frappe locale en cours (dernier écrit gagne).
    if (ev.f === clientId || hasPending()) return;
    if (!ev.d) return;
    try {
      applyRemote(await decrypt(ev.d));
      setStatus("synchronisé", "ok");
    } catch {
      setStatus("déchiffrement impossible (mauvaise clé ?)", "err");
    }
  }

  function connect() {
    const es = new EventSource(`/api/rooms/${roomId}/events`);
    es.onopen = () => setStatus("connecté", "ok");
    es.onerror = () => setStatus("reconnexion…", "warn"); // EventSource se reconnecte seul
    es.addEventListener("doc", (e) => {
      const ev = JSON.parse(e.data);
      queue = queue.then(() => onDoc(ev)); // déchiffrements dans l'ordre d'arrivée
    });
    es.addEventListener("presence", (e) => {
      const { n } = JSON.parse(e.data);
      $("peers").textContent = n > 1 ? `${n} appareils connectés` : "seul ici";
    });
  }

  function schedule() {
    dirty = true;
    clearTimeout(sendTimer);
    sendTimer = setTimeout(flush, 300);
  }

  async function flush() {
    sendTimer = null;
    if (sending) return; // la boucle en cours reprendra `dirty`
    sending = true;
    try {
      while (dirty) {
        dirty = false;
        const d = await encrypt(editor.value);
        const res = await fetch(`/api/rooms/${roomId}`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ d, f: clientId }),
        });
        if (res.status === 401) { location.reload(); return; }
        if (!res.ok) {
          setStatus(`erreur ${res.status} : ${(await res.text()).trim()}`, "err");
          dirty = true;
          if (res.status !== 413 && res.status !== 507) sendTimer = setTimeout(flush, 3000);
          return;
        }
        setStatus("enregistré", "ok");
      }
    } catch {
      setStatus("hors ligne, nouvel essai…", "warn");
      dirty = true;
      sendTimer = setTimeout(flush, 3000);
    } finally {
      sending = false;
    }
  }

  function newRoomURL() {
    const id = b64url(crypto.getRandomValues(new Uint8Array(16)));
    const k = b64url(crypto.getRandomValues(new Uint8Array(32)));
    return `/r/${id}#${k}`;
  }

  async function openRoom() {
    const m = location.pathname.match(/^\/r\/([A-Za-z0-9_-]{16,64})$/);
    if (!m) { location.replace(newRoomURL()); return; }
    roomId = m[1];

    let raw;
    try { raw = unb64url(location.hash.slice(1)); } catch { raw = null; }
    if (!raw || raw.length !== 32) {
      fatal("Lien incomplet ou clé invalide : il manque la partie après « # ».");
      return;
    }
    key = await crypto.subtle.importKey("raw", raw, "AES-GCM", false, ["encrypt", "decrypt"]);

    $("actions").hidden = false;
    editor.hidden = false;
    editor.focus();
    connect();
  }

  editor.addEventListener("input", schedule);
  editor.addEventListener("keydown", (e) => {
    if (e.key === "Tab" && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
      e.preventDefault();
      editor.setRangeText("\t", editor.selectionStart, editor.selectionEnd, "end");
      schedule();
    }
  });

  $("btn-new").addEventListener("click", () => { location.href = newRoomURL(); });
  $("btn-copy").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(location.href);
      setStatus("lien copié", "ok");
    } catch {
      setStatus("copie refusée par le navigateur", "err");
    }
  });
  $("btn-download").addEventListener("click", () => {
    const url = URL.createObjectURL(new Blob([editor.value], { type: "text/plain" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `zedcode-${roomId.slice(0, 8)}.txt`;
    a.click();
    URL.revokeObjectURL(url);
  });
  $("btn-logout").addEventListener("click", async () => {
    await fetch("/api/logout", { method: "POST" });
    location.reload();
  });

  $("login").addEventListener("submit", async (e) => {
    e.preventDefault();
    const res = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: $("token").value }),
    });
    if (res.ok) {
      $("login").hidden = true;
      openRoom();
    } else {
      const err = $("login-error");
      err.textContent = "Jeton invalide.";
      err.hidden = false;
    }
  });

  window.addEventListener("beforeunload", (e) => {
    if (hasPending()) e.preventDefault();
  });

  async function main() {
    if (!window.isSecureContext || !crypto.subtle) {
      fatal("HTTPS requis : WebCrypto n'est pas disponible sur une page non sécurisée.");
      return;
    }
    const res = await fetch("/api/me");
    if (res.status === 401) {
      $("login").hidden = false;
      $("token").focus();
      return;
    }
    if (!res.ok) { fatal("Serveur indisponible."); return; }
    openRoom();
  }

  main();
})();
