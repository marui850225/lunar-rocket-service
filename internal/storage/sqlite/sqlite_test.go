package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rockets-service/internal/model"
	"rockets-service/internal/storage"
)

var ctx = context.Background()

func open(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func inChannel(t *testing.T, db *DB, ch string, fn func(storage.Tables) error) {
	t.Helper()
	if err := db.InChannel(ctx, ch, fn); err != nil {
		t.Fatal(err)
	}
}

var plusOne = time.FixedZone("+01:00", 3600)

func sampleRocket(id string) model.Rocket {
	return model.Rocket{
		ID: id, Type: "Falcon-9", Speed: -300, Mission: "ARTEMIS", Status: model.StatusExploded,
		ExplosionReason:   "PRESSURE_VESSEL_FAILURE",
		LaunchedAt:        time.Date(2022, 2, 2, 19, 39, 5, 863370000, plusOne),
		UpdatedAt:         time.Date(2022, 2, 2, 19, 41, 0, 1, plusOne),
		LastMessageNumber: 57,
	}
}

func sameRocket(a, b model.Rocket) bool {
	return a.ID == b.ID && a.Type == b.Type && a.Speed == b.Speed && a.Mission == b.Mission &&
		a.Status == b.Status && a.ExplosionReason == b.ExplosionReason &&
		a.LaunchedAt.Equal(b.LaunchedAt) && a.UpdatedAt.Equal(b.UpdatedAt) &&
		a.LastMessageNumber == b.LastMessageNumber
}

func TestRocketRoundTrip(t *testing.T) {
	db, _ := open(t)
	want := sampleRocket("a")
	inChannel(t, db, "a", func(tables storage.Tables) error { return tables.Rocket.Save(want) })

	got, found, err := db.GetRocket(ctx, "a")
	if err != nil || !found || !sameRocket(got, want) {
		t.Fatalf("GetRocket = %+v, %v, %v; want %+v", got, found, err, want)
	}
	inChannel(t, db, "a", func(tables storage.Tables) error {
		got, err := tables.Rocket.Get()
		if !sameRocket(got, want) {
			t.Errorf("Rocket.Get = %+v, %v", got, err)
		}
		return err
	})

	// Overwrite with fewer fields: empty strings and zero times become NULL and come back empty.
	inChannel(t, db, "a", func(tables storage.Tables) error {
		return tables.Rocket.Save(model.Rocket{ID: "a", LastMessageNumber: 3})
	})
	got, _, _ = db.GetRocket(ctx, "a")
	if got.Type != "" || got.Status != "" || !got.LaunchedAt.IsZero() || got.LastMessageNumber != 3 {
		t.Fatalf("after overwrite: %+v", got)
	}
}

func TestMissingRocket(t *testing.T) {
	db, _ := open(t)
	if _, found, err := db.GetRocket(ctx, "nope"); found || err != nil {
		t.Fatalf("found=%v err=%v", found, err)
	}
	inChannel(t, db, "nope", func(tables storage.Tables) error {
		r, err := tables.Rocket.Get()
		if r != (model.Rocket{}) {
			t.Errorf("Rocket.Get = %+v, want zero", r)
		}
		return err
	})
}

func msg(n int, p model.Payload) model.Message {
	return model.Message{Channel: "a", Number: n, Time: time.Date(2022, 2, 2, 10, 0, n, 0, plusOne), Payload: p}
}

func TestPending(t *testing.T) {
	db, _ := open(t)
	in := []model.Message{
		msg(5, model.Exploded{Reason: "X"}),
		msg(2, model.Launched{RocketType: "Atlas", LaunchSpeed: 9, Mission: "M"}),
		msg(4, model.MissionChanged{NewMission: "N"}),
		msg(3, model.SpeedDecreased{By: 7}),
		msg(1, model.SpeedIncreased{By: 6}),
	}
	inChannel(t, db, "a", func(tables storage.Tables) error {
		for _, m := range in {
			if added, err := tables.Pending.Add(m); !added || err != nil {
				t.Fatalf("AddPending #%d: %v %v", m.Number, added, err)
			}
		}
		if added, err := tables.Pending.Add(msg(3, model.SpeedDecreased{By: 999})); added || err != nil {
			t.Fatalf("duplicate AddPending: added=%v err=%v", added, err)
		}
		return nil
	})

	inChannel(t, db, "a", func(tables storage.Tables) error {
		for _, src := range in {
			g, found, err := tables.Pending.Get(src.Number)
			if err != nil || !found {
				t.Fatalf("Pending(%d): found=%v err=%v", src.Number, found, err)
			}
			if g.Number != src.Number || g.Channel != "a" || g.Type() != src.Type() || g.Payload != src.Payload || !g.Time.Equal(src.Time) {
				t.Errorf("Pending(%d) = %+v, want %+v", src.Number, g, src)
			}
		}
		if g, _, _ := tables.Pending.Get(3); g.Payload.(model.SpeedDecreased).By != 7 {
			t.Errorf("duplicate overwrote first copy: %+v", g.Payload)
		}
		if _, found, err := tables.Pending.Get(99); found || err != nil {
			t.Errorf("Pending(99): found=%v err=%v", found, err)
		}

		if err := tables.Pending.Delete(2, 4); err != nil {
			return err
		}
		for k, want := range map[int]bool{1: true, 2: false, 3: false, 4: false, 5: true} {
			if _, found, _ := tables.Pending.Get(k); found != want {
				t.Errorf("after DeletePending(2, 4): Pending(%d) found=%v, want %v", k, found, want)
			}
		}
		return nil
	})
}

func TestRollbackOnError(t *testing.T) {
	db, _ := open(t)
	boom := errors.New("boom")
	err := db.InChannel(ctx, "a", func(tables storage.Tables) error {
		tables.Rocket.Save(sampleRocket("a"))
		tables.Pending.Add(msg(9, model.SpeedIncreased{By: 1}))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, found, _ := db.GetRocket(ctx, "a"); found {
		t.Fatal("rocket saved despite rollback")
	}
	if chs, _ := db.Channels(ctx); len(chs) != 0 {
		t.Fatalf("channels after rollback: %+v", chs)
	}
}

func TestListRockets(t *testing.T) {
	db, _ := open(t)
	base := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	rockets := []model.Rocket{
		// launchedAt deliberately mixes time zones and nanosecond widths:
		// sorting must follow the instant, not the text.
		{ID: "c", Type: "Atlas", Speed: 300, Mission: "Z", Status: model.StatusActive,
			LaunchedAt: base.In(plusOne), UpdatedAt: base.Add(3 * time.Second)},
		{ID: "a", Type: "Falcon", Speed: 500, Mission: "Y", Status: model.StatusExploded,
			LaunchedAt: base.Add(time.Second + 5), UpdatedAt: base.Add(1 * time.Second)},
		{ID: "b", Type: "Atlas", Speed: 500, Mission: "X", Status: model.StatusActive,
			LaunchedAt: base.Add(time.Second + 500*time.Millisecond), UpdatedAt: base.Add(2 * time.Second)},
		{ID: "z-unlaunched", LastMessageNumber: 1},
	}
	for _, r := range rockets {
		inChannel(t, db, r.ID, func(tables storage.Tables) error { return tables.Rocket.Save(r) })
	}

	tests := []struct {
		opts storage.ListOptions
		want string
	}{
		{storage.ListOptions{SortBy: storage.SortLaunchedAt, LaunchedOnly: true}, "c,a,b"},
		{storage.ListOptions{SortBy: storage.SortLaunchedAt, Desc: true, LaunchedOnly: true}, "b,a,c"},
		{storage.ListOptions{SortBy: storage.SortUpdatedAt, LaunchedOnly: true}, "a,b,c"},
		{storage.ListOptions{SortBy: storage.SortSpeed, LaunchedOnly: true}, "c,a,b"},
		{storage.ListOptions{SortBy: storage.SortSpeed, Desc: true, LaunchedOnly: true}, "a,b,c"}, // tie: id asc
		{storage.ListOptions{SortBy: storage.SortType, LaunchedOnly: true}, "b,c,a"},
		{storage.ListOptions{SortBy: storage.SortMission, LaunchedOnly: true}, "b,a,c"},
		{storage.ListOptions{SortBy: storage.SortStatus, LaunchedOnly: true}, "b,c,a"},
		{storage.ListOptions{SortBy: storage.SortID, Desc: true, LaunchedOnly: true}, "c,b,a"},
		{storage.ListOptions{SortBy: storage.SortID}, "a,b,c,z-unlaunched"},
	}
	for _, tt := range tests {
		got, err := db.ListRockets(ctx, tt.opts)
		if err != nil {
			t.Fatalf("%+v: %v", tt.opts, err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		if strings.Join(ids, ",") != tt.want {
			t.Errorf("%+v: got %v, want %s", tt.opts, ids, tt.want)
		}
	}

	if _, err := db.ListRockets(ctx, storage.ListOptions{SortBy: "speed; DROP TABLE rockets"}); err == nil {
		t.Fatal("unknown sort field accepted")
	}
	for _, f := range storage.SortFields {
		if _, ok := sortColumns[f]; !ok {
			t.Errorf("sort field %q has no column", f)
		}
	}
}

func TestSurvivesReopen(t *testing.T) {
	db, path := open(t)
	inChannel(t, db, "a", func(tables storage.Tables) error {
		if err := tables.Rocket.Save(sampleRocket("a")); err != nil {
			return err
		}
		_, err := tables.Pending.Add(msg(60, model.SpeedIncreased{By: 1}))
		return err
	})
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if got, found, _ := db2.GetRocket(ctx, "a"); !found || !sameRocket(got, sampleRocket("a")) {
		t.Fatalf("after reopen: %+v %v", got, found)
	}
	if chs, _ := db2.Channels(ctx); len(chs) != 1 || chs[0].PendingCount != 1 {
		t.Fatalf("pending after reopen: %+v", chs)
	}
}

func TestSettings(t *testing.T) {
	db, _ := open(t)
	var mode string
	var sync int
	if err := db.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	if err := db.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil || sync != 1 {
		t.Errorf("synchronous = %d, %v; want 1 (NORMAL)", sync, err)
	}
}

func TestPanicInTransactionRollsBackAndReleasesWriter(t *testing.T) {
	db, _ := open(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed")
			}
		}()
		db.InChannel(ctx, "a", func(tables storage.Tables) error {
			tables.Rocket.Save(sampleRocket("a"))
			panic("boom")
		})
	}()
	if _, found, _ := db.GetRocket(ctx, "a"); found {
		t.Fatal("write from the panicking transaction was kept")
	}

	// The single writer connection must be free again. Before the fix this
	// blocked forever; the timeout turns a hang into a failure.
	done := make(chan error, 1)
	go func() {
		done <- db.InChannel(ctx, "b", func(tables storage.Tables) error { return tables.Rocket.Save(sampleRocket("b")) })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer still blocked after a panic")
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	for _, p := range []model.Payload{
		model.Launched{RocketType: "Atlas", LaunchSpeed: 5, Mission: "M"},
		model.SpeedIncreased{By: 3},
		model.SpeedDecreased{By: 2},
		model.MissionChanged{NewMission: "N"},
		model.Exploded{Reason: "R"},
	} {
		body, err := encodePayload(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodePayload(p.Type(), body)
		if err != nil || got != p {
			t.Errorf("%s: stored as %s, read back %+v, %v", p.Type(), body, got, err)
		}
	}
	if body, _ := encodePayload(model.Launched{RocketType: "Atlas"}); !strings.Contains(body, `"type":"Atlas"`) {
		t.Errorf("stored body %s does not use the publisher's field names", body)
	}
}
