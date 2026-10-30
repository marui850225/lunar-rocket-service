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

// Open opens (creating if needed) the database file at path.
func Open(path string) (*DB, error) {
	pragmas := "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"
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
	if err := fn(&tx{ctx: ctx, tx: sqlTx, channel: channel}); err != nil {
		sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
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

func (db *DB) Channels(ctx context.Context) ([]storage.ChannelInfo, error) {
	rows, err := db.reader.QueryContext(ctx, `
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
	var out []storage.ChannelInfo
	for rows.Next() {
		var ci storage.ChannelInfo
		if err := rows.Scan(&ci.Channel, &ci.Launched, &ci.LastMessageNumber, &ci.PendingCount); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ci)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		out[i].PendingNumbers = []int{}
		if out[i].PendingCount == 0 {
			continue
		}
		nums, err := db.reader.QueryContext(ctx,
			`SELECT message_number FROM pending WHERE channel = ? ORDER BY message_number LIMIT ?`,
			out[i].Channel, maxDebugPending)
		if err != nil {
			return nil, err
		}
		for nums.Next() {
			var n int
			if err := nums.Scan(&n); err != nil {
				nums.Close()
				return nil, err
			}
			out[i].PendingNumbers = append(out[i].PendingNumbers, n)
		}
		nums.Close()
	}
	if out == nil {
		out = []storage.ChannelInfo{}
	}
	return out, nil
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
		t.channel, m.Number, string(m.Type), formatTime(m.Time), body)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (t *tx) PendingAfter(after int) ([]model.Message, error) {
	rows, err := t.tx.QueryContext(t.ctx, `
		SELECT message_number, message_type, message_time, body FROM pending
		WHERE channel = ? AND message_number > ? ORDER BY message_number`, t.channel, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Message
	for rows.Next() {
		var (
			m       model.Message
			typ     string
			msgTime string
			body    string
		)
		if err := rows.Scan(&m.Number, &typ, &msgTime, &body); err != nil {
			return nil, err
		}
		m.Channel, m.Type = t.channel, model.MessageType(typ)
		if m.Time, err = parseTime(msgTime); err != nil {
			return nil, err
		}
		if m.Payload, err = decodePayload(m.Type, body); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (t *tx) DeletePending(numbers ...int) error {
	for _, n := range numbers {
		if _, err := t.tx.ExecContext(t.ctx,
			`DELETE FROM pending WHERE channel = ? AND message_number = ?`, t.channel, n); err != nil {
			return err
		}
	}
	return nil
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

// Buffered payloads are stored as JSON of the model structs, keyed by type.

func encodePayload(p model.Payload) (string, error) {
	b, err := json.Marshal(p)
	return string(b), err
}

func decodePayload(t model.MessageType, body string) (model.Payload, error) {
	switch t {
	case model.TypeLaunched:
		return unmarshalAs[model.Launched](t, body)
	case model.TypeSpeedIncreased:
		return unmarshalAs[model.SpeedIncreased](t, body)
	case model.TypeSpeedDecreased:
		return unmarshalAs[model.SpeedDecreased](t, body)
	case model.TypeMissionChanged:
		return unmarshalAs[model.MissionChanged](t, body)
	case model.TypeExploded:
		return unmarshalAs[model.Exploded](t, body)
	}
	return nil, fmt.Errorf("unknown message type %q in pending", t)
}

func unmarshalAs[P model.Payload](t model.MessageType, body string) (model.Payload, error) {
	var p P
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return nil, fmt.Errorf("decoding pending %s body: %w", t, err)
	}
	return p, nil
}
