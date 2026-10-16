# zedcode

Partage de code en temps réel entre tes machines, façon codeshare, auto-hébergé.
Go, bibliothèque standard uniquement (zéro dépendance), image Docker `scratch` d'environ 10 Mo.

## Utilisation

- `https://code.zedxp.com` → crée une salle et redirige vers `https://code.zedxp.com/aB3x9Z`.
- Sur l'autre PC : tape `code.zedxp.com/aB3x9Z`, ou saisis le code dans « Rejoindre ».
- Boutons :
  - **Rejoindre** : ouvre la salle dont tu as tapé le code ;
  - **Nouvelle salle** : crée une salle vide ;
  - **Copier le lien** : copie l'URL de la salle ;
  - **Télécharger** : enregistre le contenu en `.txt`.

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
- Pas de coloration syntaxique (`<textarea>`). Évolution : CodeMirror 6 embarqué.
