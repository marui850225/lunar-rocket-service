// Package sqlite implements storage.Storage on a SQLite database file.
//
// Concurrency: SQLite allows one writer at a time. All InChannel transactions
// go through a single connection and start with BEGIN IMMEDIATE, so they are
// serialized in Go instead of failing with SQLITE_BUSY. Reads use a separate
// pool and, in WAL mode, never block or wait for the writer.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"rockets-service/internal/model"
	"rockets-service/internal/storage"
)

const schema = `
CREATE TABLE IF NOT EXISTS rockets (
	channel             TEXT    PRIMARY KEY,
	type                TEXT,
	speed               INTEGER NOT NULL DEFAULT 0,
	mission             TEXT,
	status              TEXT,              -- NULL until RocketLaunched is processed
	explosion_reason    TEXT,
	launched_at         TEXT,              -- UTC, fixed width: sorts as text
	updated_at          TEXT,
	last_message_number INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS pending (
	channel        TEXT    NOT NULL,
	message_number INTEGER NOT NULL,
	message_type   TEXT    NOT NULL,
	message_time   TEXT    NOT NULL,
	body           TEXT    NOT NULL,
	PRIMARY KEY (channel, message_number)
);`

// timeFormat is RFC 3339 with fixed-width nanoseconds. Times are stored in
// UTC so that text order equals time order.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// maxDebugPending caps ChannelInfo.PendingNumbers.
const maxDebugPending = 100

// DB is a storage.Storage backed by one SQLite file.
type DB struct {
	writer *sql.DB // exactly one connection: all writes are serialized
	reader *sql.DB
}

var _ storage.Storage = (*DB)(nil)

// Sync sets how often SQLite fsyncs, trading durability for speed.
type Sync string

const (
	// SyncNormal fsyncs at WAL checkpoints only. A commit survives a crash of
	// this process; an OS crash or power loss can lose the last few commits,
	// but never corrupts the database. About 100x faster than SyncFull.
	SyncNormal Sync = "normal"
	// SyncFull fsyncs every commit: a commit survives power loss.
	SyncFull Sync = "full"
)

// Open opens (creating if needed) the database file at path.
func Open(path string, sync Sync) (*DB, error) {
	var level string
	switch sync {
	case SyncNormal:
		level = "NORMAL"
	case SyncFull:
		level = "FULL"
	default:
		return nil, fmt.Errorf("unknown sync mode %q (want normal or full)", sync)
	}
	pragmas := "_pragma=journal_mode(WAL)&_pragma=synchronous(" + level + ")&_pragma=busy_timeout(5000)"
	base := "file:" + url.PathEscape(path) + "?" + pragmas

	writer, err := sql.Open("sqlite", base+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(schema); err != nil {
		writer.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	reader, err := sql.Open("sqlite", base+"&_pragma=query_only(1)")
	if err != nil {
		writer.Close()
		return nil, err
	}
	return &DB{writer: writer, reader: reader}, nil
}

func (db *DB) Close() error {
	return errors.Join(db.reader.Close(), db.writer.Close())
}

// InChannel runs fn in a write transaction. Because every write transaction
// is serialized, it also excludes every other channel; the channel argument
// scopes what the Tx reads and writes.
func (db *DB) InChannel(ctx context.Context, channel string, fn func(storage.Tx) error) error {
	sqlTx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Roll back on any exit but a successful commit, including a panic in
	// fn: otherwise the only writer connection would stay inside an open
	// transaction and every later write would wait forever.
	committed := false
	defer func() {
		if !committed {
			sqlTx.Rollback()
		}
	}()
	if err := fn(&tx{ctx: ctx, tx: sqlTx, channel: channel}); err != nil {
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

const rocketColumns = `channel, type, speed, mission, status, explosion_reason, launched_at, updated_at, last_message_number`

func (db *DB) GetRocket(ctx context.Context, channel string) (model.Rocket, bool, error) {
	row := db.reader.QueryRowContext(ctx, `SELECT `+rocketColumns+` FROM rockets WHERE channel = ?`, channel)
	r, err := scanRocket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Rocket{}, false, nil
	}
	return r, err == nil, err
}

// sortColumns maps each sort field to a fixed column name. User input is
// never placed in SQL: an unknown field is an error.
var sortColumns = map[storage.SortField]string{
	storage.SortID:         "channel",
	storage.SortType:       "type",
	storage.SortSpeed:      "speed",
	storage.SortMission:    "mission",
	storage.SortStatus:     "status",
	storage.SortLaunchedAt: "launched_at",
	storage.SortUpdatedAt:  "updated_at",
}

func (db *DB) ListRockets(ctx context.Context, opts storage.ListOptions) ([]model.Rocket, error) {
	column, ok := sortColumns[opts.SortBy]
	if !ok {
		return nil, fmt.Errorf("unknown sort field %q", opts.SortBy)
	}
	query := `SELECT ` + rocketColumns + ` FROM rockets`
	if opts.LaunchedOnly {
		query += ` WHERE status IS NOT NULL`
	}
	dir := "ASC"
	if opts.Desc {
		dir = "DESC"
	}
	query += ` ORDER BY ` + column + ` ` + dir + `, channel ASC`

	rows, err := db.reader.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Rocket{}
	for rows.Next() {
		r, err := scanRocket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Channels reads every channel's bookkeeping in one read transaction, so
// counts and number lists come from the same snapshot.
func (db *DB) Channels(ctx context.Context) ([]storage.ChannelInfo, error) {
	rtx, err := db.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer rtx.Rollback()

	rows, err := rtx.QueryContext(ctx, `
		SELECT c.channel,
		       r.status IS NOT NULL,
		       COALESCE(r.last_message_number, 0),
		       (SELECT COUNT(*) FROM pending p WHERE p.channel = c.channel)
		FROM (SELECT channel FROM rockets UNION SELECT channel FROM pending) c
		LEFT JOIN rockets r ON r.channel = c.channel
		ORDER BY c.channel`)
	if err != nil {
		return nil, err
	}
	out := []storage.ChannelInfo{}
	index := map[string]int{}
	for rows.Next() {
		ci := storage.ChannelInfo{PendingNumbers: []int{}}
		if err := rows.Scan(&ci.Channel, &ci.Launched, &ci.LastMessageNumber, &ci.PendingCount); err != nil {
			rows.Close()
			return nil, err
		}
		index[ci.Channel] = len(out)
		out = append(out, ci)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	nums, err := rtx.QueryContext(ctx, `
		SELECT channel, message_number FROM (
			SELECT channel, message_number,
			       ROW_NUMBER() OVER (PARTITION BY channel ORDER BY message_number) AS rn
			FROM pending)
		WHERE rn <= ? ORDER BY channel, message_number`, maxDebugPending)
	if err != nil {
		return nil, err
	}
	defer nums.Close()
	for nums.Next() {
		var ch string
		var n int
		if err := nums.Scan(&ch, &n); err != nil {
			return nil, err
		}
		if i, ok := index[ch]; ok {
			out[i].PendingNumbers = append(out[i].PendingNumbers, n)
		}
	}
	return out, nums.Err()
}

// --- transaction -----------------------------------------------------------

type tx struct {
	ctx     context.Context
	tx      *sql.Tx
	channel string
}

func (t *tx) Rocket() (model.Rocket, error) {
	row := t.tx.QueryRowContext(t.ctx, `SELECT `+rocketColumns+` FROM rockets WHERE channel = ?`, t.channel)
	r, err := scanRocket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Rocket{}, nil
	}
	return r, err
}

func (t *tx) SaveRocket(r model.Rocket) error {
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO rockets (`+rocketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel) DO UPDATE SET
			type = excluded.type, speed = excluded.speed, mission = excluded.mission,
			status = excluded.status, explosion_reason = excluded.explosion_reason,
			launched_at = excluded.launched_at, updated_at = excluded.updated_at,
			last_message_number = excluded.last_message_number`,
		t.channel, nullString(r.Type), r.Speed, nullString(r.Mission), nullString(string(r.Status)),
		nullString(r.ExplosionReason), nullTime(r.LaunchedAt), nullTime(r.UpdatedAt), r.LastMessageNumber)
	return err
}

func (t *tx) AddPending(m model.Message) (bool, error) {
	body, err := encodePayload(m.Payload)
	if err != nil {
		return false, err
	}
	res, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO pending (channel, message_number, message_type, message_time, body)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (channel, message_number) DO NOTHING`,
		t.channel, m.Number, string(m.Type()), formatTime(m.Time), body)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (t *tx) Pending(number int) (model.Message, bool, error) {
	var typ, msgTime, body string
	err := t.tx.QueryRowContext(t.ctx, `
		SELECT message_type, message_time, body FROM pending
		WHERE channel = ? AND message_number = ?`, t.channel, number).Scan(&typ, &msgTime, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, false, nil
	}
	if err != nil {
		return model.Message{}, false, err
	}
	m := model.Message{Channel: t.channel, Number: number}
	if m.Time, err = parseTime(msgTime); err != nil {
		return model.Message{}, false, err
	}
	if m.Payload, err = decodePayload(model.MessageType(typ), body); err != nil {
		return model.Message{}, false, err
	}
	return m, true, nil
}

func (t *tx) DeletePending(from, to int) error {
	_, err := t.tx.ExecContext(t.ctx,
		`DELETE FROM pending WHERE channel = ? AND message_number BETWEEN ? AND ?`, t.channel, from, to)
	return err
}

func (t *tx) CountPending() (int, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM pending WHERE channel = ?`, t.channel).Scan(&n)
	return n, err
}

// --- row mapping -------------------------------------------------------------

type scanner interface{ Scan(dest ...any) error }

func scanRocket(s scanner) (model.Rocket, error) {
	var (
		r                                               model.Rocket
		typ, mission, status, reason, launched, updated sql.NullString
	)
	err := s.Scan(&r.ID, &typ, &r.Speed, &mission, &status, &reason, &launched, &updated, &r.LastMessageNumber)
	if err != nil {
		return model.Rocket{}, err
	}
	r.Type, r.Mission, r.Status, r.ExplosionReason = typ.String, mission.String, model.Status(status.String), reason.String
	if launched.Valid {
		if r.LaunchedAt, err = parseTime(launched.String); err != nil {
			return model.Rocket{}, err
		}
	}
	if updated.Valid {
		if r.UpdatedAt, err = parseTime(updated.String); err != nil {
			return model.Rocket{}, err
		}
	}
	return r, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeFormat, s) }

func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// Buffered payloads are stored as JSON in the storage format below. Its tags
// are fixed on purpose: renaming a model field must not change, or silently
// misread, what is already on disk. Decoding rejects unknown fields and
// requires every field, so a format mismatch is an error, not a zero value.

type storedLaunched struct {
	Type        *string `json:"type"`
	LaunchSpeed *int64  `json:"launchSpeed"`
	Mission     *string `json:"mission"`
}

type storedSpeed struct {
	By *int64 `json:"by"`
}

type storedMission struct {
	NewMission *string `json:"newMission"`
}

type storedExploded struct {
	Reason *string `json:"reason"`
}

func encodePayload(p model.Payload) (string, error) {
	var v any
	switch p := p.(type) {
	case model.Launched:
		v = storedLaunched{Type: &p.RocketType, LaunchSpeed: &p.LaunchSpeed, Mission: &p.Mission}
	case model.SpeedIncreased:
		v = storedSpeed{By: &p.By}
	case model.SpeedDecreased:
		v = storedSpeed{By: &p.By}
	case model.MissionChanged:
		v = storedMission{NewMission: &p.NewMission}
	case model.Exploded:
		v = storedExploded{Reason: &p.Reason}
	default:
		return "", fmt.Errorf("unknown payload %T", p)
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func decodePayload(t model.MessageType, body string) (model.Payload, error) {
	missing := fmt.Errorf("decoding pending %s body %s: missing field", t, body)
	switch t {
	case model.TypeLaunched:
		var v storedLaunched
		if err := strictUnmarshal(t, body, &v); err != nil {
			return nil, err
		}
		if v.Type == nil || v.LaunchSpeed == nil || v.Mission == nil {
			return nil, missing
		}
		return model.Launched{RocketType: *v.Type, LaunchSpeed: *v.LaunchSpeed, Mission: *v.Mission}, nil
	case model.TypeSpeedIncreased, model.TypeSpeedDecreased:
		var v storedSpeed
		if err := strictUnmarshal(t, body, &v); err != nil {
			return nil, err
		}
		if v.By == nil {
			return nil, missing
		}
		if t == model.TypeSpeedIncreased {
			return model.SpeedIncreased{By: *v.By}, nil
		}
		return model.SpeedDecreased{By: *v.By}, nil
	case model.TypeMissionChanged:
		var v storedMission
		if err := strictUnmarshal(t, body, &v); err != nil {
			return nil, err
		}
		if v.NewMission == nil {
			return nil, missing
		}
		return model.MissionChanged{NewMission: *v.NewMission}, nil
	case model.TypeExploded:
		var v storedExploded
		if err := strictUnmarshal(t, body, &v); err != nil {
			return nil, err
		}
		if v.Reason == nil {
			return nil, missing
		}
		return model.Exploded{Reason: *v.Reason}, nil
	}
	return nil, fmt.Errorf("unknown message type %q in pending", t)
}

func strictUnmarshal(t model.MessageType, body string, v any) error {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decoding pending %s body: %w", t, err)
	}
	return nil
}
