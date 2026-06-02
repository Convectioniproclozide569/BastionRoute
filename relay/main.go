package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	WriteTimeout = 5 * time.Second
	MaxQueueSize = 4096
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// =========================
// STRUCTURES
// =========================

type PeerConn struct {
	Conn *websocket.Conn
	Send chan []byte
	Once sync.Once // Ensures clean, single-time teardown
}

func NewPeerConn(conn *websocket.Conn) *PeerConn {
	return &PeerConn{
		Conn: conn,
		Send: make(chan []byte, MaxQueueSize),
	}
}

// Close safely tears down the writer loop and socket exactly once
func (p *PeerConn) Close() {
	p.Once.Do(func() {
		close(p.Send) // Unblocks the writer loop
		p.Conn.Close()
	})
}

type Room struct {
	Peers   sync.Map // peerID → *PeerConn
	WGPeers sync.Map // peerID → *PeerConn

	mu      sync.RWMutex
	Control *websocket.Conn // Protects control plane connection
}

// Thread-safe helper to notify control plane
func (r *Room) NotifyControl(event string, peerID string) {
	r.mu.Lock() // Upgraded to exclusive Lock to safe-guard concurrent writes with the Ping loop
	defer r.mu.Unlock()

	if r.Control != nil {
		_ = r.Control.SetWriteDeadline(time.Now().Add(WriteTimeout))
		_ = r.Control.WriteMessage(websocket.TextMessage, []byte(event+" "+peerID))
	}
}

var rooms sync.Map

// =========================
// ROOM MANAGEMENT
// =========================

func getRoom(roomID string) *Room {
	val, loaded := rooms.LoadOrStore(roomID, &Room{})
	if !loaded {
		log.Println("[ROOM] created:", roomID)
	}
	return val.(*Room)
}

// =========================
// UTIL
// =========================

func tune(conn *websocket.Conn) {
	raw := conn.UnderlyingConn()
	if raw != nil {
		if tcp, ok := raw.(interface {
			SetNoDelay(bool) error
		}); ok {
			_ = tcp.SetNoDelay(true)
		}
	}
}

// Safe writer loop shared by BOTH normal peers and WG shims
func writer(p *PeerConn) {
	for data := range p.Send {
		_ = p.Conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
		err := p.Conn.WriteMessage(websocket.BinaryMessage, data)
		if err != nil {
			break
		}
	}
	p.Conn.Close()
}

// =========================
// HANDLERS
// =========================

func controlHandler(w http.ResponseWriter, r *http.Request, roomID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	tune(conn)

	room := getRoom(roomID)

	room.mu.Lock()
	if room.Control != nil {
		room.mu.Unlock()
		log.Println("[CONTROL] duplicate rejected:", roomID)
		conn.Close()
		return
	}
	room.Control = conn
	room.mu.Unlock()

	log.Println("[CONTROL] connected:", roomID)

	// --- NEW: Asynchronous Keep-Alive Ping Loop ---
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				room.mu.Lock()
				// Double-check ownership before writing
				if room.Control != conn {
					room.mu.Unlock()
					return
				}
				_ = conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				room.mu.Unlock()
				if err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()
	// ───────────────────────────────────────────────

	// Keep-alive/Read loop for control plane
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		log.Println("[CONTROL] incoming message ignored:", string(msg))
	}

	// Signifies structural loop breakdown to the ping thread
	close(done)

	room.mu.Lock()
	if room.Control == conn {
		room.Control = nil
	}
	room.mu.Unlock()
	conn.Close()

	log.Println("[CONTROL] disconnected:", roomID)
}

func peerHandler(w http.ResponseWriter, r *http.Request, roomID, peerID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	tune(conn)

	room := getRoom(roomID)
	p := NewPeerConn(conn)

	if _, loaded := room.Peers.LoadOrStore(peerID, p); loaded {
		log.Println("[PEER] duplicate rejected:", peerID)
		conn.Close()
		return
	}

	log.Println("[PEER] connected:", peerID)
	go writer(p)
	room.NotifyControl("peer_connected", peerID)

	// Read loop: Peer -> WG Server
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		if val, ok := room.WGPeers.Load(peerID); ok {
			wg := val.(*PeerConn)
			select {
			case wg.Send <- data:
			default:
				// Dropping packets if queue fills to maintain real-time performance
			}
		}
	}

	room.Peers.Delete(peerID)
	p.Close()
	room.NotifyControl("peer_disconnected", peerID)
	log.Println("[PEER] disconnected:", peerID)
}

func wgHandler(w http.ResponseWriter, r *http.Request, roomID, peerID string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	tune(conn)

	room := getRoom(roomID)
	wg := NewPeerConn(conn)

	if _, loaded := room.WGPeers.LoadOrStore(peerID, wg); loaded {
		log.Println("[WG] duplicate rejected:", peerID)
		conn.Close()
		return
	}

	log.Println("[WG] connected:", peerID)
	go writer(wg)

	// Read loop: WG Server -> Peer
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		if val, ok := room.Peers.Load(peerID); ok {
			p := val.(*PeerConn)
			select {
			case p.Send <- data:
			default:
				// Dropping packets if queue fills to maintain real-time performance
			}
		}
	}

	room.WGPeers.Delete(peerID)
	wg.Close()
	log.Println("[WG] disconnected:", peerID)
}

// =========================
// ROUTER & MAIN
// =========================

func routerHandler(w http.ResponseWriter, r *http.Request) {
	// Clean path split avoiding strings.Contains bugs
	// Paths expected:
	//   /ws/{room}/control
	//   /ws/{room}/wgserver/{peerID}
	//   /ws/{room}/{peerID}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "ws" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	roomID := parts[1]

	if parts[2] == "control" {
		controlHandler(w, r, roomID)
		return
	}

	if parts[2] == "wgserver" && len(parts) == 4 {
		peerID := parts[3]
		wgHandler(w, r, roomID, peerID)
		return
	}

	if len(parts) == 3 {
		peerID := parts[2]
		peerHandler(w, r, roomID, peerID)
		return
	}

	http.Error(w, "Bad Request", http.StatusBadRequest)
}

func main() {
	// Define the dynamic port parameter (Defaults to 8080 if not passed)
	port := flag.Int("port", 8080, "Port interface for the public relay to bind onto")
	flag.Parse()

	// 1. Map your existing multiplexer router handler
	http.HandleFunc("/ws/", routerHandler)

	// 2. Safely configure the listener server matrix
	addr := fmt.Sprintf(":%d", *port)
	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 5 * time.Second, // Protect against Slowloris attacks
	}

	log.Printf("Safe bidirectional relay running on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
