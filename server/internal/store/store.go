// Package store keeps users, device sessions and playlists in a single SQLite file.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrExists        = errors.New("already exists")
	ErrBadCredential = errors.New("invalid username or password")
	ErrConflict      = errors.New("revision conflict")
)

const bcryptCost = 12

type Store struct {
	db *sql.DB
}

type User struct {
	ID        int64
	Username  string
	CreatedAt time.Time
}

type Session struct {
	UserID     int64
	Username   string
	DeviceName string
}

// Item is one song in a playlist. Metadata travels with the song so another
// phone can show the playlist before it asks YouTube for anything.
type Item struct {
	SongID       string `json:"songId"`
	SetVideoID   string `json:"setVideoId,omitempty"`
	Title        string `json:"title"`
	Artists      string `json:"artists,omitempty"`
	Album        string `json:"album,omitempty"`
	Duration     int    `json:"duration,omitempty"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
}

type Playlist struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Rev       int64  `json:"rev"`
	UpdatedAt int64  `json:"updatedAt"` // unix milliseconds, set by the phone
	Deleted   bool   `json:"deleted"`
	Items     []Item `json:"items"`
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
	password_hash TEXT NOT NULL,
	rev           INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash   TEXT PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	device_name  TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE TABLE IF NOT EXISTS playlists (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	id         TEXT NOT NULL,
	name       TEXT NOT NULL,
	rev        INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	deleted    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, id)
);
CREATE INDEX IF NOT EXISTS playlists_rev ON playlists(user_id, rev);
CREATE TABLE IF NOT EXISTS playlist_items (
	user_id      INTEGER NOT NULL,
	playlist_id  TEXT NOT NULL,
	position     INTEGER NOT NULL,
	song_id      TEXT NOT NULL,
	set_video_id TEXT NOT NULL DEFAULT '',
	title        TEXT NOT NULL DEFAULT '',
	artists      TEXT NOT NULL DEFAULT '',
	album        TEXT NOT NULL DEFAULT '',
	duration     INTEGER NOT NULL DEFAULT 0,
	thumbnail    TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (user_id, playlist_id, position),
	FOREIGN KEY (user_id, playlist_id) REFERENCES playlists(user_id, id) ON DELETE CASCADE
);
`

// Open opens (and creates if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: no lock contention, minimal memory. Plenty for a family.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().UnixMilli() }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---- users ----

func (s *Store) CreateUser(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || len(password) < 8 {
		return errors.New("username required and password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, created_at) VALUES(?, ?, ?)`,
		username, string(hash), now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrExists
	}
	return err
}

func (s *Store) SetPassword(ctx context.Context, username, password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), id); err != nil {
		return err
	}
	// A new password logs out every phone.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteUser(ctx context.Context, username string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	for _, q := range []string{
		`DELETE FROM playlist_items WHERE user_id = ?`,
		`DELETE FROM playlists WHERE user_id = ?`,
		`DELETE FROM sessions WHERE user_id = ?`,
		`DELETE FROM users WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created int64
		if err := rows.Scan(&u.ID, &u.Username, &created); err != nil {
			return nil, err
		}
		u.CreatedAt = time.UnixMilli(created)
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- sessions ----

// Login checks the password and returns a new random bearer token for this device.
func (s *Store) Login(ctx context.Context, username, password, device string) (string, error) {
	var id int64
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, password_hash FROM users WHERE username = ?`, strings.TrimSpace(username)).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Same cost as a real check, so timing does not reveal which usernames exist.
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return "", ErrBadCredential
	}
	if err != nil {
		return "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", ErrBadCredential
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if device = strings.TrimSpace(device); device == "" {
		device = "unknown"
	}
	t := now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions(token_hash, user_id, device_name, created_at, last_seen_at) VALUES(?, ?, ?, ?, ?)`,
		hashToken(token), id, device, t, t)
	if err != nil {
		return "", err
	}
	return token, nil
}

var dummyHash = func() string {
	h, _ := bcrypt.GenerateFromPassword([]byte("melodia-dummy-password"), bcryptCost)
	return string(h)
}()

// Authenticate resolves a bearer token to its session.
func (s *Store) Authenticate(ctx context.Context, token string) (Session, error) {
	var sess Session
	var lastSeen int64
	h := hashToken(token)
	err := s.db.QueryRowContext(ctx, `
		SELECT s.user_id, u.username, s.device_name, s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?`, h).Scan(&sess.UserID, &sess.Username, &sess.DeviceName, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	// Refresh last_seen at most once an hour to avoid a write per request.
	if t := now(); t-lastSeen > time.Hour.Milliseconds() {
		s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, t, h)
	}
	return sess, nil
}

func (s *Store) Logout(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// ---- playlists ----

// Changes returns every playlist (tombstones included) changed after rev,
// plus the user's current revision.
func (s *Store) Changes(ctx context.Context, userID, since int64) ([]Playlist, int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT rev FROM users WHERE id = ?`, userID).Scan(&current); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, rev, updated_at, deleted FROM playlists
		WHERE user_id = ? AND rev > ? ORDER BY rev`, userID, since)
	if err != nil {
		return nil, 0, err
	}
	var out []Playlist
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Rev, &p.UpdatedAt, &p.Deleted); err != nil {
			rows.Close()
			return nil, 0, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for i := range out {
		if out[i].Deleted {
			out[i].Items = []Item{}
			continue
		}
		if out[i].Items, err = loadItems(ctx, tx, userID, out[i].ID); err != nil {
			return nil, 0, err
		}
	}
	if out == nil {
		out = []Playlist{}
	}
	return out, current, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadItems(ctx context.Context, q querier, userID int64, playlistID string) ([]Item, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT song_id, set_video_id, title, artists, album, duration, thumbnail
		FROM playlist_items WHERE user_id = ? AND playlist_id = ? ORDER BY position`, userID, playlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.SongID, &it.SetVideoID, &it.Title, &it.Artists, &it.Album, &it.Duration, &it.ThumbnailURL); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func getPlaylist(ctx context.Context, q querier, userID int64, id string) (Playlist, error) {
	p := Playlist{ID: id}
	err := q.QueryRowContext(ctx, `
		SELECT name, rev, updated_at, deleted FROM playlists WHERE user_id = ? AND id = ?`,
		userID, id).Scan(&p.Name, &p.Rev, &p.UpdatedAt, &p.Deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Playlist{}, ErrNotFound
	}
	if err != nil {
		return Playlist{}, err
	}
	p.Items = []Item{}
	if !p.Deleted {
		if p.Items, err = loadItems(ctx, q, userID, id); err != nil {
			return Playlist{}, err
		}
	}
	return p, nil
}

// ConflictError carries the server's copy so the phone can resolve the conflict.
type ConflictError struct {
	Current Playlist
}

func (e *ConflictError) Error() string { return ErrConflict.Error() }
func (e *ConflictError) Unwrap() error { return ErrConflict }

// Put stores the full playlist if baseRev matches the server's revision
// (0 means "I think the server does not have it"). Returns the new revision.
func (s *Store) Put(ctx context.Context, userID int64, p Playlist, baseRev int64) (int64, error) {
	return s.write(ctx, userID, p.ID, baseRev, func(tx *sql.Tx, rev int64) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO playlists(user_id, id, name, rev, updated_at, deleted) VALUES(?, ?, ?, ?, ?, 0)
			ON CONFLICT(user_id, id) DO UPDATE SET name = excluded.name, rev = excluded.rev,
				updated_at = excluded.updated_at, deleted = 0`,
			userID, p.ID, p.Name, rev, p.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM playlist_items WHERE user_id = ? AND playlist_id = ?`, userID, p.ID); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO playlist_items(user_id, playlist_id, position, song_id, set_video_id, title, artists, album, duration, thumbnail)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i, it := range p.Items {
			if _, err := stmt.ExecContext(ctx, userID, p.ID, i, it.SongID, it.SetVideoID,
				it.Title, it.Artists, it.Album, it.Duration, it.ThumbnailURL); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete leaves a tombstone so other phones learn about the deletion.
func (s *Store) Delete(ctx context.Context, userID int64, id string, baseRev, updatedAt int64) (int64, error) {
	return s.write(ctx, userID, id, baseRev, func(tx *sql.Tx, rev int64) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM playlist_items WHERE user_id = ? AND playlist_id = ?`, userID, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO playlists(user_id, id, name, rev, updated_at, deleted) VALUES(?, ?, '', ?, ?, 1)
			ON CONFLICT(user_id, id) DO UPDATE SET rev = excluded.rev, updated_at = excluded.updated_at, deleted = 1`,
			userID, id, rev, updatedAt)
		return err
	})
}

func (s *Store) write(ctx context.Context, userID int64, id string, baseRev int64, apply func(*sql.Tx, int64) error) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	cur, err := getPlaylist(ctx, tx, userID, id)
	switch {
	case errors.Is(err, ErrNotFound):
		// Unknown to the server: accept whatever baseRev the phone sends.
	case err != nil:
		return 0, err
	case cur.Rev != baseRev:
		return 0, &ConflictError{Current: cur}
	}

	var rev int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE users SET rev = rev + 1 WHERE id = ? RETURNING rev`, userID).Scan(&rev); err != nil {
		return 0, err
	}
	if err := apply(tx, rev); err != nil {
		return 0, err
	}
	return rev, tx.Commit()
}
