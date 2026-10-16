# zedcode

Partage de code en temps réel entre tes machines, auto-hébergé, **chiffré de bout en bout**.
Go, bibliothèque standard uniquement (zéro dépendance), image Docker `scratch` d'environ 10 Mo.

## Fonctionnement

```
PC A ──(HTTPS)──► code.zedxp.com ◄──(HTTPS)── PC B
 │  chiffre AES-256-GCM        relaie/stocke         │ déchiffre
 │  clé = fragment #…          du chiffré opaque     │ clé = fragment #…
```

- Une salle = une URL `https://code.zedxp.com/r/<id>#<clé>`.
- La clé (256 bits) est dans le **fragment** (`#…`) : le navigateur ne l'envoie jamais au serveur.
  Le serveur ne stocke que du texte chiffré.
- Synchro : PUT HTTP (envoi) + Server-Sent Events (réception), pas de WebSocket.
- Accès protégé par un jeton partagé (`ACCESS_TOKEN`) → cookie `HttpOnly; Secure; SameSite=Strict`.
- Persistance : un fichier JSON chiffré par salle dans `/data`, supprimé après `ROOM_TTL` d'inactivité.

## Déploiement sur Coolify

1. **DNS** : enregistrement `A` `code.zedxp.com` → IP du VPS (et `AAAA` si IPv6).
2. **Coolify** → New Resource → dépôt Git (pousse ce dossier sur GitHub/Gitea) → Build Pack **Docker Compose**.
3. Dans le champ **Domains** du service `zedcode` : `https://code.zedxp.com:8080`
   (le `:8080` indique au proxy le port interne ; l'accès public reste en 443).
4. **Variables d'environnement** : `ACCESS_TOKEN` = sortie de `openssl rand -hex 32`.
5. Deploy. Coolify génère le certificat Let's Encrypt.

Ouvre `https://code.zedxp.com`, entre le jeton, une salle est créée. « Copier le lien » → ouvre-le sur l'autre PC.

## Configuration

| Variable | Défaut | Rôle |
|---|---|---|
| `ACCESS_TOKEN` | — (obligatoire, ≥ 16 car.) | Jeton d'accès |
| `ROOM_TTL` | `72h` | Durée de vie d'une salle inactive |
| `MAX_DOC_BYTES` | `1048576` | Taille max d'un document chiffré (base64) |
| `MAX_TOTAL_BYTES` | `33554432` | Quota total en mémoire (borne la RAM) |
| `MAX_ROOMS` | `500` | Nombre max de salles |
| `MAX_CONNECTIONS` | `200` | Flux SSE simultanés max |
| `DATA_DIR` | `/data` (image) | Vide = pas de persistance |
| `COOKIE_INSECURE` | — | `1` pour tester en HTTP local uniquement |
| `INSECURE_NO_AUTH` | — | `1` + jeton vide = pas d'auth (déconseillé) |

## Dev local

```sh
docker build -t zedcode .
docker run --rm -p 8080:8080 -e ACCESS_TOKEN=$(openssl rand -hex 32) -e COOKIE_INSECURE=1 zedcode
# http://localhost:8080 (localhost compte comme contexte sécurisé pour WebCrypto)
```

## Modèle de sécurité — ce que ça protège, et ce que ça ne protège pas

Protège contre :
- lecture du code par quelqu'un qui accède au disque, aux backups ou aux logs du VPS ;
- interception réseau (TLS) ;
- accès d'inconnus (jeton) et CSRF (cookie `SameSite=Strict` + `Content-Type: application/json` imposé).

Ne protège **pas** contre :
- **un serveur compromis** : il pourrait servir un `app.js` modifié qui exfiltre la clé.
  C'est la limite de tout chiffrement E2E livré par une page web ;
- quiconque possède le lien complet **et** le jeton ;
- un PC compromis. Le lien complet reste dans l'historique du navigateur.

## Limites connues (v0)

- **Dernier écrit gagne** : si deux machines tapent en même temps, l'une écrase l'autre.
  Suffisant pour passer du code d'un PC à l'autre ; insuffisant pour de la vraie co-édition.
  Évolution possible : CRDT (Yjs) avec mises à jour chiffrées relayées par le serveur.
- Document entier renvoyé à chaque modification (debounce 300 ms).
- Pas de coloration syntaxique (`<textarea>`). Évolution : CodeMirror 6 embarqué.
- Pas de rate-limit par IP sur `/api/login` (délai fixe de 1 s par échec). Avec un jeton de 64 hex
  le brute-force est irréaliste ; ajoute un rate-limit Traefik si tu veux en plus.
