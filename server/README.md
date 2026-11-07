# melodia-sync

Server self-hosted per Melodia: login dall'app e sincronizzazione delle playlist tra telefoni.
Un solo binario Go con database SQLite. Circa 10 MB di RAM a riposo, limite morbido a 32 MB (`GOMEMLIMIT`).

Il contratto API con l'app è in [`docs/sync-api.md`](../docs/sync-api.md).

## Installazione con Docker Compose

L'immagine viene pubblicata dalla CI a ogni merge su `main` che tocca `server/`:
`ghcr.io/sangregoriopaolo/melodia-sync:latest`.

Di default i pacchetti GHCR sono privati: rendilo pubblico da GitHub
(Profilo → Packages → melodia-sync → Package settings → Change visibility),
oppure fai `docker login ghcr.io` sul server con un token che ha `read:packages`.

```yaml
services:
  melodia-sync:
    image: ghcr.io/sangregoriopaolo/melodia-sync:latest
    restart: unless-stopped
    environment:
      MELODIA_TRUST_PROXY: "1"   # dietro reverse proxy
    volumes:
      - ./melodia-data:/data
    ports:
      - "127.0.0.1:8080:8080"    # esposto solo al reverse proxy
    mem_limit: 64m
```

La cartella `melodia-data` deve essere scrivibile dall'utente 65532 del container:
`mkdir melodia-data && sudo chown 65532:65532 melodia-data`.

### HTTPS

Il server parla solo HTTP: mettilo dietro il tuo reverse proxy con HTTPS.
Esempio Caddy:

```
melodia.tuodominio.it {
    reverse_proxy 127.0.0.1:8080
}
```

Con `MELODIA_TRUST_PROXY=1` il limite sui tentativi di login usa l'IP reale del telefono
(`X-Real-IP` / `X-Forwarded-For`). Non attivarlo se il server è esposto senza proxy.

## Utenti

Non c'è registrazione pubblica: gli utenti si creano da riga di comando.

```sh
docker compose exec melodia-sync /melodia-sync user add paolo     # chiede la password
docker compose exec melodia-sync /melodia-sync user list
docker compose exec melodia-sync /melodia-sync user passwd paolo  # disconnette tutti i telefoni
docker compose exec melodia-sync /melodia-sync user del paolo     # cancella anche le sue playlist
```

Password di almeno 8 caratteri, salvate con bcrypt.

## Backup

Tutto è in `melodia-data/melodia.db` (più i file `-wal` e `-shm` mentre il server gira).
Per una copia coerente a caldo: `sqlite3 melodia-data/melodia.db ".backup melodia-backup.db"`,
oppure ferma il container e copia la cartella.

## Sviluppo

```sh
cd server
go test ./...
MELODIA_DB=dev.db go run ./cmd/melodia-sync user add test
MELODIA_DB=dev.db go run ./cmd/melodia-sync serve
```

Variabili d'ambiente: `MELODIA_DB` (default `./melodia.db`, nel container `/data/melodia.db`),
`MELODIA_ADDR` (default `:8080`), `MELODIA_TRUST_PROXY`.
