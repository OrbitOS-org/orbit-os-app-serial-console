package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	orbitos "github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
)

// echoPort is a serial port with the two wires joined: what is written comes back.
type echoPort struct {
	back   chan []byte
	closed chan struct{}
}

func newEchoPort() *echoPort {
	return &echoPort{back: make(chan []byte, 64), closed: make(chan struct{})}
}

func (p *echoPort) Write(b []byte) (int, error) {
	p.back <- append([]byte(nil), b...)
	return len(b), nil
}

func (p *echoPort) Listen(ctx context.Context, _ int, onChunk func([]byte)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case b := <-p.back:
			onChunk(b)
		}
	}
}

func (p *echoPort) Close() error {
	close(p.closed)
	return nil
}

func useEchoPort(t *testing.T) *echoPort {
	t.Helper()
	port := newEchoPort()
	real := dialUART
	dialUART = func(orbitos.UartConfig, string) (serialPort, error) { return port, nil }
	t.Cleanup(func() { dialUART = real })
	return port
}

type frame struct {
	kind    byte
	payload string
}

func parseFrames(t *testing.T, b []byte) []frame {
	t.Helper()
	var out []frame
	for len(b) > 0 {
		if len(b) < 5 {
			t.Fatalf("short frame: %q", b)
		}
		n := int(binary.BigEndian.Uint32(b[1:5]))
		out = append(out, frame{b[0], string(b[5 : 5+n])})
		b = b[5+n:]
	}
	return out
}

func do(t *testing.T, method, url string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestHTTPLink(t *testing.T) {
	port := useEchoPort(t)
	mux := http.NewServeMux()
	registerHTTPLink(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tok := "?t=" + pageToken

	// No token: refused.
	if code, _ := do(t, "POST", srv.URL+"/api/session", []byte(`{"port":"ttyUSB0"}`)); code != http.StatusForbidden {
		t.Fatalf("no token: got %d, want 403", code)
	}
	// No port name: refused, with the reason.
	if code, body := do(t, "POST", srv.URL+"/api/session"+tok, []byte(`{}`)); code != http.StatusBadRequest || !strings.Contains(string(body), "Missing UART port") {
		t.Fatalf("no port: got %d %s", code, body)
	}

	code, body := do(t, "POST", srv.URL+"/api/session"+tok, []byte(`{"port":"ttyUSB0","baud":115200}`))
	if code != http.StatusOK {
		t.Fatalf("open: got %d %s", code, body)
	}
	var opened struct{ ID string }
	if err := json.Unmarshal(body, &opened); err != nil || opened.ID == "" {
		t.Fatalf("open answer: %s", body)
	}
	s := tok + "&s=" + opened.ID

	// One console at a time.
	if code, _ := do(t, "POST", srv.URL+"/api/session"+tok, []byte(`{"port":"ttyUSB1"}`)); code != http.StatusConflict {
		t.Fatalf("second console: got %d, want 409", code)
	}

	// What is sent comes back, and the answer says where to continue.
	if code, _ := do(t, "POST", srv.URL+"/api/session/tx"+s, []byte("hello")); code != http.StatusNoContent {
		t.Fatalf("send: got %d", code)
	}
	_, body = do(t, "GET", srv.URL+"/api/session/rx"+s+"&from=0&seen=0", nil)
	got := parseFrames(t, body)
	if len(got) != 2 || got[0] != (frame{frameData, "hello"}) || got[1] != (frame{framePosition, "5,0"}) {
		t.Fatalf("receive: %+v", got)
	}

	// A request with nothing new waits, and is answered when bytes arrive.
	start := time.Now()
	go func() {
		time.Sleep(150 * time.Millisecond)
		do(t, "POST", srv.URL+"/api/session/tx"+s, []byte("later"))
	}()
	_, body = do(t, "GET", srv.URL+"/api/session/rx"+s+"&from=5&seen=0", nil)
	got = parseFrames(t, body)
	if waited := time.Since(start); waited < 100*time.Millisecond || waited > 5*time.Second {
		t.Fatalf("receive waited %v", waited)
	}
	if len(got) != 2 || got[0] != (frame{frameData, "later"}) || got[1] != (frame{framePosition, "10,0"}) {
		t.Fatalf("receive after waiting: %+v", got)
	}

	// Asking again from an old position gives the bytes again (an answer lost on the way).
	_, body = do(t, "GET", srv.URL+"/api/session/rx"+s+"&from=5&seen=0", nil)
	if got = parseFrames(t, body); got[0] != (frame{frameData, "later"}) {
		t.Fatalf("receive again: %+v", got)
	}

	// More than the buffer holds: the page is told that bytes were lost.
	link := findLink(opened.ID)
	link.received(bytes.Repeat([]byte{'x'}, linkBufferSize+100))
	_, body = do(t, "GET", srv.URL+"/api/session/rx"+s+"&from=10&seen=0", nil)
	got = parseFrames(t, body)
	if len(got) != 3 || got[0].kind != frameLost || got[1].kind != frameData || len(got[1].payload) != linkBufferSize {
		t.Fatalf("overflow: %d frames, first %c", len(got), got[0].kind)
	}
	next := strings.Split(got[2].payload, ",")[0]

	// Closing frees the port; a waiting request is told that the console ended.
	if code, _ := do(t, "POST", srv.URL+"/api/session/close"+s, nil); code != http.StatusNoContent {
		t.Fatalf("close: got %d", code)
	}
	select {
	case <-port.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the port was not closed")
	}
	_, body = do(t, "GET", srv.URL+"/api/session/rx"+s+"&from="+next+"&seen=0", nil)
	got = parseFrames(t, body)
	if len(got) != 2 || got[0].kind != frameEnded {
		t.Fatalf("after close: %+v", got)
	}

	// The console is free again; the old one is gone.
	useEchoPort(t)
	code, body = do(t, "POST", srv.URL+"/api/session"+tok, []byte(`{"port":"ttyUSB0"}`))
	if code != http.StatusOK {
		t.Fatalf("open again: got %d %s", code, body)
	}
	if code, _ := do(t, "GET", srv.URL+"/api/session/rx"+s+"&from=0&seen=0", nil); code != http.StatusGone {
		t.Fatalf("old console: got %d, want 410", code)
	}
	var again struct{ ID string }
	_ = json.Unmarshal(body, &again)
	do(t, "POST", srv.URL+"/api/session/close"+tok+"&s="+again.ID, nil)
}

// TestServeForBrowser serves the real page with an echo port and no WebSocket,
// as a remote-access service that only carries HTTP would. It is for trying
// the page by hand or with a headless browser:
//
//	SERIAL_CONSOLE_BROWSER_TEST=127.0.0.1:8766 go test -run TestServeForBrowser -timeout 0 ./cmd/serial_console
func TestServeForBrowser(t *testing.T) {
	addr := os.Getenv("SERIAL_CONSOLE_BROWSER_TEST")
	if addr == "" {
		t.Skip("set SERIAL_CONSOLE_BROWSER_TEST=host:port to serve the page")
	}
	dialUART = func(orbitos.UartConfig, string) (serialPort, error) { return newEchoPort(), nil }

	page, err := stampedPage()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerHTTPLink(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
		case r.URL.Path == "/style.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			_, _ = w.Write(styleCSS)
		case r.URL.Path == "/api/ports":
			writeJSON(w, http.StatusOK, map[string][]string{"ports": {"ttyUSB0", "ttyAMA0"}})
		case strings.HasPrefix(r.URL.Path, "/xterm/"):
			http.ServeFileFS(w, r, staticFiles, "static"+r.URL.Path)
		default:
			http.NotFound(w, r) // also /ws: the WebSocket does not get through
		}
	})
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("serving on http://%s", listener.Addr())
	_ = http.Serve(listener, mux)
}
