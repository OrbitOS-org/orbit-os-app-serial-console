// Serial Console — PuTTY-style web console over UART via the Orbit OS UART service: HTTP :9002 + WebSocket /ws.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	apphubv26 "github.com/OrbitOS-org/orbit-os-sdk-go/v26/api/app_hub_service/v26"
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

//go:embed orbit-logo.png
var orbitLogoPNG []byte

//go:embed favicon.svg
var iconSVG []byte

var appManifest = metadata.MustParseAppManifestJSON(identityJSON)

const logTag = "console"

const (
	webUIListen     = "0.0.0.0:9002"
	defaultBaud     = 9600
	defaultMaxChunk = 4096
)

// deviceHost is only used off-device (development from a laptop over TCP + mTLS);
// on the device the SDK connects through the local Unix socket. Set with -host.
var gravityHost = "192.168.1.100"

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // local testing
	},
}

var (
	sessionMu     sync.Mutex
	sessionActive bool
)

func main() {
	flag.StringVar(&gravityHost, "host", gravityHost, "Device IP address (development only)")
	flag.Parse()

	meta := metadata.Build(appManifest)
	logger.Init(appManifest.PackageId, "INFO", true)

	logger.Infof(logTag, "Starting %s v%s — HTTP %s", meta.Name, meta.Version, webUIListen)
	appManifest.PrintInfo()

	serveIcon := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(iconSVG)
	}
	http.HandleFunc("/favicon.svg", serveIcon)
	http.HandleFunc("/favicon.ico", serveIcon)

	http.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(styleCSS)
	})

	http.HandleFunc("/orbit-logo.png", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(orbitLogoPNG)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(pageHTML)
	})

	http.HandleFunc("/api/ports", func(w http.ResponseWriter, r *http.Request) {
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

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleUARTWebSocket(w, r)
	})

	logger.Infof(logTag, "Open http://%s — set port/baud on the page and click Connect.", webUIListen)

	// Register with AppHub portal (proxy mode — WebSocket is tunnelled by the portal).
	client, err := orbitos.NewClientAuto(gravityHost)
	if err != nil {
		logger.Warnf(logTag, "Gravity client: %v — AppHub registration skipped", err)
	} else {
		defer client.Close()
		if client.AppHubManager != nil {
			go func() {
				if err := client.AppHubManager.RegisterService(&apphubv26.RegisterServiceRequest{
					Host:   "127.0.0.1",
					Port:   9002,
					Routes: []*apphubv26.Route{{Path: "/console"}},
					Health: &apphubv26.HealthCheck{Type: apphubv26.HealthCheckType_HEALTH_CHECK_TCP},
				}); err != nil {
					logger.Warnf(logTag, "AppHub RegisterService: %v", err)
				}
			}()
		}
	}

	if err := http.ListenAndServe(webUIListen, nil); err != nil {
		logger.Fatalf(logTag, "ListenAndServe: %v", err)
		os.Exit(1)
	}
}

// uartOpenMsg is the first WebSocket message (JSON): port/baud/gravity config.
type uartOpenMsg struct {
	Port     string `json:"port"`
	Baud     int    `json:"baud"`
	DataBits int    `json:"databits"`
	Parity   string `json:"parity"`
	Stop     int    `json:"stop"`
	Chunk    int    `json:"chunk"`
	Gravity  string `json:"gravity"`
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

	port := strings.TrimSpace(open.Port)
	if port == "" {
		closeWith("Missing UART port: set \"port\" in the JSON config (e.g. ttyUSB0).")
		return
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

	maxChunk := open.Chunk
	if maxChunk < 256 {
		maxChunk = defaultMaxChunk
	}

	host := strings.TrimSpace(open.Gravity)
	if host == "" {
		host = gravityHost
	}

	cfg := orbitos.UartConfig{
		Port:        port,
		Baudrate:    baud,
		DataBits:    databits,
		Parity:      parseParity(open.Parity),
		StopBits:    parseStopBits(stop),
		FlowControl: orbitos.UartFlowNone,
	}

	logger.Infof(logTag, "WebSocket UART: port=%q baud=%d gravity=%q", port, baud, host)

	sessionMu.Lock()
	if sessionActive {
		sessionMu.Unlock()
		closeWith("UART session already in use; close the other tab.")
		return
	}
	sessionActive = true
	sessionMu.Unlock()
	defer func() {
		sessionMu.Lock()
		sessionActive = false
		sessionMu.Unlock()
	}()

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
