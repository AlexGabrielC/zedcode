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
    del: (k) => { try { localStorage.removeItem(k); } catch {} },
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
    es.addEventListener("files", (e) => renderFiles(JSON.parse(e.data)));
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

  // ---------- fichiers ----------
  const maxFile = Number(document.querySelector('meta[name="max-file"]').content) || 0;
  const icon = (d) => `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="${d}"/></svg>`;

  function fmtSize(n) {
    if (n < 1024) return `${n} o`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} Ko`;
    return `${(n / 1048576).toFixed(1)} Mo`;
  }

  function renderFiles(list) {
    const ul = $("files");
    ul.replaceChildren(...list.map((f) => {
      const li = document.createElement("li");
      li.className = "file";
      const a = document.createElement("a");
      a.href = `/api/rooms/${roomId}/files/${f.id}`;
      a.download = f.name;
      a.title = `Télécharger ${f.name}`;
      a.innerHTML = icon("M12 4v11M7 10l5 5 5-5M5 20h14") + '<span class="name"></span><span class="size"></span>';
      a.querySelector(".name").textContent = f.name;
      a.querySelector(".size").textContent = fmtSize(f.size);
      const del = document.createElement("button");
      del.type = "button";
      del.title = `Retirer ${f.name} de la salle`;
      del.setAttribute("aria-label", del.title);
      del.innerHTML = icon("M6 6l12 12M18 6L6 18");
      del.addEventListener("click", () => removeFile(f, li));
      li.append(a, del);
      return li;
    }));
    ul.hidden = list.length === 0;
  }

  async function removeFile(f, li) {
    li.classList.add("pending");
    try {
      const res = await fetch(`/api/rooms/${roomId}/files/${f.id}`, { method: "DELETE" });
      if (!res.ok && res.status !== 404) throw new Error((await res.text()).trim());
    } catch (e) {
      li.classList.remove("pending");
      setStatus(`suppression impossible : ${e.message}`, "err");
    }
    // En cas de succès, la liste arrive par l'événement "files".
  }

  // ---------- envoi par morceaux, avec reprise ----------
  // Chaque morceau est une requête courte : le proxy coupe toute requête de
  // plus de 60 s. Taille ajustée au débit mesuré pour viser ~15 s par morceau.
  const CHUNK_MIN = 256 << 10, CHUNK_MAX = 8 << 20, CHUNK_SECONDS = 15;
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  // Requête XHR (fetch n'expose pas la progression d'envoi). Ne rejette jamais :
  // status 0 = réseau coupé.
  function xhr(method, url, body, type, onProgress) {
    return new Promise((resolve) => {
      const x = new XMLHttpRequest();
      x.open(method, url);
      if (type) x.setRequestHeader("Content-Type", type);
      if (onProgress) x.upload.onprogress = (e) => onProgress(e.loaded);
      x.onload = () => {
        let json = null;
        try { json = JSON.parse(x.responseText); } catch {}
        resolve({ status: x.status, json, text: x.responseText.trim() });
      };
      x.onerror = x.onabort = x.ontimeout = () => resolve({ status: 0, json: null, text: "" });
      x.send(body);
    });
  }

  // Un envoi interrompu est retrouvé par nom + taille : remettre le même
  // fichier reprend là où le serveur en était.
  async function sendFile(f) {
    const key = `zc-up:${roomId}:${f.name}:${f.size}`;
    const pct = (n) => (f.size ? Math.floor(n / f.size * 100) : 100);
    let id = store.get(key), off = 0;
    if (id) {
      const r = await xhr("GET", `/api/rooms/${roomId}/uploads/${id}`);
      if (r.status === 200 && r.json && r.json.size === f.size) off = r.json.off;
      else id = null;
    }
    if (id && off) setStatus(`reprise de ${f.name} à ${pct(off)} %`, "warn");
    if (!id) {
      const r = await xhr("POST", `/api/rooms/${roomId}/uploads`,
        JSON.stringify({ name: f.name, size: f.size }), "application/json");
      if (r.status !== 200 || !r.json) {
        setStatus(`${f.name} : erreur ${r.status || "réseau"} ${r.text}`, "err");
        return false;
      }
      if (r.json.file) { setStatus(`${f.name} partagé`, "ok"); return true; }
      id = r.json.id;
      store.set(key, id);
    }

    let chunk = 1 << 20, fails = 0;
    while (off < f.size) {
      const end = Math.min(f.size, off + chunk);
      const t0 = performance.now();
      const r = await xhr("PUT", `/api/rooms/${roomId}/uploads/${id}?off=${off}`,
        f.slice(off, end), "application/octet-stream",
        (loaded) => setStatus(`envoi de ${f.name} : ${pct(off + loaded)} %`, "warn"));
      if (r.status === 200 && r.json) {
        const secs = (performance.now() - t0) / 1000;
        chunk = Math.max(CHUNK_MIN, Math.min(CHUNK_MAX, Math.round((end - off) / secs * CHUNK_SECONDS)));
        off = r.json.off;
        fails = 0;
        if (r.json.file) break;
        continue;
      }
      if (r.status === 409 && r.json) { off = r.json.off; continue; } // resynchronisation
      if (r.status === 404) {
        store.del(key);
        // Dernier morceau reçu mais réponse perdue : l'envoi est fini, le
        // fichier est déjà dans la liste.
        if (end === f.size) { setStatus(`${f.name} partagé`, "ok"); return true; }
        setStatus(`${f.name} : envoi expiré (30 min sans activité), recommence`, "err");
        return false;
      }
      if (r.status === 413 || r.status === 415 || r.status === 507) {
        setStatus(`${f.name} : erreur ${r.status} ${r.text}`, "err");
        return false;
      }
      // Coupure (réseau, veille, changement d'appli, proxy) : on réessaie avec
      // des morceaux plus petits. Onglet caché : on ne compte pas l'échec.
      if (!document.hidden) fails++;
      if (fails > 15) {
        setStatus(`${f.name} : coupé à ${pct(off)} %, remets le même fichier pour reprendre`, "err");
        return false;
      }
      chunk = Math.max(CHUNK_MIN, chunk >> 1);
      setStatus(`connexion perdue à ${pct(off)} %, reprise…`, "warn");
      await sleep(Math.min(30000, 1000 * 2 ** Math.min(fails, 5)));
    }
    store.del(key);
    setStatus(`${f.name} partagé`, "ok");
    return true;
  }

  // ---------- écran allumé pendant l'envoi ----------
  // Le navigateur relâche le verrou dès que la page est cachée : on le
  // redemande quand elle redevient visible.
  let wakeLock = null;
  async function keepAwake(on) {
    try {
      if (on && !wakeLock && "wakeLock" in navigator && document.visibilityState === "visible") {
        wakeLock = await navigator.wakeLock.request("screen");
        wakeLock.addEventListener("release", () => { wakeLock = null; });
      } else if (!on && wakeLock) {
        await wakeLock.release();
        wakeLock = null;
      }
    } catch {} // non supporté ou refusé (batterie faible…) : sans conséquence
  }
  document.addEventListener("visibilitychange", () => { if (uploading) keepAwake(true); });

  // Envois un par un. Le serveur garde 3 fichiers par salle et supprime les
  // plus anciens (de la salle, puis du serveur) quand la place manque.
  let uploading = false;
  async function upload(files) {
    files = Array.from(files);
    if (!files.length || uploading) return;
    uploading = true;
    $("btn-upload").classList.add("busy");
    keepAwake(true);
    try {
      for (const f of files) {
        if (maxFile && f.size > maxFile) {
          setStatus(`${f.name} : trop gros (max ${fmtSize(maxFile)})`, "err");
          continue;
        }
        if (!(await sendFile(f))) break;
      }
    } finally {
      uploading = false;
      $("btn-upload").classList.remove("busy");
      keepAwake(false);
    }
  }

  $("btn-upload").addEventListener("click", () => $("file-input").click());
  $("file-input").addEventListener("change", (e) => {
    upload(e.target.files);
    e.target.value = "";
  });

  // Glisser-déposer sur toute la page. Écouteurs en capture sur window : sans
  // ça, CodeMirror insérerait le contenu du fichier comme texte.
  const hasFiles = (e) => e.dataTransfer && Array.from(e.dataTransfer.types).includes("Files");
  let dragDepth = 0;
  window.addEventListener("dragenter", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    dragDepth++;
    $("drop").hidden = false;
  }, true);
  window.addEventListener("dragleave", (e) => {
    if (!hasFiles(e)) return;
    if (--dragDepth <= 0) { dragDepth = 0; $("drop").hidden = true; }
  }, true);
  window.addEventListener("dragover", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.stopPropagation();
    e.dataTransfer.dropEffect = "copy";
  }, true);
  window.addEventListener("drop", (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.stopPropagation();
    dragDepth = 0;
    $("drop").hidden = true;
    upload(e.dataTransfer.files);
  }, true);

  // Coller une capture d'écran ou un fichier copié : partagé comme fichier.
  // Si le presse-papiers contient aussi du texte (copie depuis Word, Excel…),
  // le texte l'emporte et va dans l'éditeur.
  window.addEventListener("paste", (e) => {
    const cd = e.clipboardData;
    if (!cd || !cd.files.length || cd.types.includes("text/plain")) return;
    const files = cd.files;
    e.preventDefault();
    e.stopPropagation();
    upload(files);
  }, true);

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
    if (hasPending() || uploading) e.preventDefault();
  });

  editor.focus();
  connect();
})();
