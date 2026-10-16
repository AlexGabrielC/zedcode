"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const editor = $("editor");

  const m = location.pathname.match(/^\/([A-Za-z0-9]{6})$/);
  if (!m) { location.replace("/"); return; }
  const roomId = m[1];
  document.title = `${roomId} · zedcode`;
  $("room").textContent = roomId;

  const clientId = Array.from(crypto.getRandomValues(new Uint8Array(8)),
    (b) => b.toString(16).padStart(2, "0")).join("");
  let version = -1;
  let connected = false;
  let dirty = false;
  let sending = false;
  let sendTimer = null;

  function setStatus(text, kind = "") {
    const el = $("status");
    el.textContent = text;
    el.className = "status " + kind;
  }

  const hasPending = () => dirty || sending || sendTimer !== null;

  function applyRemote(text) {
    if (text === editor.value) return;
    const { selectionStart: s, selectionEnd: e, scrollTop } = editor;
    editor.value = text;
    editor.setSelectionRange(Math.min(s, text.length), Math.min(e, text.length));
    editor.scrollTop = scrollTop;
  }

  function onDoc(ev) {
    if (ev.v === version) return;
    version = ev.v;
    // Serveur redémarré (salle vide) alors qu'on a encore le contenu : on le renvoie.
    if (ev.v === 0 && editor.value) { schedule(); return; }
    // Nos propres échos, ou une frappe locale en cours (dernier écrit gagne).
    if (ev.f === clientId || hasPending()) return;
    applyRemote(ev.t);
    setStatus("synchronisé", "ok");
  }

  function connect() {
    const es = new EventSource(`/api/rooms/${roomId}/events`);
    es.onopen = () => { connected = true; setStatus("connecté", "ok"); };
    es.onerror = () => {
      connected = false;
      if (es.readyState === EventSource.CLOSED) {
        // Refus HTTP (429, 503…) : EventSource ne se reconnecte pas seul.
        setStatus("connexion refusée, nouvel essai…", "warn");
        setTimeout(connect, 5000);
      } else {
        setStatus("reconnexion…", "warn");
      }
    };
    es.addEventListener("doc", (e) => onDoc(JSON.parse(e.data)));
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
    if (!connected) { sendTimer = setTimeout(flush, 1000); return; }
    sending = true;
    try {
      while (dirty) {
        dirty = false;
        const res = await fetch(`/api/rooms/${roomId}`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ t: editor.value, f: clientId }),
        });
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

  editor.addEventListener("input", schedule);
  editor.addEventListener("keydown", (e) => {
    if (e.key === "Tab" && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
      e.preventDefault();
      editor.setRangeText("\t", editor.selectionStart, editor.selectionEnd, "end");
      schedule();
    }
  });

  $("join").addEventListener("submit", (e) => {
    e.preventDefault();
    const code = $("join-code").value.trim();
    if (/^[A-Za-z0-9]{6}$/.test(code)) location.href = "/" + code;
  });
  $("btn-copy").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(location.origin + "/" + roomId);
      setStatus("lien copié", "ok");
    } catch {
      setStatus("copie refusée par le navigateur", "err");
    }
  });
  $("btn-download").addEventListener("click", () => {
    const url = URL.createObjectURL(new Blob([editor.value], { type: "text/plain" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `zedcode-${roomId}.txt`;
    a.click();
    URL.revokeObjectURL(url);
  });

  window.addEventListener("beforeunload", (e) => {
    if (hasPending()) e.preventDefault();
  });

  editor.focus();
  connect();
})();
