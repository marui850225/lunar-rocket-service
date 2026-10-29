package store

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"rockets-service/internal/rocket"
)

var (
	quiet = slog.New(slog.NewTextHandler(io.Discard, nil))
	t0    = time.Date(2022, 2, 2, 19, 39, 5, 0, time.UTC)
)

func mk(channel string, n int, p rocket.Payload) rocket.Message {
	return rocket.Message{Channel: channel, Number: n, Time: t0.Add(time.Duration(n) * time.Second), Payload: p}
}

func launch(channel string) rocket.Message {
	return mk(channel, 1, rocket.Launched{RocketType: "Falcon-9", LaunchSpeed: 500, Mission: "ARTEMIS"})
}

func up(channel string, n int, by int64) rocket.Message {
	return mk(channel, n, rocket.SpeedIncreased{By: by})
}

func mustIngest(t *testing.T, s *Store, m rocket.Message) Result {
	t.Helper()
	res, err := s.Ingest(m)
	if err != nil {
		t.Fatalf("Ingest(#%d): %v", m.Number, err)
	}
	return res
}

func TestIngestOrderingExample(t *testing.T) {
	// The worked example from the design doc: arrivals 1, 3, 2, 2, 5, 4.
	s := New(quiet, 0)
	steps := []struct {
		msg         rocket.Message
		want        Result
		wantApplied int
		wantPending int
	}{
		{launch("a"), Applied, 1, 0},
		{up("a", 3, 10), Buffered, 1, 1},
		{up("a", 2, 10), Applied, 3, 0},
		{up("a", 2, 10), Duplicate, 3, 0},
		{up("a", 5, 10), Buffered, 3, 1},
		{up("a", 4, 10), Applied, 5, 0},
	}
	for i, st := range steps {
		if got := mustIngest(t, s, st.msg); got != st.want {
			t.Fatalf("step %d (#%d): result %v, want %v", i, st.msg.Number, got, st.want)
		}
		e := s.rockets["a"]
		if e.applied != st.wantApplied || len(e.pending) != st.wantPending {
			t.Fatalf("step %d: applied=%d pending=%d, want %d/%d", i, e.applied, len(e.pending), st.wantApplied, st.wantPending)
		}
	}
	r, ok := s.Get("a")
	if !ok || r.Speed != 540 || r.LastMessageNumber != 5 {
		t.Fatalf("Get = %+v, %v", r, ok)
	}
}

func TestDuplicateOfPendingIgnoresBody(t *testing.T) {
	s := New(quiet, 0)
	mustIngest(t, s, up("a", 2, 100))
	if got := mustIngest(t, s, up("a", 2, 999)); got != Duplicate {
		t.Fatalf("result %v, want Duplicate", got)
	}
	mustIngest(t, s, launch("a"))
	if r, _ := s.Get("a"); r.Speed != 600 {
		t.Fatalf("speed %d, want 600 (first copy wins)", r.Speed)
	}
}

func TestHiddenUntilLaunched(t *testing.T) {
	s := New(quiet, 0)
	mustIngest(t, s, up("a", 2, 100))
	mustIngest(t, s, up("a", 3, 100))
	if _, ok := s.Get("a"); ok {
		t.Fatal("rocket visible before launch")
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("List = %v, want empty", got)
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("unknown rocket reported found")
	}

	mustIngest(t, s, launch("a"))
	r, ok := s.Get("a")
	if !ok || r.Speed != 700 {
		t.Fatalf("after launch: %+v, %v", r, ok)
	}
	if got := s.List(); len(got) != 1 {
		t.Fatalf("List len %d, want 1", len(got))
	}
}

func TestBufferFull(t *testing.T) {
	s := New(quiet, 2)
	mustIngest(t, s, up("a", 3, 1))
	mustIngest(t, s, up("a", 4, 1))

	if _, err := s.Ingest(up("a", 5, 1)); !errors.Is(err, ErrBufferFull) {
		t.Fatalf("err = %v, want ErrBufferFull", err)
	}
	// A duplicate of a pending message is still fine when full.
	if got := mustIngest(t, s, up("a", 3, 1)); got != Duplicate {
		t.Fatalf("result %v, want Duplicate", got)
	}
	// The gap-filling message is accepted even when full, and drains it.
	if got := mustIngest(t, s, launch("a")); got != Applied {
		t.Fatalf("result %v, want Applied", got)
	}
	mustIngest(t, s, up("a", 2, 1))
	// The rejected #5 can now be redelivered.
	mustIngest(t, s, up("a", 5, 1))
	if r, _ := s.Get("a"); r.LastMessageNumber != 5 || r.Speed != 504 {
		t.Fatalf("state %+v", r)
	}
}

// genStream returns a valid in-order stream of n messages for one channel.
func genStream(rng *rand.Rand, channel string, n int) []rocket.Message {
	msgs := []rocket.Message{mk(channel, 1, rocket.Launched{
		RocketType: fmt.Sprintf("type-%d", rng.IntN(5)), LaunchSpeed: rng.Int64N(1000), Mission: "M0"})}
	for i := 2; i <= n; i++ {
		var p rocket.Payload
		switch x := rng.IntN(100); {
		case i == n && x < 30:
			p = rocket.Exploded{Reason: "BOOM"}
		case x < 35:
			p = rocket.SpeedIncreased{By: rng.Int64N(5000)}
		case x < 90:
			p = rocket.SpeedDecreased{By: rng.Int64N(5000)}
		default:
			p = rocket.MissionChanged{NewMission: fmt.Sprintf("M%d", i)}
		}
		msgs = append(msgs, mk(channel, i, p))
	}
	return msgs
}

// foldInOrder is the oracle: deliberately naive, sharing no ordering code with Store.
func foldInOrder(msgs []rocket.Message) rocket.Rocket {
	var r rocket.Rocket
	for _, m := range msgs {
		r, _ = rocket.Apply(r, m)
	}
	return r
}

// mess shuffles msgs and delivers each 1 to 3 times.
func mess(rng *rand.Rand, msgs []rocket.Message) []rocket.Message {
	var out []rocket.Message
	for _, m := range msgs {
		for range 1 + rng.IntN(3) {
			out = append(out, m)
		}
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func TestAnyOrderGivesSameResult(t *testing.T) {
	for seed := range uint64(1000) {
		rng := rand.New(rand.NewPCG(seed, 0))
		stream := genStream(rng, "a", 200)
		want := foldInOrder(stream)

		s := New(quiet, 0)
		for _, m := range mess(rng, stream) {
			if _, err := s.Ingest(m); err != nil {
				t.Fatalf("seed %d: Ingest: %v", seed, err)
			}
		}
		got, ok := s.Get("a")
		if !ok || got != want {
			t.Fatalf("seed %d:\n got  %+v\n want %+v", seed, got, want)
		}
		if n := len(s.rockets["a"].pending); n != 0 {
			t.Fatalf("seed %d: %d messages left pending", seed, n)
		}
	}
}

func TestConcurrentIngest(t *testing.T) {
	const rockets, perRocket, workers = 8, 300, 16
	rng := rand.New(rand.NewPCG(42, 0))

	want := map[string]rocket.Rocket{}
	var all []rocket.Message
	for i := range rockets {
		ch := fmt.Sprintf("rocket-%d", i)
		stream := genStream(rng, ch, perRocket)
		want[ch] = foldInOrder(stream)
		all = append(all, stream...)
	}
	all = mess(rng, all)

	s := New(quiet, 0)
	work := make(chan rocket.Message)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for m := range work {
				if _, err := s.Ingest(m); err != nil {
					t.Errorf("Ingest: %v", err)
				}
			}
		})
	}
	// Readers run alongside writers so -race checks Get/List too.
	stop := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					s.List()
					s.Get("rocket-0")
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
		if got, ok := s.Get(ch); !ok || got != w {
			t.Errorf("%s:\n got  %+v\n want %+v", ch, got, w)
		}
	}
	if got := len(s.List()); got != rockets {
		t.Errorf("List len %d, want %d", got, rockets)
	}
}
