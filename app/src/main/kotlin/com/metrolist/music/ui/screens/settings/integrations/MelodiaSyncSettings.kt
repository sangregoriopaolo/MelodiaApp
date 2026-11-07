/**
 * Melodia Sync settings: log in to the self-hosted server and sync playlists across phones.
 */

package com.metrolist.music.ui.screens.settings.integrations

import android.text.format.DateUtils
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.hilt.lifecycle.viewmodel.compose.hiltViewModel
import androidx.lifecycle.ViewModel
import androidx.navigation.NavController
import com.metrolist.music.LocalPlayerAwareWindowInsets
import com.metrolist.music.R
import com.metrolist.music.melodiasync.MelodiaSync
import com.metrolist.music.melodiasync.MelodiaSyncServerKey
import com.metrolist.music.melodiasync.MelodiaSyncTokenKey
import com.metrolist.music.melodiasync.MelodiaSyncUsernameKey
import com.metrolist.music.ui.component.IconButton
import com.metrolist.music.ui.utils.backToMain
import com.metrolist.music.utils.rememberPreference
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.launch
import javax.inject.Inject

@HiltViewModel
class MelodiaSyncViewModel @Inject constructor(
    val sync: MelodiaSync,
) : ViewModel()

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MelodiaSyncSettings(
    navController: NavController,
    viewModel: MelodiaSyncViewModel = hiltViewModel(),
) {
    val scope = rememberCoroutineScope()
    val status by viewModel.sync.status.collectAsState()
    val savedServer by rememberPreference(MelodiaSyncServerKey, "")
    val savedUsername by rememberPreference(MelodiaSyncUsernameKey, "")
    val token by rememberPreference(MelodiaSyncTokenKey, "")
    val loggedIn = token.isNotBlank()

    var server by rememberSaveable(savedServer) { mutableStateOf(savedServer.ifBlank { "https://" }) }
    var username by rememberSaveable(savedUsername) { mutableStateOf(savedUsername) }
    var password by rememberSaveable { mutableStateOf("") }
    var busy by rememberSaveable { mutableStateOf(false) }
    var loginError by rememberSaveable { mutableStateOf<String?>(null) }

    Column(
        Modifier
            .windowInsetsPadding(LocalPlayerAwareWindowInsets.current)
            .verticalScroll(rememberScrollState())
            .padding(horizontal = 16.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Text(
            text = "Sincronizza le playlist create in Melodia con il tuo server, così le ritrovi " +
                "su ogni telefono dove accedi con lo stesso utente.",
            style = MaterialTheme.typography.bodyMedium,
        )

        if (!loggedIn) {
            OutlinedTextField(
                value = server,
                onValueChange = { server = it; loginError = null },
                label = { Text("Indirizzo del server") },
                placeholder = { Text("https://melodia.example.com") },
                singleLine = true,
                enabled = !busy,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = username,
                onValueChange = { username = it; loginError = null },
                label = { Text("Utente") },
                singleLine = true,
                enabled = !busy,
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(
                value = password,
                onValueChange = { password = it; loginError = null },
                label = { Text("Password") },
                singleLine = true,
                enabled = !busy,
                visualTransformation = PasswordVisualTransformation(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                modifier = Modifier.fillMaxWidth(),
            )
            loginError?.let {
                Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            }
            Button(
                onClick = {
                    busy = true
                    loginError = null
                    scope.launch {
                        viewModel.sync.login(server, username, password)
                            .onSuccess { password = "" }
                            .onFailure { loginError = it.message ?: "Accesso non riuscito" }
                        busy = false
                    }
                },
                enabled = !busy && server.length > 8 && username.isNotBlank() && password.isNotBlank(),
                modifier = Modifier.fillMaxWidth(),
            ) {
                if (busy) {
                    CircularProgressIndicator(modifier = Modifier.size(16.dp), strokeWidth = 2.dp)
                } else {
                    Text("Accedi")
                }
            }
        } else {
            Text("Connesso come $savedUsername", style = MaterialTheme.typography.titleMedium)
            Text(savedServer, style = MaterialTheme.typography.bodySmall)

            Row(verticalAlignment = Alignment.CenterVertically) {
                if (status.running) {
                    CircularProgressIndicator(modifier = Modifier.size(16.dp), strokeWidth = 2.dp)
                    Spacer(Modifier.size(8.dp))
                    Text("Sincronizzazione in corso…", style = MaterialTheme.typography.bodyMedium)
                } else {
                    val last = if (status.lastSyncAt > 0) {
                        DateUtils.getRelativeTimeSpanString(status.lastSyncAt).toString()
                    } else {
                        "mai"
                    }
                    Text("Ultima sincronizzazione: $last", style = MaterialTheme.typography.bodyMedium)
                }
            }
            status.error?.let {
                Text("Errore: $it", color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall)
            }

            Button(
                onClick = { scope.launch { viewModel.sync.syncNow() } },
                enabled = !status.running,
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text("Sincronizza ora")
            }
            OutlinedButton(
                onClick = {
                    busy = true
                    scope.launch {
                        viewModel.sync.logout()
                        busy = false
                    }
                },
                enabled = !busy,
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text("Esci")
            }
            Text(
                "Uscendo, le playlist restano su questo telefono ma smettono di sincronizzarsi.",
                style = MaterialTheme.typography.bodySmall,
            )
        }
    }

    TopAppBar(
        title = { Text("Melodia Sync") },
        navigationIcon = {
            IconButton(
                onClick = navController::navigateUp,
                onLongClick = navController::backToMain,
            ) {
                Icon(
                    painterResource(R.drawable.arrow_back),
                    contentDescription = null,
                )
            }
        },
    )
}
