package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"goji.io"
	"goji.io/pat"
)

type DeviceID string

type Command struct {
	Scene string        `json:"scene"`
	Fade  time.Duration `json:"fade"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type CommandAndControl struct {
	M           sync.Mutex
	LastCommand map[DeviceID]Command
	Connections map[DeviceID]map[*websocket.Conn]bool
}

func (cnc *CommandAndControl) sendCommand(conn *websocket.Conn, cmd Command) error {
	return conn.WriteJSON(cmd)
}

func (cnc *CommandAndControl) blastCommand(device DeviceID, cmd Command) error {
	cnc.M.Lock()
	defer cnc.M.Unlock()

	log.Printf("cnc: blasting command: device=%s command=%s devices=%d", device, cmd, len(cnc.Connections[device]))

	var firstErr error
	for conn := range cnc.Connections[device] {
		err := cnc.sendCommand(conn, cmd)
		if err != nil {
			log.Printf("cnc: error sending command: device=%s addr=%s command=%s err=%q",
				device, conn.RemoteAddr().String(), cmd, err)
		}

		if firstErr == nil {
			firstErr = err
		}
	}

	cnc.LastCommand[device] = cmd

	return firstErr
}

// HandleWebsocket accepts incoming connections. To subscribe to
// commands, a device just needs to connect. The only data sent from
// the device to the CNC server is a device identifier, which is
// provided as part of the URL.
//
// Otherwise, the only non-control message sent over the websocket are
// commands, which are sent as websocket text messages. (Note that the
// fade time is encoded in nanoseconds.)
//
// If a command has previously been sent to the named device through
// this server, that command is automatically sent to the device when
// it first connects.
func (cnc *CommandAndControl) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	device := DeviceID(pat.Param(r, "device"))

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("cnc: error upgrading websocket: device=%s addr=%s err=%q",
			device, r.RemoteAddr, err)
		http.Error(w, err.Error(), 500)
		return
	}

	log.Printf("cnc: accepted new connection: device=%s addr=%s",
		device, r.RemoteAddr)

	// we never need to read messages, so spin off a goroutine to
	// handle connection maintenance and close
	go func() {
		if _, _, err := conn.NextReader(); err != nil {
			log.Printf("cnc: connection closed: device=%s addr=%s",
				device, r.RemoteAddr)
			conn.Close()

			cnc.M.Lock()
			defer cnc.M.Unlock()

			delete(cnc.Connections[device], conn)
			return
		}
	}()

	cnc.M.Lock()
	defer cnc.M.Unlock()
	if cmd, ok := cnc.LastCommand[device]; ok {
		log.Printf("cnc: sending initial command to device: device=%s addr=%s command=%s",
			device, r.RemoteAddr, cmd)
		err := cnc.sendCommand(conn, cmd)
		if err != nil {
			log.Printf("cnc: error sending initial command to device: device=%s addr=%s err=%q",
				device, r.RemoteAddr, err)
			return
		}
	}

	if _, ok := cnc.Connections[device]; !ok {
		cnc.Connections[device] = make(map[*websocket.Conn]bool)
	}
	cnc.Connections[device][conn] = true
}

type sequenceElem struct {
	deviceID DeviceID
	command  Command
}

// HandleSequence handles control requests to send a sequence of
// changes to the devices
//
// It accepts a "sequence" form argument as either a query string or
// POST parameter. The sequence is a series of commands, which are
// each a tuple of (device ID, scene, fade time) joined with ".". The
// separate commands are joined with ","
//
// (These aren't the greatest separators, but it keeps the protocol
// simple and concise.)
//
// Ex (GET): curl -i http://127.0.0.1:8080/sequence?sequence=a.SolidRed.2s,b.SolidGreen.1s500ms
// EX (POST): curl -i -X POST http://127.0.0.1:8080/sequence -d "sequence=a.SolidRed.2s,b.SolidGreen.1s500ms"
//
// It returns a 200 if all commands were successfully sent to all
// connected devices, and a 500 otherwise.
func (cnc *CommandAndControl) HandleSequence(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		http.Error(w, err.Error(), 400)
	}

	sequence := r.Form.Get("sequence")
	if sequence == "" {
		http.Error(w, "no sequence provided", 400)
		return
	}

	commandStrings := strings.Split(sequence, ",")
	commands := make([]sequenceElem, 0, len(commandStrings))
	for _, str := range commandStrings {
		split := strings.Split(str, ".")
		if len(split) != 3 {
			http.Error(w, fmt.Sprintf("malformed sequence: %s", str), 400)
			return
		}

		deviceID := DeviceID(split[0])
		scene := split[1]
		fade, err := time.ParseDuration(split[2])
		if err != nil {
			http.Error(w, fmt.Sprintf("malformed sequence fade duration: %s", str), 400)
			return
		}

		commands = append(commands, sequenceElem{deviceID, Command{scene, fade}})
	}

	var firstErr error
	for _, elem := range commands {
		err := cnc.blastCommand(elem.deviceID, elem.command)
		if firstErr == nil {
			firstErr = err
		}
	}

	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte("error\n"))
		return
	}

	w.Write([]byte("ok\n"))
}

func main() {
	cnc := CommandAndControl{
		LastCommand: make(map[DeviceID]Command),
		Connections: make(map[DeviceID]map[*websocket.Conn]bool),
	}

	mux := goji.NewMux()
	mux.Use(func(h http.Handler) http.Handler {
		if os.Getenv("HUNT_REDIRECT_HTTP") == "" {
			return h
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				h.ServeHTTP(w, r)
				return
			}

			if r.Header.Get("X-Forwarded-Proto") == "https" {
				h.ServeHTTP(w, r)
				return
			}

			url := r.URL
			url.Scheme = "https"
			http.Redirect(w, r, url.String(), http.StatusPermanentRedirect)
		})
	})
	mux.HandleFunc(pat.Get("/ws/:device"), cnc.HandleWebsocket)
	mux.HandleFunc(pat.New("/sequence"), cnc.HandleSequence)
	mux.HandleFunc(pat.Get("/healthz"), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	http.ListenAndServe(":8080", mux)
}
