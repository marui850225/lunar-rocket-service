package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"rockets-service/internal/model"
	"rockets-service/internal/storage"
	"rockets-service/internal/storage/sqlite"
)

var (
	ctx   = context.Background()
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
	t0    = time.Date(2022, 2, 2, 19, 39, 5, 0, time.UTC)
)

func newService(t testing.TB) (*Service, storage.Storage) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, quiet), db
}

func mk(channel string, n int, p model.Payload) model.Message {
	return model.Message{Channel: channel, Number: n, Time: t0.Add(time.Duration(n) * time.Second), Payload: p}
}

func launch(channel string) model.Message {
	return mk(channel, 1, model.Launched{RocketType: "Falcon-9", LaunchSpeed: 500, Mission: "ARTEMIS"})
}

func up(channel string, n int, by int64) model.Message {
	return mk(channel, n, model.SpeedIncreased{By: by})
}

func mustIngest(t testing.TB, s *Service, m model.Message) Result {
	t.Helper()
	res, err := s.ingest(ctx, m)
	if err != nil {
		t.Fatalf("ingest(#%d): %v", m.Number, err)
	}
	return res
}

// bookkeeping returns a channel's watermark and buffered numbers.
func bookkeeping(t testing.TB, db storage.Storage, channel string) (last int, pending []int) {
	t.Helper()
	infos, err := db.Channels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range infos {
		if ci.Channel == channel {
			return ci.LastMessageNumber, ci.PendingNumbers
		}
	}
	return 0, nil
}

func TestIngestOrderingExample(t *testing.T) {
	// Arrivals 1, 7, 5, 4, 3, 2: message 2 applies 2..5, and 7 waits for 6.
	s, db := newService(t)
	steps := []struct {
		msg         model.Message
		want        Result
		wantLast    int
		wantPending string
	}{
		{launch("a"), Applied, 1, "[]"},
		{up("a", 7, 10), Buffered, 1, "[7]"},
		{up("a", 5, 10), Buffered, 1, "[5 7]"},
		{up("a", 4, 10), Buffered, 1, "[4 5 7]"},
		{up("a", 3, 10), Buffered, 1, "[3 4 5 7]"},
		{up("a", 2, 10), Applied, 5, "[7]"},
		{up("a", 4, 10), Duplicate, 5, "[7]"}, // already processed
		{up("a", 7, 10), Duplicate, 5, "[7]"}, // already buffered
		{up("a", 6, 10), Applied, 7, "[]"},
	}
	for i, st := range steps {
		if got := mustIngest(t, s, st.msg); got != st.want {
			t.Fatalf("step %d (#%d): result %v, want %v", i, st.msg.Number, got, st.want)
		}
		last, pending := bookkeeping(t, db, "a")
		if last != st.wantLast || fmt.Sprint(pending) != st.wantPending {
			t.Fatalf("step %d: last=%d pending=%v, want %d %s", i, last, pending, st.wantLast, st.wantPending)
		}
	}
	r, err := s.GetRocket(ctx, "a")
	if err != nil || r.Speed != 560 || r.LastMessageNumber != 7 {
		t.Fatalf("GetRocket = %+v, %v", r, err)
	}
}

func TestDuplicateOfPendingKeepsFirstCopy(t *testing.T) {
	s, _ := newService(t)
	mustIngest(t, s, up("a", 2, 100))
	if got := mustIngest(t, s, up("a", 2, 999)); got != Duplicate {
		t.Fatalf("result %v, want Duplicate", got)
	}
	mustIngest(t, s, launch("a"))
	if r, _ := s.GetRocket(ctx, "a"); r.Speed != 600 {
		t.Fatalf("speed %d, want 600 (first copy wins)", r.Speed)
	}
}

func TestHiddenUntilLaunched(t *testing.T) {
	s, _ := newService(t)
	mustIngest(t, s, up("a", 2, 100))
	if _, err := s.GetRocket(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRocket before launch: %v, want ErrNotFound", err)
	}
	if got, _ := s.ListRockets(ctx, "", ""); len(got) != 0 {
		t.Fatalf("List before launch = %v", got)
	}
	if _, err := s.GetRocket(ctx, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRocket unknown: %v", err)
	}

	mustIngest(t, s, launch("a"))
	if r, err := s.GetRocket(ctx, "a"); err != nil || r.Speed != 600 {
		t.Fatalf("after launch: %+v, %v", r, err)
	}
}

func TestIngestRejectsInvalid(t *testing.T) {
	s, db := newService(t)
	if _, err := s.Ingest(ctx, []byte(`{nope`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if infos, _ := db.Channels(ctx); len(infos) != 0 {
		t.Fatalf("invalid message touched storage: %+v", infos)
	}
	res, err := s.Ingest(ctx, []byte(`{"metadata":{"channel":"a","messageNumber":1,"messageTime":"2022-02-02T19:39:05+01:00","messageType":"RocketLaunched"},"message":{"type":"Atlas","launchSpeed":5,"mission":"M"}}`))
	if err != nil || res != Applied {
		t.Fatalf("valid raw message: %v, %v", res, err)
	}
}

// genStream returns a valid in-order stream of n messages for one channel.
func genStream(rng *rand.Rand, channel string, n int) []model.Message {
	msgs := []model.Message{mk(channel, 1, model.Launched{
		RocketType: fmt.Sprintf("type-%d", rng.IntN(5)), LaunchSpeed: rng.Int64N(1000), Mission: "M0"})}
	for i := 2; i <= n; i++ {
		var p model.Payload
		switch x := rng.IntN(100); {
		case i == n && x < 30:
			p = model.Exploded{Reason: "BOOM"}
		case x < 35:
			p = model.SpeedIncreased{By: rng.Int64N(5000)}
		case x < 90:
			p = model.SpeedDecreased{By: rng.Int64N(5000)}
		default:
			p = model.MissionChanged{NewMission: fmt.Sprintf("M%d", i)}
		}
		msgs = append(msgs, mk(channel, i, p))
	}
	return msgs
}

// foldInOrder is the oracle: deliberately naive, sharing no ordering or
// storage code with the service.
func foldInOrder(msgs []model.Message) model.Rocket {
	var r model.Rocket
	for _, m := range msgs {
		r, _ = apply(r, m)
	}
	return r
}

// mess shuffles msgs and delivers each 1 to 3 times.
func mess(rng *rand.Rand, msgs []model.Message) []model.Message {
	var out []model.Message
	for _, m := range msgs {
		for range 1 + rng.IntN(3) {
			out = append(out, m)
		}
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func sameRocket(a, b model.Rocket) bool {
	return a.ID == b.ID && a.Type == b.Type && a.Speed == b.Speed && a.Mission == b.Mission &&
		a.Status == b.Status && a.ExplosionReason == b.ExplosionReason &&
		a.LaunchedAt.Equal(b.LaunchedAt) && a.UpdatedAt.Equal(b.UpdatedAt) &&
		a.LastMessageNumber == b.LastMessageNumber
}

func BenchmarkIngestInOrder(b *testing.B) {
	s, _ := newService(b)
	mustIngest(b, s, launch("a"))
	b.ResetTimer()
	for i := range b.N {
		mustIngest(b, s, up("a", i+2, 1))
	}
}

func TestAnyOrderGivesSameResult(t *testing.T) {
	seeds := uint64(100)
	if testing.Short() {
		seeds = 20
	}
	s, _ := newService(t)
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(seed, 0))
		ch := fmt.Sprintf("seed-%d", seed) // one channel per seed, one shared database
		stream := genStream(rng, ch, 100)
		want := foldInOrder(stream)

		for _, m := range mess(rng, stream) {
			if _, err := s.ingest(ctx, m); err != nil {
				t.Fatalf("seed %d: ingest: %v", seed, err)
			}
		}
		got, err := s.GetRocket(ctx, ch)
		if err != nil || !sameRocket(got, want) {
			t.Fatalf("seed %d: %v\n got  %+v\n want %+v", seed, err, got, want)
		}
	}
	infos, _ := s.Debug(ctx)
	for _, ci := range infos {
		if ci.PendingCount != 0 {
			t.Fatalf("%s: %d messages left pending", ci.Channel, ci.PendingCount)
		}
	}
}

func TestConcurrentIngest(t *testing.T) {
	const rockets, perRocket, workers = 8, 150, 16
	rng := rand.New(rand.NewPCG(42, 0))

	want := map[string]model.Rocket{}
	var all []model.Message
	for i := range rockets {
		ch := fmt.Sprintf("rocket-%d", i)
		stream := genStream(rng, ch, perRocket)
		want[ch] = foldInOrder(stream)
		all = append(all, stream...)
	}
	all = mess(rng, all)

	s, _ := newService(t)
	work := make(chan model.Message)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for m := range work {
				if _, err := s.ingest(ctx, m); err != nil {
					t.Errorf("ingest: %v", err)
				}
			}
		})
	}
	// Readers run alongside writers.
	stop := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := s.ListRockets(ctx, "speed", "desc"); err != nil {
						t.Errorf("list: %v", err)
					}
					s.GetRocket(ctx, "rocket-0")
				}
			}
		})
	}
	for _, m := range all {
		work <- m
	}
	close(work)
	close(stop)
	wg.Wait()

	for ch, w := range want {
		if got, err := s.GetRocket(ctx, ch); err != nil || !sameRocket(got, w) {
			t.Errorf("%s: %v\n got  %+v\n want %+v", ch, err, got, w)
		}
	}
	if got, _ := s.ListRockets(ctx, "", ""); len(got) != rockets {
		t.Errorf("List len %d, want %d", len(got), rockets)
	}
}

// BenchmarkIngestWithLargeBufferBehindGap: 9,000 messages wait behind a gap
// at 1001 while in-order messages keep arriving. Each in-order ingest must
// not read the buffered messages it cannot apply.
func BenchmarkIngestWithLargeBufferBehindGap(b *testing.B) {
	s, _ := newService(b)
	mustIngest(b, s, launch("a"))
	for n := 1002; n <= 10001; n++ {
		mustIngest(b, s, up("a", n, 1))
	}
	b.ResetTimer()
	for i := range b.N {
		mustIngest(b, s, up("a", i+2, 1)) // stays below the gap for b.N < 999
	}
}
