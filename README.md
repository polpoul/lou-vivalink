# lou.vivalink.top — Galerie photo

Application de galerie photo self-hosted avec esthétique Polaroid.

## Architecture

```
lou.vivalink.top/           → Frontend galerie (index.html)
lou.vivalink.top/manage.html → Frontend gestion (manage.html)
lou.vivalink.top/api/*      → Backend Go (photo-api)
lou.vivalink.top/photos/*   → Serving des images (via photo-api)
```

## Stack

- **Backend** : Go (stdlib + golang.org/x/image pour resize)
- **Frontend** : HTML/CSS/JS vanilla, Playfair Display + DM Sans
- **Auth** : shared auth-service (magic link OTP), device_token en localStorage
- **Stockage** : `/opt/photos/` sur le VPS, monté en volume Docker
- **Resize** : auto à max 1920px, conversion JPEG qualité 88

## Setup VPS

### 1. Créer le dossier de stockage

```bash
mkdir -p /opt/photos
chmod 755 /opt/photos
```

### 2. Frontend files

```bash
mkdir -p /opt/apps-vivalink/lou
# Copier index.html et manage.html dans ce dossier
```

Ajouter dans la config nginx de `apps-vivalink` :

```nginx
location /lou/ {
    alias /usr/share/nginx/html/lou/;
    index index.html;
    try_files $uri $uri/ /lou/index.html;
}
```

Ou, si lou.vivalink.top est un vhost séparé dans apps-vivalink, ajouter un server block dédié.

### 3. docker-compose.yml

Ajouter le service `photo-api` depuis `docker-compose.snippet.yml`.

Points clés Traefik :
- Router `photo-api` : `Host(lou.vivalink.top) && PathPrefix(/api, /photos)`
- Router `lou-front` : `Host(lou.vivalink.top)` → apps-vivalink
- Les deux utilisent `letsencrypt` comme certresolver

### 4. DNS

Ajouter un enregistrement A :
```
lou.vivalink.top → 87.106.43.140
```

### 5. GitHub Actions secrets

Déjà configurés sur tes autres repos : `VPS_HOST`, `VPS_USER`, `VPS_SSH_KEY`

### 6. Premier déploiement

```bash
# Sur le VPS, forcer le pull initial
docker pull ghcr.io/polpoul/photo-api:latest
docker compose -f /opt/vivalink/docker-compose.yml up -d photo-api
```

## API endpoints

| Méthode | Route | Auth | Description |
|---------|-------|------|-------------|
| GET | `/api/gallery/list` | ✓ | Liste photos (galerie) |
| GET | `/api/photos/list` | ✓ | Liste photos (gestion) |
| POST | `/api/photos/upload` | ✓ | Upload (multipart `photos[]`) |
| DELETE | `/api/photos/{filename}` | ✓ | Suppression |
| GET | `/photos/{filename}` | ✓ | Serving image |
| GET | `/health` | ✗ | Health check |

## Resize

Toutes les images uploadées sont :
- Décodées (JPG, PNG, GIF, WebP supportés)
- Redimensionnées si > 1920px (ratio conservé, algo CatmullRom)
- Sauvegardées en JPEG qualité 88
- Nommées `{timestamp}_{nom-sanitizé}.jpg`

## Droits différenciés (futur)

La galerie et la gestion ont des tokens séparés côté auth-service.
Pour distinguer les droits, il suffira d'ajouter un champ `role` dans
la réponse de `/verify` de auth-service et de vérifier ce champ dans
les handlers `handleUpload` et `handleDelete`.
