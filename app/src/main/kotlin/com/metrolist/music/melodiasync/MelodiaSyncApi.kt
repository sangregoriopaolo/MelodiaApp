/**
 * Melodia Sync: client for the self-hosted companion server in server/.
 * API contract: docs/sync-api.md
 */

package com.metrolist.music.melodiasync

import io.ktor.client.HttpClient
import io.ktor.client.call.body
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.HttpTimeout
import io.ktor.client.plugins.contentnegotiation.ContentNegotiation
import io.ktor.client.request.bearerAuth
import io.ktor.client.request.delete
import io.ktor.client.request.get
import io.ktor.client.request.parameter
import io.ktor.client.request.post
import io.ktor.client.request.put
import io.ktor.client.request.setBody
import io.ktor.client.statement.HttpResponse
import io.ktor.http.ContentType
import io.ktor.http.contentType
import io.ktor.serialization.kotlinx.json.json
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

@Serializable
data class LoginRequest(val username: String, val password: String, val deviceName: String)

@Serializable
data class LoginResponse(val token: String, val username: String = "")

@Serializable
data class RemoteItem(
    val songId: String,
    val setVideoId: String = "",
    val title: String = "",
    val artists: String = "", // artist names separated by "\n"
    val album: String = "",
    val duration: Int = 0,
    val thumbnailUrl: String = "",
)

@Serializable
data class RemotePlaylist(
    val id: String,
    val name: String = "",
    val rev: Long = 0,
    val updatedAt: Long = 0,
    val deleted: Boolean = false,
    val items: List<RemoteItem> = emptyList(),
)

@Serializable
data class SyncResponse(val rev: Long, val playlists: List<RemotePlaylist> = emptyList())

@Serializable
data class PutRequest(val baseRev: Long, val name: String, val updatedAt: Long, val items: List<RemoteItem>)

@Serializable
data class RevResponse(val rev: Long)

@Serializable
data class ConflictResponse(val current: RemotePlaylist)

sealed class WriteResult {
    data class Ok(val rev: Long) : WriteResult()
    data class Conflict(val current: RemotePlaylist) : WriteResult()
}

sealed class MelodiaSyncException(message: String) : Exception(message) {
    class BadCredentials : MelodiaSyncException("Utente o password errati")
    class TooManyAttempts : MelodiaSyncException("Troppi tentativi falliti, riprova più tardi")
    class Unauthorized : MelodiaSyncException("Sessione scaduta, accedi di nuovo")
    class Http(code: Int) : MelodiaSyncException("Errore del server ($code)")
}

class MelodiaSyncApi(baseUrl: String, private val token: String? = null) {
    private val base = baseUrl.trim().trimEnd('/')

    private suspend fun HttpResponse.check(): HttpResponse {
        when (status.value) {
            in 200..299, 409 -> return this
            401 -> throw MelodiaSyncException.Unauthorized()
            429 -> throw MelodiaSyncException.TooManyAttempts()
            else -> throw MelodiaSyncException.Http(status.value)
        }
    }

    suspend fun login(username: String, password: String, deviceName: String): LoginResponse {
        val response = client.post("$base/api/v1/login") {
            contentType(ContentType.Application.Json)
            setBody(LoginRequest(username.trim(), password, deviceName))
        }
        if (response.status.value == 401) throw MelodiaSyncException.BadCredentials()
        return response.check().body()
    }

    suspend fun logout() {
        client.post("$base/api/v1/logout") { auth() }
    }

    suspend fun changes(since: Long): SyncResponse =
        client.get("$base/api/v1/sync") {
            auth()
            parameter("since", since)
        }.check().body()

    suspend fun put(id: String, request: PutRequest): WriteResult =
        client.put("$base/api/v1/playlists/$id") {
            auth()
            contentType(ContentType.Application.Json)
            setBody(request)
        }.check().toWriteResult()

    suspend fun delete(id: String, baseRev: Long, updatedAt: Long): WriteResult =
        client.delete("$base/api/v1/playlists/$id") {
            auth()
            parameter("baseRev", baseRev)
            parameter("updatedAt", updatedAt)
        }.check().toWriteResult()

    private fun io.ktor.client.request.HttpRequestBuilder.auth() {
        token?.let { bearerAuth(it) }
    }

    private suspend fun HttpResponse.toWriteResult(): WriteResult =
        if (status.value == 409) WriteResult.Conflict(body<ConflictResponse>().current)
        else WriteResult.Ok(body<RevResponse>().rev)

    companion object {
        private val client by lazy {
            HttpClient(OkHttp) {
                expectSuccess = false
                install(HttpTimeout) {
                    requestTimeoutMillis = 30_000
                    connectTimeoutMillis = 15_000
                }
                install(ContentNegotiation) {
                    json(
                        Json {
                            ignoreUnknownKeys = true
                            encodeDefaults = true
                        },
                    )
                }
            }
        }
    }
}
