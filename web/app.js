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

  function sendFile(f) {
    return new Promise((resolve) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", `/api/rooms/${roomId}/files?name=${encodeURIComponent(f.name)}`);
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) setStatus(`envoi de ${f.name} : ${Math.round(e.loaded / e.total * 100)} %`, "warn");
      };
      xhr.onload = () => {
        if (xhr.status === 200) setStatus(`${f.name} partagé`, "ok");
        else setStatus(`${f.name} : erreur ${xhr.status} : ${xhr.responseText.trim()}`, "err");
        resolve(xhr.status === 200);
      };
      xhr.onerror = () => { setStatus(`${f.name} : envoi échoué`, "err"); resolve(false); };
      xhr.send(f);
    });
  }

  // Un seul fichier par salle : le nouveau remplace l'actuel pour tout le monde.
  let uploading = false;
  async function upload(files) {
    const f = files[0];
    if (!f || uploading) return;
    if (maxFile && f.size > maxFile) {
      setStatus(`${f.name} : trop gros (max ${fmtSize(maxFile)})`, "err");
      return;
    }
    uploading = true;
    $("btn-upload").classList.add("busy");
    try {
      await sendFile(f);
      if (files.length > 1) setStatus(`${f.name} partagé (un seul fichier par salle)`, "warn");
    } finally {
      uploading = false;
      $("btn-upload").classList.remove("busy");
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
