// Serial Console — PuTTY-style web console over UART through the Orbit OS UART service.
//
// The app serves a page and a WebSocket (/ws) on 127.0.0.1 only and registers them
// with the Orbit OS Launcher, which serves the page at http://<device>/console
// behind the device login.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	orbitos "github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/metadata"
)

//go:embed metadata.json
var identityJSON []byte

//go:embed page.html
var pageHTML []byte

//go:embed style.css
var styleCSS []byte

//go:embed orbit-logo.svg
var orbitLogoSVG []byte

//go:embed favicon.svg
var iconSVG []byte

// The terminal emulator (xterm.js and its add-ons), served to the page as it is.
//
//go:embed static/xterm
var staticFiles embed.FS

var appManifest = metadata.MustParseAppManifestJSON(identityJSON)

const logTag = "console"

const (
	// route is the path the Launcher serves the page at: http://<device>/console.
	route = "/console"

	// Web interface ports are reserved to the range portMin–portMax. The app
	// starts at preferredPort and moves to the next one when a port is taken.
	portMin       = 50000
	portMax       = 60000
	preferredPort = 50002

	defaultBaud     = 9600
	defaultMaxChunk = 4096
)

// deviceHost is only used off-device (development from a laptop over TCP + mTLS);
// on the device the SDK connects through the local Unix socket. Set with -host.
var gravityHost = "192.168.1.100"

// pageToken is a random value made at start and written into the page. The
// page sends it back when it opens the WebSocket.
var pageToken = newPageToken()

func newPageToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no random source: nothing safe to do
	}
	return hex.EncodeToString(b)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     fromOwnPage,
}

// fromOwnPage accepts a WebSocket only from the page served by this app: the
// request must carry the token written into that page. A page of another site,
// open in the same browser, cannot read the token, so it cannot drive the
// serial port. The Origin header cannot be used for this: behind the Launcher
// the app does not see the address the browser used.
func fromOwnPage(r *http.Request) bool {
	token := r.URL.Query().Get("t")
	return subtle.ConstantTimeCompare([]byte(token), []byte(pageToken)) == 1
}

var (
	sessionMu     sync.Mutex
	sessionActive bool
)

func main() {
	flag.StringVar(&gravityHost, "host", gravityHost, "Device IP address (development only; ignored when the app runs on the device)")
	flag.Parse()

	meta := metadata.Build(appManifest)
	logger.Init(appManifest.PackageId, "INFO", true)

	logger.Infof(logTag, "Starting %s v%s", meta.Name, meta.Version)
	appManifest.PrintInfo()

	// Stop cleanly on Ctrl+C, and on SIGTERM, which Orbit OS sends to stop an app.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		logger.Fatalf(logTag, "%v", err)
		os.Exit(1)
	}
}

// stampedPage returns the page with the app version, the WebSocket token and
// a mark of this build in the address of every file it loads. Browsers keep those files for a while; with the mark,
// a new version of the app is never shown with the files of the previous one.
func stampedPage() ([]byte, error) {
	h := sha256.New()
	h.Write(pageHTML)
	h.Write(styleCSS)
	h.Write(orbitLogoSVG)
	err := fs.WalkDir(staticFiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := staticFiles.ReadFile(path)
		h.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	build := hex.EncodeToString(h.Sum(nil))[:10]
	page := bytes.ReplaceAll(pageHTML, []byte("__BUILD__"), []byte(build))
	page = bytes.ReplaceAll(page, []byte("__VERSION__"), []byte(appManifest.Version))
	return bytes.ReplaceAll(page, []byte("__TOKEN__"), []byte(pageToken)), nil
}

func run(ctx context.Context) error {
	mux := http.NewServeMux()

	page, err := stampedPage()
	if err != nil {
		return fmt.Errorf("embedded files: %w", err)
	}

	serveIcon := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(iconSVG)
	}
	mux.HandleFunc("/favicon.svg", serveIcon)
	mux.HandleFunc("/favicon.ico", serveIcon)

	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(styleCSS)
	})

	mux.HandleFunc("/orbit-logo.svg", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(orbitLogoSVG)
	})

	xterm, err := fs.Sub(staticFiles, "static/xterm")
	if err != nil {
		return fmt.Errorf("embedded files: %w", err)
	}
	xtermFiles := http.StripPrefix("/xterm/", http.FileServer(http.FS(xterm)))
	mux.HandleFunc("/xterm/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		xtermFiles.ServeHTTP(w, r)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache") // the page itself is always asked for again
		_, _ = w.Write(page)
	})

	mux.HandleFunc("/api/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		host := strings.TrimSpace(r.URL.Query().Get("gravity"))
		if host == "" {
			host = gravityHost
		}
		client, err := orbitos.NewClientAuto(host)
		if err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		defer client.Close()
		ports, err := client.UartManager.ListPorts()
		if err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string][]string{"ports": ports})
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleUARTWebSocket(w, r)
	})

	// The same console over plain HTTP requests, for when a WebSocket cannot reach the app.
	appCtx = ctx
	registerHTTPLink(mux)

	// Connect to Orbit OS to register the page with the Launcher. Without it
	// (for example when developing with no device) the page is still served
	// on this computer only.
	client, err := orbitos.NewClientAuto(gravityHost)
	if err != nil {
		logger.Warnf(logTag, "Orbit OS client: %v — the page will not be registered with the Launcher", err)
		client = nil
	} else {
		defer client.Close()
	}

	listener, registered, err := listenAndRegister(client)
	if err != nil {
		return err
	}
	if registered {
		logger.Infof(logTag, "Listening on %s, registered with the Launcher at %s", listener.Addr(), route)
		// Remove the page from the Launcher when the app stops.
		defer func() {
			if err := client.AppHubManager.UnregisterService(); err != nil {
				logger.Warnf(logTag, "UnregisterService: %v", err)
			}
		}()
	} else {
		logger.Infof(logTag, "Listening on http://%s (not registered with the Launcher)", listener.Addr())
	}

	server := &http.Server{Handler: mux}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("Serve: %w", err)
	case <-ctx.Done():
	}

	// Give the requests in progress a moment to finish. Open consoles are
	// closed: the UART port is released when the app exits.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("Shutdown: %w", err)
	}
	return nil
}

// listenAndRegister finds a free port in the reserved range, starting at
// preferredPort, and registers the page with the Launcher (proxy mode: the
// Launcher forwards the page and the WebSocket). With no client it only listens.
func listenAndRegister(client *orbitos.Client) (listener net.Listener, registered bool, err error) {
	span := portMax - portMin + 1
	for i := 0; i < span; i++ {
		port := portMin + (preferredPort-portMin+i)%span
		addr := fmt.Sprintf("127.0.0.1:%d", port)

		listener, err = net.Listen("tcp", addr)
		if err != nil {
			continue // port taken: try the next one
		}
		if client == nil || client.AppHubManager == nil {
			return listener, false, nil
		}
		if err := client.AppHubManager.RegisterWebUI(addr, route); err != nil {
			listener.Close() // the Launcher refused this port: try the next one
			logger.Warnf(logTag, "RegisterWebUI %s: %v", addr, err)
			continue
		}
		return listener, true, nil
	}
	return nil, false, fmt.Errorf("no free web interface port in %d-%d", portMin, portMax)
}

// uartOpenMsg is the page's settings (JSON): the first WebSocket message, or the body that opens an HTTP link.
type uartOpenMsg struct {
	Port     string `json:"port"`
	Baud     int    `json:"baud"`
	DataBits int    `json:"databits"`
	Parity   string `json:"parity"`
	Stop     int    `json:"stop"`
	Chunk    int    `json:"chunk"`
	Gravity  string `json:"gravity"`
}

// settings checks the page's settings and fills in the defaults.
func (open uartOpenMsg) settings() (cfg orbitos.UartConfig, maxChunk int, host string, err error) {
	port := strings.TrimSpace(open.Port)
	if port == "" {
		return cfg, 0, "", errors.New("Missing UART port: set \"port\" in the JSON config (e.g. ttyUSB0).")
	}

	baud := open.Baud
	if baud <= 0 {
		baud = defaultBaud
	}

	databits := open.DataBits
	if databits < 5 || databits > 8 {
		databits = 8
	}

	stop := open.Stop
	if stop != 2 {
		stop = 1
	}

	maxChunk = open.Chunk
	if maxChunk < 256 {
		maxChunk = defaultMaxChunk
	}

	host = strings.TrimSpace(open.Gravity)
	if host == "" {
		host = gravityHost
	}

	cfg = orbitos.UartConfig{
		Port:        port,
		Baudrate:    baud,
		DataBits:    databits,
		Parity:      parseParity(open.Parity),
		StopBits:    parseStopBits(stop),
		FlowControl: orbitos.UartFlowNone,
	}
	return cfg, maxChunk, host, nil
}

// acquireSession takes the single console; it reports false when another page has it.
func acquireSession() bool {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if sessionActive {
		return false
	}
	sessionActive = true
	return true
}

func releaseSession() {
	sessionMu.Lock()
	sessionActive = false
	sessionMu.Unlock()
}

func handleUARTWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Warnf(logTag, "WebSocket upgrade: %v", err)
		return
	}

	closeWith := func(msg string) {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(msg))
		_ = conn.Close()
	}

	mt, payload, err := conn.ReadMessage()
	if err != nil {
		logger.Warnf(logTag, "first WebSocket message: %v", err)
		return
	}
	if mt == websocket.BinaryMessage {
		closeWith("The first message must be JSON (text) with port, baud, etc.")
		return
	}

	var open uartOpenMsg
	if err := json.Unmarshal(payload, &open); err != nil {
		closeWith("Invalid JSON: " + err.Error() + " — send {\"port\":\"ttyUSB0\",\"baud\":115200,...} as the first message.")
		return
	}

	cfg, maxChunk, host, err := open.settings()
	if err != nil {
		closeWith(err.Error())
		return
	}

	logger.Infof(logTag, "WebSocket UART: port=%q baud=%d gravity=%q", cfg.Port, cfg.Baudrate, host)

	if !acquireSession() {
		closeWith("UART session already in use; close the other tab.")
		return
	}
	defer releaseSession()

	var writeMu sync.Mutex
	writeText := func(s string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(s))
	}
	writeBin := func(b []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		_ = conn.WriteMessage(websocket.BinaryMessage, b)
	}

	client, err := orbitos.NewClientAuto(host)
	if err != nil {
		writeText("Gravity: " + err.Error())
		_ = conn.Close()
		return
	}
	defer client.Close()

	uart, err := client.UartManager.Open(cfg)
	if err != nil {
		writeText("OpenUart: " + err.Error())
		_ = conn.Close()
		return
	}
	defer func() {
		if cerr := uart.Close(); cerr != nil {
			logger.Warnf(logTag, "CloseUart: %v", cerr)
		}
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		lerr := uart.Listen(ctx, maxChunk, func(b []byte) {
			writeBin(b)
		})
		if lerr != nil && ctx.Err() == nil {
			writeText("ListenUart: " + lerr.Error())
		}
		_ = conn.Close()
	}()

	for {
		mt, msg, rerr := conn.ReadMessage()
		if rerr != nil {
			cancel()
			<-listenDone
			return
		}
		var data []byte
		if mt == websocket.BinaryMessage {
			data = msg
		} else {
			data = []byte(msg)
		}
		if _, werr := uart.Write(data); werr != nil {
			writeText("WriteUart: " + werr.Error())
		}
	}
}

func parseParity(s string) orbitos.UartParity {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "E":
		return orbitos.UartParityEven
	case "O":
		return orbitos.UartParityOdd
	default:
		return orbitos.UartParityNone
	}
}

func parseStopBits(n int) orbitos.UartStopBits {
	if n == 2 {
		return orbitos.UartStopBits2
	}
	return orbitos.UartStopBits1
}
