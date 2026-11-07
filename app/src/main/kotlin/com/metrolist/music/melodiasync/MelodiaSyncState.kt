/**
 * Melodia Sync: what this phone last agreed with the server, kept in a small JSON file
 * so the app's Room schema stays untouched.
 */

package com.metrolist.music.melodiasync

import org.json.JSONObject
import java.io.File

/** rev = server revision this phone last saw; hash = local content at that moment. */
data class SyncedPlaylist(val rev: Long, val hash: String)

class MelodiaSyncState(
    var lastRev: Long = 0,
    val playlists: MutableMap<String, SyncedPlaylist> = mutableMapOf(),
    /** When each playlist was last changed on this phone (unix ms), for conflict resolution. */
    val localChangedAt: MutableMap<String, Long> = mutableMapOf(),
) {
    fun save(file: File) {
        val json = JSONObject()
        json.put("lastRev", lastRev)
        json.put(
            "playlists",
            JSONObject().apply {
                playlists.forEach { (id, p) ->
                    put(id, JSONObject().put("rev", p.rev).put("hash", p.hash))
                }
            },
        )
        json.put("localChangedAt", JSONObject().apply { localChangedAt.forEach { (id, t) -> put(id, t) } })
        val tmp = File(file.parentFile, file.name + ".tmp")
        tmp.writeText(json.toString())
        tmp.renameTo(file)
    }

    companion object {
        fun load(file: File): MelodiaSyncState {
            if (!file.exists()) return MelodiaSyncState()
            return runCatching {
                val json = JSONObject(file.readText())
                val state = MelodiaSyncState(lastRev = json.optLong("lastRev", 0))
                json.optJSONObject("playlists")?.let { obj ->
                    obj.keys().forEach { id ->
                        val p = obj.getJSONObject(id)
                        state.playlists[id] = SyncedPlaylist(p.getLong("rev"), p.getString("hash"))
                    }
                }
                json.optJSONObject("localChangedAt")?.let { obj ->
                    obj.keys().forEach { id -> state.localChangedAt[id] = obj.getLong(id) }
                }
                state
            }.getOrElse { MelodiaSyncState() }
        }
    }
}
