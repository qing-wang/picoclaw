package botlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/config"
)

// ---- test helpers -----------------------------------------------------------

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newTestServer builds a Server with one bot ("bot1"/"secret-token") plus any
// extraBots, wraps it in an httptest.Server, and returns both along with a
// cleanup func. The Server's own goroutines (Stop()) and the httptest server
// are both torn down by cleanup.
func newTestServer(t *testing.T, extraBots ...config.BotLinkBot) (*Server, *httptest.Server, func()) {
	t.Helper()
	bots := append([]config.BotLinkBot{
		{BotID: "bot1", TokenSHA256: tokenHash("secret-token"), Name: "Bot One"},
	}, extraBots...)

	srv := New(config.BotLinkConfig{Enabled: true, Bots: bots})
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handleWebSocket))

	cleanup := func() {
		httpSrv.Close()
		srv.Stop()
	}
	return srv, httpSrv, cleanup
}

// dialBot connects to httpSrv as botID with the given bearer token, returning
// the client-side *websocket.Conn (nil on failure, with the dial error).
func dialBot(httpSrv *httptest.Server, botID, token string) (*websocket.Conn, *http.Response, error) {
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	if botID != "" {
		header.Set("X-Bot-Id", botID)
	}
	return websocket.DefaultDialer.Dial(wsURL, header)
}

// mustDialBot dials and fails the test immediately on error.
func mustDialBot(t *testing.T, httpSrv *httptest.Server, botID, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := dialBot(httpSrv, botID, token)
	if err != nil {
		status := "n/a"
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dial bot %q failed: %v (status=%s)", botID, err, status)
	}
	return conn
}

// readWireMessage reads and decodes exactly one message from conn, or fails
// the test after timeout.
func readWireMessage(t *testing.T, conn *websocket.Conn, timeout time.Duration) wireMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var msg wireMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read message: %v", err)
	}
	return msg
}

// ---- authentication ----------------------------------------------------------

func TestAuth_ValidToken_Connects(t *testing.T) {
	_, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	conn, resp, err := dialBot(httpSrv, "bot1", "secret-token")
	if err != nil {
		t.Fatalf("expected successful upgrade, got error: %v (status=%v)", err, resp)
	}
	defer conn.Close()
}

func TestAuth_WrongToken_Rejected(t *testing.T) {
	_, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	_, resp, err := dialBot(httpSrv, "bot1", "wrong-token")
	if err == nil {
		t.Fatal("expected dial to fail for wrong token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got resp=%v", resp)
	}
}

func TestAuth_MissingHeaders_Rejected(t *testing.T) {
	_, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	tests := []struct {
		name  string
		botID string
		token string
	}{
		{"missing both", "", ""},
		{"missing token", "bot1", ""},
		{"missing bot id", "", "secret-token"},
		{"unknown bot id", "no-such-bot", "secret-token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, resp, err := dialBot(httpSrv, tc.botID, tc.token)
			if err == nil {
				t.Fatal("expected dial to fail")
			}
			if resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got resp=%v", resp)
			}
		})
	}
}

// ---- command round trip / request_id correlation ----------------------------

// TestCommandRoundTrip verifies that SendCommand publishes a "command" message
// and, once the fake bot replies with a matching request_id, returns the
// reply payload as a JSON string (matching the MQTT transport's format so
// fanreflex's shared botReply parsing works unchanged).
func TestCommandRoundTrip(t *testing.T) {
	srv, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	botConn := mustDialBot(t, httpSrv, "bot1", "secret-token")
	defer botConn.Close()

	// Give the server a moment to register the connection in the hub.
	waitForConnected(t, srv.Sender(), "bot1")

	done := make(chan struct{})
	var replyText string
	var sendErr error
	go func() {
		defer close(done)
		replyText, sendErr = srv.Sender().SendCommand(context.Background(), "bot1", "GetAllSensorValues", 2*time.Second)
	}()

	cmd := readWireMessage(t, botConn, 2*time.Second)
	if cmd.Type != msgTypeCommand {
		t.Fatalf("expected command message, got type=%q", cmd.Type)
	}
	if cmd.Text != "GetAllSensorValues" {
		t.Fatalf("expected text=GetAllSensorValues, got %q", cmd.Text)
	}
	if cmd.RequestID == "" {
		t.Fatal("expected non-empty request_id")
	}

	reply := wireMessage{
		Type:      msgTypeReply,
		RequestID: cmd.RequestID,
		Payload:   json.RawMessage(`{"success":true,"result":"ok","data":[{"name":"VRM","value":45}]}`),
	}
	if err := botConn.WriteJSON(reply); err != nil {
		t.Fatalf("write reply: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SendCommand did not return in time")
	}

	if sendErr != nil {
		t.Fatalf("SendCommand error: %v", sendErr)
	}
	const want = `{"success":true,"result":"ok","data":[{"name":"VRM","value":45}]}`
	if replyText != want {
		t.Errorf("SendCommand reply = %q, want %q", replyText, want)
	}
}

// TestRequestIDCorrelation_Concurrent verifies that two concurrent SendCommand
// calls each receive their own reply and are never cross-matched, even when
// the fake bot replies out of send order.
func TestRequestIDCorrelation_Concurrent(t *testing.T) {
	srv, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	botConn := mustDialBot(t, httpSrv, "bot1", "secret-token")
	defer botConn.Close()
	waitForConnected(t, srv.Sender(), "bot1")

	type result struct {
		text string
		err  error
	}
	results := make(chan result, 2)

	go func() {
		text, err := srv.Sender().SendCommand(context.Background(), "bot1", "GetSensorValue A", 3*time.Second)
		results <- result{text, err}
	}()
	go func() {
		text, err := srv.Sender().SendCommand(context.Background(), "bot1", "GetSensorValue B", 3*time.Second)
		results <- result{text, err}
	}()

	// Read both inbound commands, remembering which request_id asked about
	// which sensor so we can reply with a payload that lets us verify
	// correlation (not just "a reply arrived").
	cmds := make(map[string]string) // request_id -> sensor letter
	for i := 0; i < 2; i++ {
		cmd := readWireMessage(t, botConn, 3*time.Second)
		if !strings.HasPrefix(cmd.Text, "GetSensorValue ") {
			t.Fatalf("unexpected command text %q", cmd.Text)
		}
		letter := strings.TrimPrefix(cmd.Text, "GetSensorValue ")
		cmds[cmd.RequestID] = letter
	}
	if len(cmds) != 2 {
		t.Fatalf("expected 2 distinct request_ids, got %d: %v", len(cmds), cmds)
	}

	// Reply in reverse arbitrary order to stress correlation.
	for reqID, letter := range cmds {
		reply := wireMessage{
			Type:      msgTypeReply,
			RequestID: reqID,
			Payload:   json.RawMessage(`{"success":true,"result":"` + letter + `","data":null}`),
		}
		if err := botConn.WriteJSON(reply); err != nil {
			t.Fatalf("write reply: %v", err)
		}
	}

	gotA, gotB := false, false
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("SendCommand error: %v", r.err)
			}
			switch {
			case strings.Contains(r.text, `"result":"A"`):
				gotA = true
			case strings.Contains(r.text, `"result":"B"`):
				gotB = true
			default:
				t.Fatalf("unexpected reply payload: %q", r.text)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for SendCommand results")
		}
	}
	if !gotA || !gotB {
		t.Errorf("expected both A and B replies, gotA=%v gotB=%v", gotA, gotB)
	}
}

// ---- timeout vs not-connected -------------------------------------------------

// TestTimeout_BotConnectedButSilent verifies that when a bot is connected but
// never replies, SendCommand returns an error after the wait duration that is
// NOT ErrBotNotConnected (the two must be distinguishable — see §5 of the
// task spec).
func TestTimeout_BotConnectedButSilent(t *testing.T) {
	srv, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	botConn := mustDialBot(t, httpSrv, "bot1", "secret-token")
	defer botConn.Close()
	waitForConnected(t, srv.Sender(), "bot1")

	start := time.Now()
	_, err := srv.Sender().SendCommand(context.Background(), "bot1", "GetAllSensorValues", 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if errors.Is(err, ErrBotNotConnected) {
		t.Errorf("timeout error must NOT be ErrBotNotConnected, got: %v", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("SendCommand returned too early (%v), expected to wait out the timeout", elapsed)
	}
}

// TestNotConnected_ImmediateError verifies that SendCommand/SendMessage
// return ErrBotNotConnected immediately (not after waiting out waitReply)
// when the target bot has no active connection.
func TestNotConnected_ImmediateError(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()

	start := time.Now()
	_, err := srv.Sender().SendCommand(context.Background(), "bot1", "Ping", 5*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBotNotConnected) {
		t.Fatalf("expected ErrBotNotConnected, got: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("SendCommand took %v — expected an immediate error, not waiting out the 5s timeout", elapsed)
	}

	if err := srv.Sender().SendMessage("bot1", "hello"); !errors.Is(err, ErrBotNotConnected) {
		t.Errorf("SendMessage: expected ErrBotNotConnected, got: %v", err)
	}

	if srv.Sender().IsTargetConnected("bot1") {
		t.Error("IsTargetConnected should be false when no bot is connected")
	}
	if srv.Sender().IsConnected() {
		t.Error("IsConnected should be false when no bot is connected")
	}
}

// ---- duplicate connection -----------------------------------------------------

// TestDuplicateConnection_OldClosed verifies that a second connection for the
// same bot_id replaces (and closes) the first, per §3: "同一 bot_id 重複連線
// → 新連線取代舊連線".
func TestDuplicateConnection_OldClosed(t *testing.T) {
	srv, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	first := mustDialBot(t, httpSrv, "bot1", "secret-token")
	defer first.Close()
	waitForConnected(t, srv.Sender(), "bot1")

	second := mustDialBot(t, httpSrv, "bot1", "secret-token")
	defer second.Close()

	// The first connection must observe a close (read error) shortly after
	// the second one takes over.
	_ = first.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := first.ReadMessage()
	if err == nil {
		t.Fatal("expected the first connection to be closed after a duplicate bot_id connected")
	}

	// The hub must now route to the second (surviving) connection.
	waitForConnected(t, srv.Sender(), "bot1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.Sender().SendCommand(context.Background(), "bot1", "Ping", 2*time.Second)
	}()
	cmd := readWireMessage(t, second, 2*time.Second)
	if cmd.Text != "Ping" {
		t.Errorf("expected the surviving (second) connection to receive the command, got %q", cmd.Text)
	}
	reply := wireMessage{Type: msgTypeReply, RequestID: cmd.RequestID, Payload: json.RawMessage(`{"success":true}`)}
	_ = second.WriteJSON(reply)
	<-done
}

// ---- concurrency --------------------------------------------------------------

// TestConcurrentWrites_NoPanic drives many concurrent SendCommand calls
// against one connection and asserts the process does not panic — gorilla/
// websocket forbids concurrent writers on one *Conn, so botConn.writeJSON's
// mutex must actually serialize every write. Run with -race to also catch
// data races.
func TestConcurrentWrites_NoPanic(t *testing.T) {
	srv, httpSrv, cleanup := newTestServer(t)
	defer cleanup()

	botConn := mustDialBot(t, httpSrv, "bot1", "secret-token")
	defer botConn.Close()
	waitForConnected(t, srv.Sender(), "bot1")

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)

	// Fake bot: read every inbound command and reply immediately.
	replierDone := make(chan struct{})
	go func() {
		defer close(replierDone)
		for i := 0; i < n; i++ {
			_ = botConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var msg wireMessage
			if err := botConn.ReadJSON(&msg); err != nil {
				return
			}
			reply := wireMessage{Type: msgTypeReply, RequestID: msg.RequestID, Payload: json.RawMessage(`{"success":true}`)}
			_ = botConn.WriteJSON(reply)
		}
	}()

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = srv.Sender().SendCommand(context.Background(), "bot1", "Ping", 5*time.Second)
		}()
	}

	wg.Wait()
	select {
	case <-replierDone:
	case <-time.After(5 * time.Second):
		t.Fatal("fake bot replier did not finish in time")
	}
}

// ---- helpers -------------------------------------------------------------------

// waitForConnected polls until sender reports botID connected, or fails the
// test after a short deadline. This avoids a race between the client-side
// Dial() returning and the server-side hub registration goroutine running.
func waitForConnected(t *testing.T, sender *HubSender, botID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sender.IsTargetConnected(botID) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("bot %q never became connected", botID)
}
