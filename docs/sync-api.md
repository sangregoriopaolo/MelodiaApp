# Melodia Sync API (v1)

Contract between the Melodia Android app and the `melodia-sync` server in `server/`.
All bodies are JSON. All endpoints except `login` and `healthz` need
`Authorization: Bearer <token>`. Errors look like `{"error": "message"}`.

## Auth

| Method | Path | Body | Response |
| --- | --- | --- | --- |
| POST | `/api/v1/login` | `{username, password, deviceName}` | `200 {token, username}` · `401` bad credentials · `429` too many failures (10 per 15 min per IP and per username) |
| POST | `/api/v1/logout` | none | `204`, the token stops working |
| GET | `/api/v1/me` | none | `200 {username, deviceName}` · `401` token invalid (log the user out locally) |
| GET | `/healthz` | none | `200 ok` |

Tokens do not expire. They are revoked by logout, by `melodia-sync user passwd` (all devices) or by deleting the user.

## Playlist

```json
{
  "id": "LPabcdefgh",
  "name": "Road trip",
  "rev": 42,
  "updatedAt": 1762515000000,
  "deleted": false,
  "items": [
    {"songId": "dQw4w9WgXcQ", "setVideoId": "", "title": "…", "artists": "…", "album": "…",
     "duration": 213, "thumbnailUrl": "https://…"}
  ]
}
```

- `id` is the app's local playlist id (`[A-Za-z0-9_-]`, max 64). Ids are scoped per user.
- `rev` is assigned by the server. Each user has one counter that grows on every write.
- `updatedAt` is unix milliseconds from the phone that made the change; used to pick the winner in a conflict.
- `items` is the full ordered list. `songId` is required; the rest is display metadata.
- Only playlists created in Melodia are synced (not YouTube playlists, not local-file songs).

## Sync

| Method | Path | Body | Response |
| --- | --- | --- | --- |
| GET | `/api/v1/sync?since=<rev>` | none | `200 {rev, playlists: [Playlist…]}`: everything changed after `since`, tombstones included (`deleted: true`, `items: []`) |
| PUT | `/api/v1/playlists/{id}` | `{baseRev, name, updatedAt, items}` | `200 {rev}` · `409 {error: "conflict", current: Playlist}` |
| DELETE | `/api/v1/playlists/{id}?baseRev=<rev>&updatedAt=<ms>` | none | `200 {rev}` · `409 {error: "conflict", current: Playlist}` |

`baseRev` is the `rev` the phone last saw for that playlist (0 if never synced).
The server accepts the write when it matches its own `rev`, or when it has never seen the playlist.

## Client algorithm

1. Keep `lastRev` (from the last `/sync` response) and, per playlist, `syncRev` and a `dirty` flag. Keep tombstones for playlists deleted while offline.
2. **Pull** `GET /sync?since=lastRev`. For each playlist: if it is not dirty locally, apply it (or delete it if `deleted`). If it is dirty, resolve as a conflict (step 4). Save `lastRev`.
3. **Push** each dirty playlist with `PUT` (or `DELETE` for tombstones) and `baseRev = syncRev`. On `200` store the new `rev` as `syncRev` and clear `dirty`.
4. **Conflict** (`409`, or a dirty playlist that came back in a pull): the version with the newer `updatedAt` wins. If the losing version has a different song list, keep it as a new local playlist named `"<name> (conflitto)"`, which is pushed as a new playlist. Then retry the push with `baseRev = current.rev` if the local one won, or apply `current` if it lost.
5. First login on a phone with existing playlists: pull with `since=0`, then push local playlists the server does not have (`baseRev = 0`). Playlists with the same name are not merged.
