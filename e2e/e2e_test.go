//go:build e2e

// Package e2e runs the real rockets test program against the real server
// binary and checks the final state against an independent reference.
//
//	rockets ──POST──▶ recording proxy ──▶ server binary
//	                  (optionally fails
//	                   some requests)
//
// The proxy records every message the server accepted. The reference is a
// naive fold of those messages written here from scratch (it shares no code
// with the service), compared with GET /rockets and GET /debug/store.
//
// Run with: make e2e   (needs ROCKETS_BIN, see Makefile)
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	// Exactly how the graders run it: no flags besides the URL.
	runScenario(t, 0, nil)
}

func TestInjectedFailures(t *testing.T) {
	// 10% of requests fail, so the real program redelivers them 500 ms later.
	// Half are dropped before the server sees them (reordering, gaps); half
	// reach the server but lose the reply (true at-least-once duplicates).
	// The default run produces almost none of this.
	runScenario(t, 0.10, []string{"--max-messages=20000"})
}

func runScenario(t *testing.T, failRate float64, extraFlags []string) {
	rocketsBin := os.Getenv("ROCKETS_BIN")
	if rocketsBin == "" {
		rocketsBin = defaultRocketsBin()
	}
	if _, err := os.Stat(rocketsBin); err != nil {
		t.Fatalf("rockets binary not found at %q; set ROCKETS_BIN (%v)", rocketsBin, err)
	}
	serverBin := os.Getenv("SERVER_BIN")
	if serverBin == "" {
		serverBin = "../bin/server"
	}

	serverURL := startServer(t, serverBin)
	proxy := &recorder{backend: serverURL, failRate: failRate, rng: rand.New(rand.NewPCG(7, 0))}
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	logPath := filepath.Join(t.TempDir(), "rockets.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	args := append([]string{"launch", front.URL + "/messages"}, extraFlags...)
	cmd := exec.Command(rocketsBin, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile

	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("rockets: %v (log: %s)", err, logPath)
	}
	t.Logf("rockets finished in %s: %d requests, %d injected failures, server results %v",
		time.Since(start).Round(time.Millisecond), proxy.requests, proxy.injected, proxy.results)

	want := reference(t, proxy.accepted)
	checkRockets(t, serverURL, want)
	checkDebug(t, serverURL, want)
}

// --- server process -------------------------------------------------------

func startServer(t *testing.T, bin string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	dbPath := filepath.Join(t.TempDir(), "e2e.db")
	cmd := exec.Command(bin, "-addr", addr, "-db", dbPath)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v (run `make build` first)", bin, err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})

	url := "http://" + addr
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if resp, err := http.Get(url + "/rockets"); err == nil {
			resp.Body.Close()
			return url
		}
	}
	t.Fatalf("server did not become healthy; logs:\n%s", logs.String())
	return ""
}

func defaultRocketsBin() string {
	home, _ := os.UserHomeDir()
	name := "rockets"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, "Downloads", "lunar-backend-engineer-challenge-ai", runtime.GOOS+"_"+runtime.GOARCH, name)
}

// --- recording proxy ------------------------------------------------------

type recorder struct {
	backend  string
	failRate float64

	mu       sync.Mutex
	rng      *rand.Rand
	requests int
	injected int
	results  map[string]int // server's "result" field → count
	accepted [][]byte       // bodies the server answered 200 to
}

func (p *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	p.requests++
	fail := p.rng.Float64() < p.failRate
	loseReply := fail && p.rng.IntN(2) == 0 // else: drop the request
	if fail {
		p.injected++
	}
	p.mu.Unlock()
	if fail && !loseReply {
		http.Error(w, "injected failure: request dropped", http.StatusInternalServerError)
		return
	}

	resp, err := http.Post(p.backend+r.URL.Path, "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var res struct{ Result string }
	json.Unmarshal(respBody, &res)
	p.mu.Lock()
	if p.results == nil {
		p.results = map[string]int{}
	}
	p.results[cmpOr(res.Result, strconv.Itoa(resp.StatusCode))]++
	if resp.StatusCode == http.StatusOK {
		p.accepted = append(p.accepted, body)
	}
	p.mu.Unlock()

	if loseReply {
		http.Error(w, "injected failure: reply lost", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- independent reference ------------------------------------------------

type wireMsg struct {
	Metadata struct {
		Channel       string `json:"channel"`
		MessageNumber int    `json:"messageNumber"`
		MessageType   string `json:"messageType"`
	} `json:"metadata"`
	Message struct {
		Type        string `json:"type"`
		LaunchSpeed int64  `json:"launchSpeed"`
		Mission     string `json:"mission"`
		By          int64  `json:"by"`
		NewMission  string `json:"newMission"`
		Reason      string `json:"reason"`
	} `json:"message"`
}

type expected struct {
	Type, Mission, Status string
	Speed                 int64
	ExplosionReason       *string
	LastMessageNumber     int
	Launched              bool
	Pending               int
}

// reference folds the accepted messages of each channel in number order,
// stopping at the first gap. Messages after a gap must still be pending.
func reference(t *testing.T, bodies [][]byte) map[string]expected {
	t.Helper()
	byChannel := map[string]map[int]wireMsg{}
	for _, b := range bodies {
		var m wireMsg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("captured body: %v", err)
		}
		ch := m.Metadata.Channel
		if byChannel[ch] == nil {
			byChannel[ch] = map[int]wireMsg{}
		}
		if _, dup := byChannel[ch][m.Metadata.MessageNumber]; !dup {
			byChannel[ch][m.Metadata.MessageNumber] = m
		}
	}

	out := map[string]expected{}
	for ch, msgs := range byChannel {
		var e expected
		n := 1
		for ; ; n++ {
			m, ok := msgs[n]
			if !ok {
				break
			}
			switch b := m.Message; m.Metadata.MessageType {
			case "RocketLaunched":
				e.Type, e.Speed, e.Mission, e.Status, e.Launched = b.Type, b.LaunchSpeed, b.Mission, "ACTIVE", true
			case "RocketSpeedIncreased":
				if e.Status == "ACTIVE" {
					e.Speed += b.By
				}
			case "RocketSpeedDecreased":
				if e.Status == "ACTIVE" {
					e.Speed -= b.By
				}
			case "RocketMissionChanged":
				if e.Status == "ACTIVE" {
					e.Mission = b.NewMission
				}
			case "RocketExploded":
				if e.Status == "ACTIVE" {
					reason := b.Reason
					e.Status, e.ExplosionReason = "EXPLODED", &reason
				}
			}
		}
		e.LastMessageNumber = n - 1
		e.Pending = len(msgs) - e.LastMessageNumber
		out[ch] = e
	}
	return out
}

// --- checks ---------------------------------------------------------------

func checkRockets(t *testing.T, url string, want map[string]expected) {
	t.Helper()
	var got []struct {
		ID, Type, Mission, Status string
		Speed                     int64
		ExplosionReason           *string
		LastMessageNumber         int
	}
	getJSON(t, url+"/rockets", &got)

	launched := 0
	for _, w := range want {
		if w.Launched {
			launched++
		}
	}
	if len(got) != launched {
		t.Errorf("GET /rockets returned %d rockets, reference has %d launched", len(got), launched)
	}
	exploded := 0
	for _, g := range got {
		w, ok := want[g.ID]
		switch {
		case !ok || !w.Launched:
			t.Errorf("%s: listed but not launched in reference", g.ID)
		case g.Type != w.Type || g.Speed != w.Speed || g.Mission != w.Mission || g.Status != w.Status ||
			fmt.Sprint(deref(g.ExplosionReason)) != fmt.Sprint(deref(w.ExplosionReason)) ||
			g.LastMessageNumber != w.LastMessageNumber:
			t.Errorf("%s mismatch:\n got  %+v reason=%v\n want %+v reason=%v", g.ID, g, deref(g.ExplosionReason), w, deref(w.ExplosionReason))
		}
		if g.Status == "EXPLODED" {
			exploded++
		}
	}
	t.Logf("verified %d rockets (%d exploded) field by field", len(got), exploded)
}

func checkDebug(t *testing.T, url string, want map[string]expected) {
	t.Helper()
	var got []struct {
		Channel           string
		LastMessageNumber int
		PendingCount      int
	}
	getJSON(t, url+"/debug/store", &got)
	if len(got) != len(want) {
		t.Errorf("debug lists %d channels, reference has %d", len(got), len(want))
	}
	pending := 0
	for _, g := range got {
		w := want[g.Channel]
		if g.LastMessageNumber != w.LastMessageNumber || g.PendingCount != w.Pending {
			t.Errorf("%s: lastMessageNumber=%d pending=%d, want %d/%d", g.Channel, g.LastMessageNumber, g.PendingCount, w.LastMessageNumber, w.Pending)
		}
		pending += g.PendingCount
	}
	t.Logf("verified watermark and buffer of %d channels (%d messages still pending behind gaps)", len(got), pending)
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
