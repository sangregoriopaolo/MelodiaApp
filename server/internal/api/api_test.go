package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sangregoriopaolo/melodiaapp/server/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, u := range []string{"paolo", "anna"} {
		if err := st.CreateUser(context.Background(), u, "password123"); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(New(st, false, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func login(t *testing.T, srv *httptest.Server, user, device string) string {
	t.Helper()
	code, out := call(t, srv, "POST", "/api/v1/login", "",
		map[string]string{"username": user, "password": "password123", "deviceName": device})
	if code != 200 {
		t.Fatalf("login %s: %d %v", user, code, out)
	}
	return out["token"].(string)
}

func playlistBody(baseRev int64, name string, updatedAt int64, songs ...string) map[string]any {
	items := []map[string]any{}
	for _, s := range songs {
		items = append(items, map[string]any{"songId": s, "title": "Song " + s, "artists": "Artist"})
	}
	return map[string]any{"baseRev": baseRev, "name": name, "updatedAt": updatedAt, "items": items}
}

func TestLoginAndAuth(t *testing.T) {
	srv := newTestServer(t)

	if code, _ := call(t, srv, "GET", "/api/v1/me", "", nil); code != 401 {
		t.Fatalf("no token: want 401, got %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/v1/login", "",
		map[string]string{"username": "paolo", "password": "wrong"}); code != 401 {
		t.Fatalf("bad password: want 401, got %d", code)
	}
	tok := login(t, srv, "Paolo", "Pixel") // usernames are case-insensitive
	code, me := call(t, srv, "GET", "/api/v1/me", tok, nil)
	if code != 200 || me["deviceName"] != "Pixel" {
		t.Fatalf("me: %d %v", code, me)
	}
	if code, _ := call(t, srv, "POST", "/api/v1/logout", tok, nil); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := call(t, srv, "GET", "/api/v1/me", tok, nil); code != 401 {
		t.Fatalf("after logout: want 401, got %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < loginMaxFailures; i++ {
		call(t, srv, "POST", "/api/v1/login", "", map[string]string{"username": "anna", "password": "nope"})
	}
	code, _ := call(t, srv, "POST", "/api/v1/login", "",
		map[string]string{"username": "anna", "password": "password123"})
	if code != 429 {
		t.Fatalf("want 429 after %d failures, got %d", loginMaxFailures, code)
	}
}

func TestSyncTwoPhones(t *testing.T) {
	srv := newTestServer(t)
	phoneA := login(t, srv, "paolo", "A")
	phoneB := login(t, srv, "paolo", "B")

	// Phone A creates a playlist.
	code, out := call(t, srv, "PUT", "/api/v1/playlists/LPabcdefgh", phoneA, playlistBody(0, "Road trip", 1000, "s1", "s2"))
	if code != 200 {
		t.Fatalf("put: %d %v", code, out)
	}
	rev1 := int64(out["rev"].(float64))

	// Phone B logs in fresh and pulls everything.
	code, out = call(t, srv, "GET", "/api/v1/sync?since=0", phoneB, nil)
	if code != 200 {
		t.Fatalf("sync: %d %v", code, out)
	}
	pls := out["playlists"].([]any)
	if len(pls) != 1 {
		t.Fatalf("want 1 playlist, got %v", pls)
	}
	p := pls[0].(map[string]any)
	items := p["items"].([]any)
	if p["name"] != "Road trip" || len(items) != 2 || items[0].(map[string]any)["songId"] != "s1" {
		t.Fatalf("unexpected playlist %v", p)
	}

	// Phone B edits it from rev1: accepted.
	code, out = call(t, srv, "PUT", "/api/v1/playlists/LPabcdefgh", phoneB, playlistBody(rev1, "Road trip", 2000, "s2", "s1", "s3"))
	if code != 200 {
		t.Fatalf("put B: %d %v", code, out)
	}
	rev2 := int64(out["rev"].(float64))

	// Phone A still thinks it is at rev1: conflict, with the server copy.
	code, out = call(t, srv, "PUT", "/api/v1/playlists/LPabcdefgh", phoneA, playlistBody(rev1, "Road trip", 1500, "s1"))
	if code != 409 {
		t.Fatalf("want 409, got %d %v", code, out)
	}
	cur := out["current"].(map[string]any)
	if int64(cur["rev"].(float64)) != rev2 || len(cur["items"].([]any)) != 3 {
		t.Fatalf("conflict payload %v", cur)
	}

	// Incremental pull from rev2 returns nothing new.
	_, out = call(t, srv, "GET", "/api/v1/sync?since="+itoa(rev2), phoneA, nil)
	if len(out["playlists"].([]any)) != 0 {
		t.Fatalf("want no changes, got %v", out["playlists"])
	}

	// Delete leaves a tombstone that the other phone sees.
	code, out = call(t, srv, "DELETE", "/api/v1/playlists/LPabcdefgh?baseRev="+itoa(rev2)+"&updatedAt=3000", phoneA, nil)
	if code != 200 {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, out = call(t, srv, "GET", "/api/v1/sync?since="+itoa(rev2), phoneB, nil)
	pls = out["playlists"].([]any)
	if len(pls) != 1 || pls[0].(map[string]any)["deleted"] != true {
		t.Fatalf("want tombstone, got %v", pls)
	}
}

func TestUsersAreIsolated(t *testing.T) {
	srv := newTestServer(t)
	paolo := login(t, srv, "paolo", "A")
	anna := login(t, srv, "anna", "B")

	call(t, srv, "PUT", "/api/v1/playlists/LPsameidxx", paolo, playlistBody(0, "Paolo's", 1000, "s1"))
	// Same playlist id for another user is a separate playlist, not a conflict.
	if code, out := call(t, srv, "PUT", "/api/v1/playlists/LPsameidxx", anna, playlistBody(0, "Anna's", 1000, "s9")); code != 200 {
		t.Fatalf("anna put: %d %v", code, out)
	}
	_, out := call(t, srv, "GET", "/api/v1/sync", anna, nil)
	pls := out["playlists"].([]any)
	if len(pls) != 1 || pls[0].(map[string]any)["name"] != "Anna's" {
		t.Fatalf("anna sees %v", pls)
	}
}

func TestValidation(t *testing.T) {
	srv := newTestServer(t)
	tok := login(t, srv, "paolo", "A")
	cases := []struct {
		path string
		body any
	}{
		{"/api/v1/playlists/bad%20id", playlistBody(0, "x", 1)},
		{"/api/v1/playlists/LPok", playlistBody(0, "", 1)},
		{"/api/v1/playlists/LPok", playlistBody(0, "x", 0)},
		{"/api/v1/playlists/LPok", map[string]any{"name": "x", "updatedAt": 1, "items": []map[string]any{{"title": "no id"}}}},
	}
	for i, c := range cases {
		if code, out := call(t, srv, "PUT", c.path, tok, c.body); code != 400 {
			t.Errorf("case %d: want 400, got %d %v", i, code, out)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
