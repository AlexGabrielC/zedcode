# zedcode

Partage de code en temps réel entre tes machines, façon codeshare, auto-hébergé.
Go, bibliothèque standard uniquement (zéro dépendance), image Docker `scratch` d'environ 10 Mo.

## Utilisation

- `https://code.zedxp.com` → crée une salle et redirige vers `https://code.zedxp.com/aB3x9Z`.
- Sur l'autre PC : tape `code.zedxp.com/aB3x9Z`, ou saisis le code dans « Rejoindre ».
- Barre d'outils :
  - **code de salle** (en haut à gauche) : un clic copie le lien ;
  - **langage** : coloration syntaxique (SQL, Java, Bash, Python, Go…), partagé par tous les appareils de la salle ;
  - **Code → flèche** : rejoindre une autre salle ;
  - **Télécharger** : enregistre le contenu avec la bonne extension (`.sql`, `.java`…) ;
  - **Nouvelle salle** : crée une salle vide ;
  - **soleil / lune** : thème clair ou sombre (mémorisé par navigateur).
- Éditeur CodeMirror 6 : numéros de ligne, `Ctrl+D` sélection de l'occurrence suivante,
  `Alt+clic` multi-curseur, `Ctrl+F` recherche, `Tab` indente.

Code de salle : 6 caractères `[A-Za-z0-9]`, soit 62⁶ = 56 800 235 584 combinaisons, tirés avec `crypto/rand`.

## Fonctionnement

- Synchro : PUT HTTP (envoi) + Server-Sent Events (réception), pas de WebSocket.
- **Tout est en RAM** : rien sur disque, donc rien dans les sauvegardes Coolify/OVH.
  Un redéploiement vide les salles ; un onglet encore ouvert renvoie alors son contenu.
- Une salle est effacée `ROOM_TTL` (2 h) après la dernière activité (modification ou départ du dernier appareil), si plus personne n'est connecté.
- Ouvrir une salle est limité à `JOIN_PER_MINUTE` (30) par IP : balayer les codes au hasard
  demanderait des milliards de requêtes. Un PUT sur une salle non ouverte est refusé (404).

## Déploiement sur Coolify

1. **DNS** : enregistrement `A` `code.zedxp.com` → IP du VPS.
2. **Coolify** → New Resource → Public Repository `https://github.com/AlexGabrielC/zedcode`
   → Build Pack **Docker Compose**, Base directory `/`, Compose file `/docker-compose.yml`.
3. Champ **Domains** du service `zedcode` : `https://code.zedxp.com:8080`.
4. Deploy. Aucune variable obligatoire.

## Configuration

| Variable | Défaut | Rôle |
|---|---|---|
| `ROOM_TTL` | `2h` | Durée de vie d'une salle inactive |
| `JOIN_PER_MINUTE` | `30` | Ouvertures de salle max par IP et par minute |
| `MAX_DOC_BYTES` | `1048576` | Taille max d'un document |
| `MAX_TOTAL_BYTES` | `33554432` | Quota total en mémoire (borne la RAM) |
| `MAX_ROOMS` | `500` | Nombre max de salles |
| `MAX_CONNECTIONS` | `200` | Appareils connectés simultanément, toutes salles confondues |
| `TRUST_PROXY` | `1` (compose) | Lit l'IP client dans `X-Forwarded-For` (uniquement derrière un proxy) |

## Éditeur (web/editor.js)

`web/editor.js` est un fichier **généré** (CodeMirror 6, bundle esbuild) et versionné dans le dépôt :
le build Docker ne compile que du Go, pas de Node sur le VPS. Source : `editor/src/main.js`.

Après modification de `editor/` :

```sh
docker run --rm -v "$PWD:/w" -w /w/editor node:22-alpine sh -c "npm ci && npm run build"
```

Poids : 1,1 Mo brut, environ 386 Ko en gzip (pré-compressé au build Docker), mis en cache un an
par le navigateur via une URL versionnée (`?v=<empreinte>`).

## Dev local

```sh
docker build -t zedcode .
docker run --rm -p 8080:8080 zedcode   # http://localhost:8080
```

## Sécurité : ce qu'il faut savoir

- Transport chiffré par HTTPS (Let's Encrypt via Coolify).
- **Pas de chiffrement de bout en bout** : le serveur voit le texte en clair (en RAM uniquement).
- **Quiconque connaît le code de salle peut lire et modifier.** Ne colle pas de secrets (clés d'API, mots de passe).
- Pas de compte : n'importe qui peut créer des salles. Les quotas bornent la RAM utilisée.
- Si tu passes Cloudflare (proxy orange) devant le VPS, la limite par IP verra les IP de Cloudflare :
  il faudrait alors lire `CF-Connecting-IP`.

## Limites connues

- **Dernier écrit gagne** : si deux machines tapent en même temps, l'une écrase l'autre.
- Document entier renvoyé à chaque modification (debounce 300 ms).
- Pas de détection automatique du langage : choix manuel dans la liste.
