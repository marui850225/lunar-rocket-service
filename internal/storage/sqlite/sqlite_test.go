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
	db, err := Open(path, SyncNormal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func inChannel(t *testing.T, db *DB, ch string, fn func(storage.Tx) error) {
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
	inChannel(t, db, "a", func(tx storage.Tx) error { return tx.SaveRocket(want) })

	got, found, err := db.GetRocket(ctx, "a")
	if err != nil || !found || !sameRocket(got, want) {
		t.Fatalf("GetRocket = %+v, %v, %v; want %+v", got, found, err, want)
	}
	inChannel(t, db, "a", func(tx storage.Tx) error {
		got, err := tx.Rocket()
		if !sameRocket(got, want) {
			t.Errorf("tx.Rocket = %+v, %v", got, err)
		}
		return err
	})

	// Overwrite with fewer fields: empty strings and zero times become NULL and come back empty.
	inChannel(t, db, "a", func(tx storage.Tx) error {
		return tx.SaveRocket(model.Rocket{ID: "a", LastMessageNumber: 3})
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
	inChannel(t, db, "nope", func(tx storage.Tx) error {
		r, err := tx.Rocket()
		if r != (model.Rocket{}) {
			t.Errorf("tx.Rocket = %+v, want zero", r)
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
	inChannel(t, db, "a", func(tx storage.Tx) error {
		for _, m := range in {
			if added, err := tx.AddPending(m); !added || err != nil {
				t.Fatalf("AddPending #%d: %v %v", m.Number, added, err)
			}
		}
		if added, err := tx.AddPending(msg(3, model.SpeedDecreased{By: 999})); added || err != nil {
			t.Fatalf("duplicate AddPending: added=%v err=%v", added, err)
		}
		return nil
	})

	inChannel(t, db, "a", func(tx storage.Tx) error {
		for _, src := range in {
			g, found, err := tx.Pending(src.Number)
			if err != nil || !found {
				t.Fatalf("Pending(%d): found=%v err=%v", src.Number, found, err)
			}
			if g.Number != src.Number || g.Channel != "a" || g.Type() != src.Type() || g.Payload != src.Payload || !g.Time.Equal(src.Time) {
				t.Errorf("Pending(%d) = %+v, want %+v", src.Number, g, src)
			}
		}
		if g, _, _ := tx.Pending(3); g.Payload.(model.SpeedDecreased).By != 7 {
			t.Errorf("duplicate overwrote first copy: %+v", g.Payload)
		}
		if _, found, err := tx.Pending(99); found || err != nil {
			t.Errorf("Pending(99): found=%v err=%v", found, err)
		}

		if err := tx.DeletePending(2, 4); err != nil {
			return err
		}
		n, err := tx.CountPending()
		if n != 2 {
			t.Errorf("CountPending after deleting 2..4 = %d, want 2 (1 and 5 left)", n)
		}
		for _, k := range []int{1, 5} {
			if _, found, _ := tx.Pending(k); !found {
				t.Errorf("Pending(%d) deleted by DeletePending(2, 4)", k)
			}
		}
		return err
	})
}

func TestRollbackOnError(t *testing.T) {
	db, _ := open(t)
	boom := errors.New("boom")
	err := db.InChannel(ctx, "a", func(tx storage.Tx) error {
		tx.SaveRocket(sampleRocket("a"))
		tx.AddPending(msg(9, model.SpeedIncreased{By: 1}))
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
		inChannel(t, db, r.ID, func(tx storage.Tx) error { return tx.SaveRocket(r) })
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

func TestEmptyList(t *testing.T) {
	db, _ := open(t)
	got, err := db.ListRockets(ctx, storage.ListOptions{SortBy: storage.SortID})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %#v, %v; want empty non-nil slice", got, err)
	}
}

func TestChannels(t *testing.T) {
	db, _ := open(t)
	inChannel(t, db, "b", func(tx storage.Tx) error {
		return tx.SaveRocket(model.Rocket{ID: "b", Status: model.StatusActive, LastMessageNumber: 2})
	})
	inChannel(t, db, "b", func(tx storage.Tx) error {
		_, err := tx.AddPending(model.Message{Channel: "b", Number: 4, Time: time.Now(), Payload: model.Exploded{}})
		return err
	})
	inChannel(t, db, "a", func(tx storage.Tx) error { // buffered only, no rocket row
		_, err := tx.AddPending(model.Message{Channel: "a", Number: 3, Time: time.Now(), Payload: model.Exploded{}})
		return err
	})

	got, err := db.Channels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []storage.ChannelInfo{
		{Channel: "a", Launched: false, LastMessageNumber: 0, PendingCount: 1, PendingNumbers: []int{3}},
		{Channel: "b", Launched: true, LastMessageNumber: 2, PendingCount: 1, PendingNumbers: []int{4}},
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Channel != w.Channel || g.Launched != w.Launched || g.LastMessageNumber != w.LastMessageNumber ||
			g.PendingCount != w.PendingCount || len(g.PendingNumbers) != 1 || g.PendingNumbers[0] != w.PendingNumbers[0] {
			t.Errorf("channel %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestSurvivesReopen(t *testing.T) {
	db, path := open(t)
	inChannel(t, db, "a", func(tx storage.Tx) error {
		if err := tx.SaveRocket(sampleRocket("a")); err != nil {
			return err
		}
		_, err := tx.AddPending(msg(60, model.SpeedIncreased{By: 1}))
		return err
	})
	db.Close()

	db2, err := Open(path, SyncNormal)
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

func TestPragmasApplied(t *testing.T) {
	for mode, want := range map[Sync]int{SyncNormal: 1, SyncFull: 2} {
		db, err := Open(filepath.Join(t.TempDir(), "sync.db"), mode)
		if err != nil {
			t.Fatal(err)
		}
		var got int
		if err := db.writer.QueryRow(`PRAGMA synchronous`).Scan(&got); err != nil || got != want {
			t.Errorf("%s: synchronous = %d, %v; want %d", mode, got, err, want)
		}
		db.Close()
	}
	if _, err := Open(filepath.Join(t.TempDir(), "x.db"), "sometimes"); err == nil {
		t.Error("unknown sync mode accepted")
	}

	db, _ := open(t)
	var mode string
	var queryOnly int
	if err := db.writer.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	if err := db.reader.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Errorf("reader query_only = %d, %v; want 1", queryOnly, err)
	}
	if _, err := db.reader.Exec(`DELETE FROM rockets`); err == nil {
		t.Error("reader connection accepted a write")
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
		db.InChannel(ctx, "a", func(tx storage.Tx) error {
			tx.SaveRocket(sampleRocket("a"))
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
		done <- db.InChannel(ctx, "b", func(tx storage.Tx) error { return tx.SaveRocket(sampleRocket("b")) })
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

func TestStoredPayloadFormat(t *testing.T) {
	// The on-disk format is fixed: these strings must keep decoding even if
	// model field names change.
	good := map[model.MessageType]string{
		model.TypeLaunched:       `{"type":"Atlas","launchSpeed":5,"mission":"M"}`,
		model.TypeSpeedIncreased: `{"by":3}`,
		model.TypeSpeedDecreased: `{"by":0}`,
		model.TypeMissionChanged: `{"newMission":"N"}`,
		model.TypeExploded:       `{"reason":""}`,
	}
	for typ, body := range good {
		p, err := decodePayload(typ, body)
		if err != nil || p.Type() != typ {
			t.Errorf("%s %s: %v, %v", typ, body, p, err)
		}
		again, _ := encodePayload(p)
		if again != body {
			t.Errorf("%s: re-encoded as %s, want %s", typ, again, body)
		}
	}
	for _, bad := range []struct {
		typ  model.MessageType
		body string
	}{
		{model.TypeLaunched, `{"RocketType":"Atlas","launchSpeed":5,"mission":"M"}`}, // old Go field name
		{model.TypeLaunched, `{"type":"Atlas","mission":"M"}`},                       // missing field
		{model.TypeSpeedIncreased, `{}`},
		{model.TypeExploded, `{"reason":"x","extra":1}`},
	} {
		if _, err := decodePayload(bad.typ, bad.body); err == nil {
			t.Errorf("%s %s decoded without error", bad.typ, bad.body)
		}
	}
}
