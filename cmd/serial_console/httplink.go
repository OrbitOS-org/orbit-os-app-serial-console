package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	orbitos "github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
)

// The HTTP link is a second way for the page to reach the serial port, for
// when a WebSocket cannot get to the app (a remote-access service or a proxy
// that only carries plain HTTP requests). The page opens a console with one
// request, then asks again and again for what was received (each request is
// answered as soon as there is something, or after a short wait), and sends
// what is typed with separate requests.
//
//	POST /api/session        JSON settings        → {"id": "..."}
//	GET  /api/session/rx     ?s=id&from=N&seen=M  → frames (see pollFrames)
//	POST /api/session/tx     ?s=id, body = bytes  → 204
//	POST /api/session/close  ?s=id                → 204
//
// Every request carries the page token, like the WebSocket.

const (
	linkBufferSize = 1 << 20               // received bytes kept for a page that is slow to ask
	linkPollHold   = 15 * time.Second      // longest wait of one request; remote tunnels cut long requests
	linkLinger     = 25 * time.Millisecond // short wait for more bytes, to answer with a fuller block
	linkIdleLimit  = 45 * time.Second      // no request from the page for this long: the console is closed
	linkMaxWrite   = 1 << 20               // largest body accepted by one send request
	linkReapPeriod = 5 * time.Second
)

// Frame types of a receive answer. A frame is one type byte, the payload
// length in 4 bytes (big-endian), and the payload.
const (
	frameData     = 'D' // bytes received from the serial port
	frameMessage  = 'M' // a message from the app (an error), as text
	frameLost     = 'L' // received bytes were dropped before the page asked for them
	frameEnded    = 'E' // the console is closed
	framePosition = 'P' // "from,seen" to send with the next request; always the last frame
)

type httpLink struct {
	id    string
	write func([]byte) error
	stop  context.CancelFunc

	mu       sync.Mutex
	buf      []byte        // received bytes not dropped yet
	base     int64         // position of buf[0], counted from the opening of the port
	notes    []string      // messages from the app
	ended    bool          // the port is closed
	changed  chan struct{} // closed, and replaced, each time something above changes
	polling  int           // receive requests waiting now
	lastPoll time.Time
}

var (
	linkMu      sync.Mutex
	currentLink *httpLink

	// appCtx ends when the app is asked to stop; open consoles end with it.
	appCtx = context.Background()
)

func findLink(id string) *httpLink {
	linkMu.Lock()
	defer linkMu.Unlock()
	if currentLink != nil && currentLink.id == id {
		return currentLink
	}
	return nil
}

// serialPort is what the HTTP link needs from an open serial port.
type serialPort interface {
	Write(data []byte) (int, error)
	Listen(ctx context.Context, maxChunkSize int, onChunk func([]byte)) error
	Close() error
}

// orbitPort is a serial port opened through the Orbit OS API, with the
// connection it was opened on.
type orbitPort struct {
	*orbitos.UartPort
	client *orbitos.Client
}

func (p orbitPort) Close() error {
	err := p.UartPort.Close()
	p.client.Close()
	return err
}

// dialUART opens the serial port. The tests replace it with a port that needs no device.
var dialUART = func(cfg orbitos.UartConfig, host string) (serialPort, error) {
	client, err := orbitos.NewClientAuto(host)
	if err != nil {
		return nil, fmt.Errorf("Gravity: %w", err)
	}
	uart, err := client.UartManager.Open(cfg)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("OpenUart: %w", err)
	}
	return orbitPort{UartPort: uart, client: client}, nil
}

// notify wakes the receive requests that are waiting. Call it with mu held.
func (l *httpLink) notify() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *httpLink) received(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, b...)
	if over := len(l.buf) - linkBufferSize; over > 0 {
		l.buf = append(l.buf[:0], l.buf[over:]...)
		l.base += int64(over)
	}
	l.notify()
}

func (l *httpLink) note(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notes = append(l.notes, text)
	l.notify()
}

func (l *httpLink) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended = true
	l.notify()
}

// openHTTPLink opens the serial port and starts a console for the page.
// On failure it returns the HTTP status to answer with.
func openHTTPLink(open uartOpenMsg) (*httpLink, int, error) {
	cfg, maxChunk, host, err := open.settings()
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if !acquireSession() {
		return nil, http.StatusConflict, fmt.Errorf("UART session already in use; close the other tab.")
	}

	uart, err := dialUART(cfg, host)
	if err != nil {
		releaseSession()
		return nil, http.StatusBadGateway, err
	}

	ctx, cancel := context.WithCancel(appCtx)
	l := &httpLink{
		id:       newPageToken(),
		stop:     cancel,
		changed:  make(chan struct{}),
		lastPoll: time.Now(),
	}
	l.write = func(b []byte) error {
		_, err := uart.Write(b)
		return err
	}

	linkMu.Lock()
	currentLink = l
	linkMu.Unlock()

	logger.Infof(logTag, "HTTP link UART: port=%q baud=%d gravity=%q", cfg.Port, cfg.Baudrate, host)

	// Read the port until the console is closed, then release everything.
	go func() {
		lerr := uart.Listen(ctx, maxChunk, l.received)
		if lerr != nil && ctx.Err() == nil {
			l.note("ListenUart: " + lerr.Error())
		}
		cancel()
		if cerr := uart.Close(); cerr != nil {
			logger.Warnf(logTag, "CloseUart: %v", cerr)
		}
		releaseSession()
		l.end()
	}()

	// Close the console when the page stops asking (tab closed, network lost).
	go func() {
		tick := time.NewTicker(linkReapPeriod)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				l.mu.Lock()
				idle := l.polling == 0 && time.Since(l.lastPoll) > linkIdleLimit
				l.mu.Unlock()
				if idle {
					logger.Infof(logTag, "HTTP link: the page stopped asking; closing the port")
					cancel()
					return
				}
			}
		}
	}()

	return l, http.StatusOK, nil
}

func appendFrame(dst []byte, kind byte, payload []byte) []byte {
	dst = append(dst, kind)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(payload)))
	return append(dst, payload...)
}

// pollFrames waits until there is something the page has not seen (bytes after
// position from, messages after the first seen ones, or the end), or until the
// hold time passes, and returns it as frames.
func (l *httpLink) pollFrames(ctx context.Context, from int64, seen int) []byte {
	l.mu.Lock()
	l.polling++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.polling--
		l.lastPoll = time.Now()
		l.mu.Unlock()
	}()

	hold := time.NewTimer(linkPollHold)
	defer hold.Stop()
	for waiting := true; waiting; {
		l.mu.Lock()
		news := from != l.base+int64(len(l.buf)) || seen < len(l.notes) || l.ended
		changed := l.changed
		ended := l.ended
		l.mu.Unlock()
		if news {
			if !ended {
				time.Sleep(linkLinger)
			}
			break
		}
		select {
		case <-changed:
		case <-hold.C:
			waiting = false
		case <-ctx.Done():
			waiting = false
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	var out []byte
	end := l.base + int64(len(l.buf))
	if from < l.base || from > end {
		if from < l.base {
			out = appendFrame(out, frameLost, nil)
		}
		from = l.base
	}
	if from < end {
		out = appendFrame(out, frameData, l.buf[from-l.base:])
		from = end
	}
	if seen < 0 || seen > len(l.notes) {
		seen = 0
	}
	for ; seen < len(l.notes); seen++ {
		out = appendFrame(out, frameMessage, []byte(l.notes[seen]))
	}
	if l.ended {
		out = appendFrame(out, frameEnded, nil)
	}
	return appendFrame(out, framePosition, []byte(strconv.FormatInt(from, 10)+","+strconv.Itoa(seen)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// linkRequest checks what every HTTP link request needs: the method and the page token.
func linkRequest(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !fromOwnPage(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// linkOf finds the console named in the request; it answers 410 Gone when it no longer exists.
func linkOf(w http.ResponseWriter, r *http.Request) *httpLink {
	l := findLink(r.URL.Query().Get("s"))
	if l == nil {
		http.Error(w, "this console is closed", http.StatusGone)
	}
	return l
}

func registerHTTPLink(mux *http.ServeMux) {
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if !linkRequest(w, r, http.MethodPost) {
			return
		}
		var open uartOpenMsg
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&open); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid settings: " + err.Error()})
			return
		}
		l, status, err := openHTTPLink(open)
		if err != nil {
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": l.id})
	})

	mux.HandleFunc("/api/session/rx", func(w http.ResponseWriter, r *http.Request) {
		if !linkRequest(w, r, http.MethodGet) {
			return
		}
		l := linkOf(w, r)
		if l == nil {
			return
		}
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		seen, _ := strconv.Atoi(r.URL.Query().Get("seen"))
		frames := l.pollFrames(r.Context(), from, seen)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(frames)
	})

	mux.HandleFunc("/api/session/tx", func(w http.ResponseWriter, r *http.Request) {
		if !linkRequest(w, r, http.MethodPost) {
			return
		}
		l := linkOf(w, r)
		if l == nil {
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, linkMaxWrite))
		if err != nil {
			http.Error(w, "too much data in one request", http.StatusRequestEntityTooLarge)
			return
		}
		if len(data) > 0 {
			if err := l.write(data); err != nil {
				l.note("WriteUart: " + err.Error())
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/session/close", func(w http.ResponseWriter, r *http.Request) {
		if !linkRequest(w, r, http.MethodPost) {
			return
		}
		if l := findLink(r.URL.Query().Get("s")); l != nil {
			l.stop()
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
