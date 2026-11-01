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
	"strings"
	"sync"
	"testing"
	"time"
)

type scenario struct {
	name     string
	goal     string
	messages int     // passed as --max-messages when not the default
	failRate float64 // share of requests the proxy fails
}

const defaultMessages = 100000 // the rockets program's own default

func TestDefaults(t *testing.T) {
	runScenario(t, scenario{
		name:     "defaults",
		goal:     "exactly how the graders run it: no flags besides the URL",
		messages: defaultMessages,
	})
}

func TestInjectedFailures(t *testing.T) {
	// Half the failures are dropped before the server sees them (gaps,
	// reordering); half reach the server but lose the reply (true
	// at-least-once duplicates). The default run produces almost none of this.
	runScenario(t, scenario{
		name:     "injected failures",
		goal:     "10% of requests fail and are redelivered 500 ms later: real duplicates and reordering",
		messages: 20000,
		failRate: 0.10,
	})
}

func runScenario(t *testing.T, sc scenario) {
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

	t.Logf("[1/5] scenario %q: %s", sc.name, sc.goal)
	t.Logf("      settings: %d messages, concurrency 3 (program default), injected failure rate %.0f%%",
		sc.messages, sc.failRate*100)

	t.Logf("[2/5] starting the server")
	serverURL := startServer(t, serverBin)
	proxy := &recorder{backend: serverURL, failRate: sc.failRate, rng: rand.New(rand.NewPCG(7, 0))}
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	logPath := filepath.Join(t.TempDir(), "rockets.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	args := []string{"launch", front.URL + "/messages"}
	if sc.messages != defaultMessages {
		args = append(args, fmt.Sprintf("--max-messages=%d", sc.messages))
	}
	cmd := exec.Command(rocketsBin, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile

	t.Logf("[3/5] publishing: rockets %s (via the recording proxy)", strings.Join(args, " "))
	start := time.Now()
	if err := cmd.Run(); err != nil {
		// The log lives in t.TempDir, which is deleted after the test: show
		// its tail now.
		out, _ := os.ReadFile(logPath)
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		t.Fatalf("rockets exited with %v; last log lines:\n%s", err, strings.Join(lines[max(0, len(lines)-20):], "\n"))
	}
	t.Logf("      finished in %s", time.Since(start).Round(time.Millisecond))
	t.Logf("      requests from rockets:   %d", proxy.requests)
	t.Logf("      injected failures:       %d (%d dropped before the server, %d processed but reply lost)",
		proxy.dropped+proxy.replyLost, proxy.dropped, proxy.replyLost)
	t.Logf("      forwarded to the server: %d", proxy.forwarded)
	t.Logf("      server replies:          applied %d, buffered %d, duplicate %d, other %d",
		proxy.results["applied"], proxy.results["buffered"], proxy.results["duplicate"],
		proxy.forwarded-proxy.results["applied"]-proxy.results["buffered"]-proxy.results["duplicate"])

	want, ref := reference(t, proxy.accepted)
	t.Logf("[4/5] reference: folded %d unique messages (%d accepted copies) over %d channels",
		ref.unique, len(proxy.accepted), ref.channels)
	if lost := sc.messages - ref.unique; lost > 0 {
		t.Logf("      %d of %d published messages never reached the server: rockets drops retries still pending when it exits",
			lost, sc.messages)
	}
	t.Logf("      expects %d launched rockets (%d exploded), %d messages still pending behind gaps",
		ref.launched, ref.exploded, ref.pending)

	t.Logf("[5/5] comparing the server with the reference")
	rocketMismatches := checkRockets(t, serverURL, want)
	debugMismatches := checkDebug(t, serverURL, want)

	verdict := "PASS"
	if t.Failed() {
		verdict = "FAIL"
	}
	t.Logf("SUMMARY %q: %s | %d messages published, %d requests, %d duplicates seen by the server, "+
		"%d rockets and %d channels compared, %d mismatches",
		sc.name, verdict, sc.messages, proxy.requests, proxy.results["duplicate"],
		ref.launched, ref.channels, rocketMismatches+debugMismatches)
}

// TestOutOfOrderAndDuplicates sends a short sequence by hand, because the
// real program almost never reorders or duplicates: messages 3 and 2 before
// the launch, then the launch, then a resent message 2.
func TestOutOfOrderAndDuplicates(t *testing.T) {
	serverBin := os.Getenv("SERVER_BIN")
	if serverBin == "" {
		serverBin = "../bin/server"
	}
	t.Logf("[1/4] scenario \"out of order and duplicates\": messages sent by hand to one rocket")
	url := startServer(t, serverBin)

	post := func(n int, msgType, body string) string {
		t.Helper()
		msg := fmt.Sprintf(`{"metadata":{"channel":"demo","messageNumber":%d,"messageTime":"2026-11-01T10:00:0%dZ","messageType":%q},"message":%s}`,
			n, n, msgType, body)
		resp, err := http.Post(url+"/messages", "application/json", strings.NewReader(msg))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res struct{ Result string }
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST message %d: %s, %v", n, resp.Status, err)
		}
		return res.Result
	}
	expect := func(what, got, want string) {
		t.Helper()
		status := "ok"
		if got != want {
			status = "WRONG"
			t.Errorf("%s: got %s, want %s", what, got, want)
		}
		t.Logf("      %-55s → %-22s %s", what, got, status)
	}
	rocket := func() (status int, speed int64, last int) {
		t.Helper()
		resp, err := http.Get(url + "/rockets/demo")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r struct {
			Speed             int64
			LastMessageNumber int
		}
		json.NewDecoder(resp.Body).Decode(&r)
		return resp.StatusCode, r.Speed, r.LastMessageNumber
	}
	pending := func() string {
		t.Helper()
		var infos []struct {
			Channel        string
			PendingNumbers []int
		}
		getJSON(t, url+"/debug/store", &infos)
		for _, ci := range infos {
			if ci.Channel == "demo" {
				return fmt.Sprint(ci.PendingNumbers)
			}
		}
		return "[]"
	}

	t.Logf("[2/4] messages 3 and 2 arrive before the launch (message 1)")
	expect("POST message 3 (speed +100)", post(3, "RocketSpeedIncreased", `{"by":100}`), "buffered")
	expect("POST message 2 (speed +50)", post(2, "RocketSpeedIncreased", `{"by":50}`), "buffered")
	code, _, _ := rocket()
	expect("GET /rockets/demo before launch", strconv.Itoa(code), "404")
	expect("pending in /debug/store", pending(), "[2 3]")

	t.Logf("[3/4] the launch arrives: 1, 2 and 3 are applied in one go")
	expect("POST message 1 (launch, speed 500)", post(1, "RocketLaunched", `{"type":"Falcon-9","launchSpeed":500,"mission":"ARTEMIS"}`), "applied")
	code, speed, last := rocket()
	expect("GET /rockets/demo", strconv.Itoa(code), "200")
	expect("speed (500 + 50 + 100)", strconv.FormatInt(speed, 10), "650")
	expect("lastMessageNumber", strconv.Itoa(last), "3")
	expect("pending in /debug/store", pending(), "[]")

	t.Logf("[4/4] message 2 is delivered again")
	expect("POST message 2 again", post(2, "RocketSpeedIncreased", `{"by":50}`), "duplicate")
	_, speed, last = rocket()
	expect("speed unchanged", strconv.FormatInt(speed, 10), "650")
	expect("lastMessageNumber unchanged", strconv.Itoa(last), "3")

	verdict := "PASS"
	if t.Failed() {
		verdict = "FAIL"
	}
	t.Logf("SUMMARY \"out of order and duplicates\": %s | checked: 2 early messages buffered, applied after the launch; 1 resent message ignored", verdict)
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
	t.Logf("      server: started %s on %s with a fresh database", bin, addr)
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

	mu        sync.Mutex
	rng       *rand.Rand
	requests  int            // received from rockets
	dropped   int            // injected: never reached the server
	replyLost int            // injected: reached the server, reply replaced by a 500
	forwarded int            // reached the server
	results   map[string]int // server's "result" field → count
	accepted  [][]byte       // bodies the server answered 200 to
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
	switch {
	case loseReply:
		p.replyLost++
	case fail:
		p.dropped++
	}
	if !fail || loseReply {
		p.forwarded++
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

type refStats struct {
	unique, channels, launched, exploded, pending int
}

// reference folds the accepted messages of each channel in number order,
// stopping at the first gap. Messages after a gap must still be pending.
func reference(t *testing.T, bodies [][]byte) (map[string]expected, refStats) {
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

	st := refStats{channels: len(out)}
	for ch, e := range out {
		st.unique += len(byChannel[ch])
		st.pending += e.Pending
		if e.Launched {
			st.launched++
		}
		if e.Status == "EXPLODED" {
			st.exploded++
		}
	}
	return out, st
}

// --- checks ---------------------------------------------------------------

func checkRockets(t *testing.T, url string, want map[string]expected) (mismatches int) {
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
		mismatches++
		t.Errorf("GET /rockets returned %d rockets, reference has %d launched", len(got), launched)
	}
	exploded := 0
	for _, g := range got {
		w, ok := want[g.ID]
		switch {
		case !ok || !w.Launched:
			mismatches++
			t.Errorf("%s: listed but not launched in reference", g.ID)
		case g.Type != w.Type || g.Speed != w.Speed || g.Mission != w.Mission || g.Status != w.Status ||
			fmt.Sprint(deref(g.ExplosionReason)) != fmt.Sprint(deref(w.ExplosionReason)) ||
			g.LastMessageNumber != w.LastMessageNumber:
			mismatches++
			t.Errorf("%s mismatch:\n got  %+v reason=%v\n want %+v reason=%v", g.ID, g, deref(g.ExplosionReason), w, deref(w.ExplosionReason))
		}
		if g.Status == "EXPLODED" {
			exploded++
		}
	}
	t.Logf("      GET /rockets: %d rockets (%d exploded) vs %d expected; compared type, speed, mission, status, "+
		"explosionReason, lastMessageNumber (%d fields): %d mismatches", len(got), exploded, launched, len(got)*6, mismatches)
	return mismatches
}

func checkDebug(t *testing.T, url string, want map[string]expected) (mismatches int) {
	t.Helper()
	var got []struct {
		Channel           string
		LastMessageNumber int
		PendingCount      int
	}
	getJSON(t, url+"/debug/store", &got)
	if len(got) != len(want) {
		mismatches++
		t.Errorf("debug lists %d channels, reference has %d", len(got), len(want))
	}
	pending := 0
	for _, g := range got {
		w := want[g.Channel]
		if g.LastMessageNumber != w.LastMessageNumber || g.PendingCount != w.Pending {
			mismatches++
			t.Errorf("%s: lastMessageNumber=%d pending=%d, want %d/%d", g.Channel, g.LastMessageNumber, g.PendingCount, w.LastMessageNumber, w.Pending)
		}
		pending += g.PendingCount
	}
	t.Logf("      GET /debug/store: %d channels vs %d expected; compared lastMessageNumber and pending count "+
		"(%d still pending behind gaps): %d mismatches", len(got), len(want), pending, mismatches)
	return mismatches
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
