/**
 * Melodia Sync: keeps the playlists created in Melodia in sync with the self-hosted
 * melodia-sync server (server/ in this repository), so the same account sees the same
 * playlists on every phone. Algorithm: docs/sync-api.md, "Client algorithm".
 */

package com.metrolist.music.melodiasync

import android.content.Context
import android.os.Build
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.longPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import com.metrolist.music.db.MelodiaSyncRow
import com.metrolist.music.db.MusicDatabase
import com.metrolist.music.db.entities.PlaylistEntity
import com.metrolist.music.db.entities.PlaylistSongMap
import com.metrolist.music.di.ApplicationScope
import com.metrolist.music.models.MediaMetadata
import com.metrolist.music.utils.dataStore
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import timber.log.Timber
import java.io.File
import java.security.MessageDigest
import java.time.LocalDateTime
import javax.inject.Inject
import javax.inject.Singleton

val MelodiaSyncServerKey = stringPreferencesKey("melodiaSyncServer")
val MelodiaSyncUsernameKey = stringPreferencesKey("melodiaSyncUsername")
val MelodiaSyncTokenKey = stringPreferencesKey("melodiaSyncToken")
val MelodiaSyncLastSyncKey = longPreferencesKey("melodiaSyncLastSync")

data class MelodiaSyncStatus(
    val running: Boolean = false,
    val lastSyncAt: Long = 0,
    val error: String? = null,
)

private data class LocalPlaylist(val id: String, val name: String, val songIds: List<String>) {
    val hash: String = contentHash(name, songIds)
}

private fun contentHash(name: String, songIds: List<String>): String {
    val digest = MessageDigest.getInstance("SHA-256")
    digest.update(name.trim().toByteArray())
    songIds.forEach {
        digest.update(0)
        digest.update(it.toByteArray())
    }
    return digest.digest().joinToString("") { "%02x".format(it) }
}

private fun RemotePlaylist.hash() = contentHash(name, items.map { it.songId })

private fun List<MelodiaSyncRow>.toLocalPlaylists(): Map<String, LocalPlaylist> =
    groupBy { it.playlistId }.mapValues { (id, rows) ->
        LocalPlaylist(id, rows.first().name, rows.mapNotNull { it.songId })
    }

@Singleton
class MelodiaSync @Inject constructor(
    @ApplicationContext private val context: Context,
    private val database: MusicDatabase,
    @ApplicationScope private val scope: CoroutineScope,
) {
    private val mutex = Mutex()
    private val stateFile = File(context.filesDir, "melodia_sync_state.json")
    private var state: MelodiaSyncState? = null
    private val lastObserved = mutableMapOf<String, String>()
    private val syncRequests = MutableSharedFlow<Unit>(extraBufferCapacity = 1)
    private var started = false

    private val _status = MutableStateFlow(MelodiaSyncStatus())
    val status: StateFlow<MelodiaSyncStatus> = _status.asStateFlow()

    private fun state(): MelodiaSyncState = state ?: MelodiaSyncState.load(stateFile).also { state = it }

    private suspend fun credentials(): Pair<String, String>? {
        val prefs = context.dataStore.data.first()
        val server = prefs[MelodiaSyncServerKey].orEmpty()
        val token = prefs[MelodiaSyncTokenKey].orEmpty()
        return if (server.isNotBlank() && token.isNotBlank()) server to token else null
    }

    /** Called once from App.onCreate. */
    @OptIn(FlowPreview::class)
    fun start() {
        if (started) return
        started = true
        scope.launch(Dispatchers.IO) {
            _status.update { it.copy(lastSyncAt = context.dataStore.data.first()[MelodiaSyncLastSyncKey] ?: 0) }

            // Note when each playlist changes locally, then sync a few seconds after the last change.
            launch {
                database.melodiaSyncRows()
                    .map { it.toLocalPlaylists() }
                    .distinctUntilChanged()
                    .collect { local ->
                        if (credentials() == null) return@collect
                        var dirty = false
                        mutex.withLock {
                            val st = state()
                            val now = System.currentTimeMillis()
                            local.values.forEach { lp ->
                                val changedSinceLastLook = lastObserved[lp.id] != null && lastObserved[lp.id] != lp.hash
                                val differsFromServer = st.playlists[lp.id]?.hash != lp.hash
                                if (differsFromServer && (changedSinceLastLook || lp.id !in st.localChangedAt)) {
                                    st.localChangedAt[lp.id] = now
                                }
                                if (differsFromServer) dirty = true
                            }
                            if (st.playlists.keys.any { it !in local }) dirty = true
                            lastObserved.clear()
                            local.values.forEach { lastObserved[it.id] = it.hash }
                            st.save(stateFile)
                        }
                        if (dirty) syncRequests.tryEmit(Unit)
                    }
            }
            launch {
                syncRequests.debounce(5_000).collect { syncNow() }
            }
            // Pull changes made on other phones: at startup and every 30 minutes.
            while (true) {
                syncNow()
                delay(30 * 60 * 1000L)
            }
        }
    }

    suspend fun login(server: String, username: String, password: String): Result<Unit> = withContext(Dispatchers.IO) {
        runCatching {
            val url = server.trim().trimEnd('/')
            require(url.startsWith("https://") || url.startsWith("http://")) { "L'indirizzo deve iniziare con https://" }
            val deviceName = "${Build.MANUFACTURER} ${Build.MODEL}".trim()
            val response = MelodiaSyncApi(url).login(username, password, deviceName)
            mutex.withLock {
                // A fresh login starts from scratch: pull everything, push what the server lacks.
                state = MelodiaSyncState().also { it.save(stateFile) }
                lastObserved.clear()
            }
            context.dataStore.edit {
                it[MelodiaSyncServerKey] = url
                it[MelodiaSyncUsernameKey] = response.username.ifBlank { username.trim() }
                it[MelodiaSyncTokenKey] = response.token
            }
            _status.update { it.copy(error = null) }
            scope.launch(Dispatchers.IO) { syncNow() }
            Unit
        }
    }

    suspend fun logout() = withContext(Dispatchers.IO) {
        credentials()?.let { (server, token) -> runCatching { MelodiaSyncApi(server, token).logout() } }
        clearSession()
    }

    private suspend fun clearSession() {
        context.dataStore.edit {
            it.remove(MelodiaSyncTokenKey)
            it.remove(MelodiaSyncLastSyncKey)
        }
        mutex.withLock {
            state = MelodiaSyncState()
            stateFile.delete()
            lastObserved.clear()
        }
        _status.value = MelodiaSyncStatus()
    }

    /** Runs one full sync if logged in. Safe to call from anywhere; concurrent calls queue up. */
    suspend fun syncNow(): Unit = withContext(Dispatchers.IO) {
        val (server, token) = credentials() ?: return@withContext
        _status.update { it.copy(running = true) }
        try {
            mutex.withLock {
                val api = MelodiaSyncApi(server, token)
                // A conflict can leave work for another pass; three passes always settle it.
                for (pass in 1..3) {
                    if (!syncPass(api, state())) break
                }
            }
            val now = System.currentTimeMillis()
            context.dataStore.edit { it[MelodiaSyncLastSyncKey] = now }
            _status.update { it.copy(running = false, lastSyncAt = now, error = null) }
        } catch (e: CancellationException) {
            _status.update { it.copy(running = false) }
            throw e
        } catch (e: MelodiaSyncException.Unauthorized) {
            clearSession()
            _status.update { it.copy(error = e.message) }
        } catch (e: Exception) {
            Timber.w(e, "Melodia Sync failed")
            _status.update { it.copy(running = false, error = e.message ?: e.javaClass.simpleName) }
        }
    }

    /** One pull + push. Returns true if a conflict means another pass is needed. */
    private suspend fun syncPass(api: MelodiaSyncApi, st: MelodiaSyncState): Boolean {
        var again = false

        // 1. Pull what other phones changed.
        val changes = api.changes(st.lastRev)
        var local = database.melodiaSyncRowsBlocking().toLocalPlaylists()
        changes.playlists.forEach { remote ->
            if (handleRemote(remote, local[remote.id], st)) again = true
        }
        st.lastRev = maxOf(st.lastRev, changes.rev)
        st.save(stateFile)

        // 2. Push local changes and new playlists.
        local = database.melodiaSyncRowsBlocking().toLocalPlaylists()
        for (lp in local.values) {
            val synced = st.playlists[lp.id]
            if (synced != null && synced.hash == lp.hash) continue
            val request = PutRequest(
                baseRev = synced?.rev ?: 0,
                name = lp.name.trim().ifBlank { "Playlist" },
                updatedAt = st.localChangedAt[lp.id] ?: System.currentTimeMillis(),
                items = remoteItems(lp.id),
            )
            when (val result = api.put(lp.id, request)) {
                is WriteResult.Ok -> {
                    st.playlists[lp.id] = SyncedPlaylist(result.rev, lp.hash)
                    st.localChangedAt.remove(lp.id)
                }
                is WriteResult.Conflict -> {
                    handleRemote(result.current, lp, st)
                    again = true
                }
            }
            st.save(stateFile)
        }

        // 3. Push deletions: synced before, gone from this phone now.
        for ((id, synced) in st.playlists.toMap()) {
            if (id in local) continue
            when (val result = api.delete(id, synced.rev, System.currentTimeMillis())) {
                is WriteResult.Ok -> st.playlists.remove(id)
                is WriteResult.Conflict -> {
                    handleRemote(result.current, null, st)
                    again = true
                }
            }
            st.localChangedAt.remove(id)
            st.save(stateFile)
        }
        return again
    }

    /**
     * Applies one playlist from the server. Returns true if this phone still has changes
     * to push for it afterwards.
     */
    private suspend fun handleRemote(remote: RemotePlaylist, lp: LocalPlaylist?, st: MelodiaSyncState): Boolean {
        val synced = st.playlists[remote.id]
        val localDirty = when {
            synced == null -> lp != null
            lp == null -> true // deleted on this phone
            else -> lp.hash != synced.hash
        }

        if (!localDirty) {
            if (remote.deleted) {
                deleteLocal(remote.id)
                st.playlists.remove(remote.id)
            } else if (lp == null || lp.hash != remote.hash()) {
                applyRemote(remote)
                st.playlists[remote.id] = SyncedPlaylist(remote.rev, currentHash(remote.id) ?: remote.hash())
            } else {
                st.playlists[remote.id] = SyncedPlaylist(remote.rev, lp.hash)
            }
            return false
        }

        // Both sides changed. Same content: nothing to resolve.
        if (lp != null && !remote.deleted && lp.hash == remote.hash()) {
            st.playlists[remote.id] = SyncedPlaylist(remote.rev, lp.hash)
            st.localChangedAt.remove(remote.id)
            return false
        }
        // Deleted here but edited elsewhere: keep the edits.
        if (lp == null) {
            if (remote.deleted) {
                st.playlists.remove(remote.id)
            } else {
                applyRemote(remote)
                st.playlists[remote.id] = SyncedPlaylist(remote.rev, currentHash(remote.id) ?: remote.hash())
            }
            st.localChangedAt.remove(remote.id)
            return false
        }
        // Edited here but deleted elsewhere: keep this phone's version, re-upload it.
        if (remote.deleted) {
            st.playlists[remote.id] = SyncedPlaylist(remote.rev, synced?.hash.orEmpty())
            return true
        }

        // Real conflict: the most recent edit wins, the other version is kept as a copy.
        val localChangedAt = st.localChangedAt[lp.id] ?: 0L
        if (remote.updatedAt >= localChangedAt) {
            copyLocal(lp.id, "${lp.name} (conflitto)")
            applyRemote(remote)
            st.playlists[remote.id] = SyncedPlaylist(remote.rev, currentHash(remote.id) ?: remote.hash())
            st.localChangedAt.remove(remote.id)
            return true // the copy is new and needs pushing
        } else {
            applyRemote(remote.copy(id = PlaylistEntity.generatePlaylistId(), name = "${remote.name} (conflitto)"))
            st.playlists[remote.id] = SyncedPlaylist(remote.rev, synced?.hash.orEmpty())
            return true
        }
    }

    private fun currentHash(id: String): String? =
        database.melodiaSyncRowsBlocking().toLocalPlaylists()[id]?.hash

    private fun remoteItems(playlistId: String): List<RemoteItem> =
        database.playlistSongsBlocking(playlistId)
            .filter { !it.song.song.isLocal }
            .map { ps ->
                val song = ps.song
                RemoteItem(
                    songId = song.song.id,
                    setVideoId = ps.map.setVideoId.orEmpty(),
                    title = song.song.title,
                    artists = song.artists.joinToString("\n") { it.name },
                    album = song.song.albumName.orEmpty(),
                    duration = song.song.duration.coerceAtLeast(0),
                    thumbnailUrl = song.song.thumbnailUrl.orEmpty(),
                )
            }

    private suspend fun deleteLocal(id: String) {
        database.withTransaction {
            playlistBlocking(id)?.let { delete(it.playlist) }
        }
    }

    /** Writes the server's version of a playlist into Room, keeping local-file songs at the end. */
    private suspend fun applyRemote(remote: RemotePlaylist) {
        database.withTransaction {
            val existing = playlistBlocking(remote.id)?.playlist
            val now = LocalDateTime.now()
            val localFileSongs = if (existing == null) emptyList() else
                playlistSongsBlocking(remote.id).filter { it.song.song.isLocal }.map { it.map }
            if (existing == null) {
                insert(
                    PlaylistEntity(
                        id = remote.id,
                        name = remote.name,
                        bookmarkedAt = now,
                        isEditable = true,
                    ),
                )
            } else {
                update(existing.copy(name = remote.name, lastUpdateTime = now))
                clearPlaylist(remote.id)
            }
            var position = 0
            remote.items.forEach { item ->
                insert(
                    MediaMetadata(
                        id = item.songId,
                        title = item.title.ifBlank { item.songId },
                        artists = item.artists.split("\n").filter { it.isNotBlank() }
                            .map { MediaMetadata.Artist(id = null, name = it) },
                        duration = if (item.duration > 0) item.duration else -1,
                        thumbnailUrl = item.thumbnailUrl.ifBlank { null },
                        setVideoId = item.setVideoId.ifBlank { null },
                    ),
                )
                insert(
                    PlaylistSongMap(
                        playlistId = remote.id,
                        songId = item.songId,
                        position = position++,
                        setVideoId = item.setVideoId.ifBlank { null },
                    ),
                )
            }
            localFileSongs.forEach { map ->
                insert(map.copy(id = 0, position = position++))
            }
        }
    }

    /** Duplicates a local playlist under a new id, used to keep the losing side of a conflict. */
    private suspend fun copyLocal(id: String, name: String) {
        database.withTransaction {
            val source = playlistBlocking(id)?.playlist ?: return@withTransaction
            val newId = PlaylistEntity.generatePlaylistId()
            insert(
                PlaylistEntity(
                    id = newId,
                    name = name,
                    bookmarkedAt = LocalDateTime.now(),
                    isEditable = true,
                    thumbnailUrl = source.thumbnailUrl,
                ),
            )
            playlistSongsBlocking(id).forEachIndexed { index, ps ->
                insert(ps.map.copy(id = 0, playlistId = newId, position = index))
            }
        }
    }
}
