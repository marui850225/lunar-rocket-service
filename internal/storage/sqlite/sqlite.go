// Package sqlite implements storage.Storage on a SQLite database file.
//
// Concurrency: the database is used through exactly one connection, so every
// query and transaction runs one at a time. Concurrent requests simply wait
// for the connection; there is no locking in our own code.
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
	db *sql.DB
}

var _ storage.Storage = (*DB)(nil)

// Open opens the database file at path, creating it if needed.
//
// WAL with synchronous=NORMAL: a commit survives a crash of this process; an
// OS crash or power loss can lose the last few commits but never corrupts the
// database. synchronous=FULL was measured about 100x slower.
func Open(path string) (*DB, error) {
	dsn := "file:" + url.PathEscape(path) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &DB{db: db}, nil
}

func (db *DB) Close() error { return db.db.Close() }

// InTx runs fn in a transaction. With one connection, no other query can run
// until it ends.
func (db *DB) InTx(ctx context.Context, fn func(storage.Tables) error) error {
	sqlTx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Undo everything unless Commit succeeds, also if fn returns an error or
	// panics. After a successful Commit this does nothing.
	defer sqlTx.Rollback()
	in := scope{ctx: ctx, tx: sqlTx}
	if err := fn(storage.Tables{Rockets: rocketTable{in}, Pending: pendingTable{in}}); err != nil {
		return err
	}
	return sqlTx.Commit()
}

// scope is what both tables share: the open transaction and its context.
type scope struct {
	ctx context.Context
	tx  *sql.Tx
}

// --- rockets table -------------------------------------------------------------

// rocketTable implements storage.RocketTable.
type rocketTable struct{ scope }

const rocketColumns = `channel, type, speed, mission, status, explosion_reason, launched_at, updated_at, last_message_number`

func (t rocketTable) Get(channel string) (model.Rocket, bool, error) {
	row := t.tx.QueryRowContext(t.ctx, `SELECT `+rocketColumns+` FROM rockets WHERE channel = ?`, channel)
	r, err := scanRocket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Rocket{}, false, nil
	}
	return r, err == nil, err
}

func (t rocketTable) Save(r model.Rocket) error {
	if r.ID == "" {
		return errors.New("saving a rocket without an ID")
	}
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO rockets (`+rocketColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel) DO UPDATE SET
			type = excluded.type, speed = excluded.speed, mission = excluded.mission,
			status = excluded.status, explosion_reason = excluded.explosion_reason,
			launched_at = excluded.launched_at, updated_at = excluded.updated_at,
			last_message_number = excluded.last_message_number`,
		r.ID, nullString(r.Type), r.Speed, nullString(r.Mission), nullString(string(r.Status)),
		nullString(r.ExplosionReason), nullTime(r.LaunchedAt), nullTime(r.UpdatedAt), r.LastMessageNumber)
	return err
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

func (t rocketTable) List(opts storage.ListOptions) ([]model.Rocket, error) {
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

	rows, err := t.tx.QueryContext(t.ctx, query)
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

// --- pending table -------------------------------------------------------------

// pendingTable implements storage.PendingTable.
type pendingTable struct{ scope }

func (t pendingTable) Add(m model.Message) (bool, error) {
	body, err := encodePayload(m.Payload)
	if err != nil {
		return false, err
	}
	res, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO pending (channel, message_number, message_type, message_time, body)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (channel, message_number) DO NOTHING`,
		m.Channel, m.Number, string(m.Type()), formatTime(m.Time), body)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (t pendingTable) Get(channel string, number int) (model.Message, bool, error) {
	var typ, msgTime, body string
	err := t.tx.QueryRowContext(t.ctx, `
		SELECT message_type, message_time, body FROM pending
		WHERE channel = ? AND message_number = ?`, channel, number).Scan(&typ, &msgTime, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, false, nil
	}
	if err != nil {
		return model.Message{}, false, err
	}
	m := model.Message{Channel: channel, Number: number}
	if m.Time, err = parseTime(msgTime); err != nil {
		return model.Message{}, false, err
	}
	if m.Payload, err = decodePayload(model.MessageType(typ), body); err != nil {
		return model.Message{}, false, err
	}
	return m, true, nil
}

func (t pendingTable) Delete(channel string, from, to int) error {
	_, err := t.tx.ExecContext(t.ctx,
		`DELETE FROM pending WHERE channel = ? AND message_number BETWEEN ? AND ?`, channel, from, to)
	return err
}

// Summary reads every channel's progress and buffer. It also reads the
// rockets table, for each channel's last_message_number.
func (t pendingTable) Summary() ([]model.ChannelInfo, error) {
	rows, err := t.tx.QueryContext(t.ctx, `
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
	out := []model.ChannelInfo{}
	index := map[string]int{}
	for rows.Next() {
		ci := model.ChannelInfo{PendingNumbers: []int{}}
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

	nums, err := t.tx.QueryContext(t.ctx, `
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

// Buffered payloads are stored as JSON, using the publisher's field names
// (the json tags on the model types).

func encodePayload(p model.Payload) (string, error) {
	b, err := json.Marshal(p)
	return string(b), err
}

func decodePayload(t model.MessageType, body string) (model.Payload, error) {
	switch t {
	case model.TypeLaunched:
		return unmarshalAs[model.Launched](body)
	case model.TypeSpeedIncreased:
		return unmarshalAs[model.SpeedIncreased](body)
	case model.TypeSpeedDecreased:
		return unmarshalAs[model.SpeedDecreased](body)
	case model.TypeMissionChanged:
		return unmarshalAs[model.MissionChanged](body)
	case model.TypeExploded:
		return unmarshalAs[model.Exploded](body)
	}
	return nil, fmt.Errorf("unknown message type %q in pending", t)
}

func unmarshalAs[P model.Payload](body string) (model.Payload, error) {
	var p P
	err := json.Unmarshal([]byte(body), &p)
	return p, err
}
