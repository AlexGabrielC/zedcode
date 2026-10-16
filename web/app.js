"use strict";
(() => {
  const $ = (id) => document.getElementById(id);

  const m = location.pathname.match(/^\/([A-Za-z0-9]{6})$/);
  if (!m) { location.replace("/"); return; }
  const roomId = m[1];
  document.title = `${roomId} · zedcode`;
  $("room-code").textContent = roomId;

  const store = {
    get: (k) => { try { return localStorage.getItem(k); } catch { return null; } },
    set: (k, v) => { try { localStorage.setItem(k, v); } catch {} },
  };

  // ---------- langage ----------
  const langs = window.ZedEditor.languages;
  const langById = Object.fromEntries(langs.map((l) => [l.id, l]));
  const select = $("lang");
  for (const l of langs) select.add(new Option(l.label, l.id));
  let lang = langById[store.get("zc-lang")] ? store.get("zc-lang") : "text";
  select.value = lang;

  const isDark = () => document.documentElement.dataset.theme === "dark";
  const editor = window.ZedEditor.create($("editor"), { onChange: schedule, lang, dark: isDark() });

  function setLang(id, local) {
    if (!langById[id] || id === lang) return;
    lang = id;
    select.value = id;
    editor.setLanguage(id);
    if (local) { store.set("zc-lang", id); schedule(); }
  }
  select.addEventListener("change", () => setLang(select.value, true));

  // ---------- thème ----------
  $("btn-theme").addEventListener("click", () => {
    const t = isDark() ? "light" : "dark";
    document.documentElement.dataset.theme = t;
    store.set("zc-theme", t);
    editor.setDark(t === "dark");
  });

  // ---------- état ----------
  function setStatus(text, kind = "") {
    $("status").textContent = text;
    $("dot").className = "dot " + kind;
  }

  const clientId = Array.from(crypto.getRandomValues(new Uint8Array(8)),
    (b) => b.toString(16).padStart(2, "0")).join("");
  let version = -1;
  let connected = false;
  let dirty = false;
  let sending = false;
  let sendTimer = null;

  const hasPending = () => dirty || sending || sendTimer !== null;

  function onDoc(ev) {
    if (ev.v === version) return;
    version = ev.v;
    // Serveur redémarré (salle vide) alors qu'on a encore le contenu : on le renvoie.
    if (ev.v === 0 && editor.getText()) { schedule(); return; }
    // Nos propres échos, ou une frappe locale en cours (dernier écrit gagne).
    if (ev.f === clientId || hasPending()) return;
    if (ev.l) setLang(ev.l, false);
    editor.setText(ev.t);
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
      $("peers").textContent = n > 1 ? `${n} appareils` : "seul ici";
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
          body: JSON.stringify({ t: editor.getText(), l: lang, f: clientId }),
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

  // ---------- actions ----------
  $("join").addEventListener("submit", (e) => {
    e.preventDefault();
    const code = $("join-code").value.trim();
    if (/^[A-Za-z0-9]{6}$/.test(code)) location.href = "/" + code;
  });

  $("room").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(location.origin + "/" + roomId);
      setStatus("lien copié", "ok");
    } catch {
      setStatus("copie refusée par le navigateur", "err");
    }
  });

  $("btn-download").addEventListener("click", () => {
    const ext = langById[lang].ext;
    const url = URL.createObjectURL(new Blob([editor.getText()], { type: "text/plain" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = ext === "Dockerfile" ? "Dockerfile" : `zedcode-${roomId}.${ext}`;
    a.click();
    URL.revokeObjectURL(url);
  });

  window.addEventListener("beforeunload", (e) => {
    if (hasPending()) e.preventDefault();
  });

  editor.focus();
  connect();
})();
