package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"crypto/rand"
    "math/big"
	"github.com/gorilla/websocket"
)

// Active tracking structure for server mode workers
var activePeers sync.Map // Map[string]context.CancelFunc

// Helper to determine ws vs wss scheme matching the base URI
func getWebSocketURL(relayURI, path string) (string, error) {
	u, err := url.Parse(relayURI)
	if err != nil {
		return "", err
	}
	scheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, path), nil
}

// ============================================================================
// SERVER MODE LOGIC
// ============================================================================

func runServerPeerPipeline(ctx context.Context, relayURI, room, peerID string, wgIP string, wgPort int) {
	log.Printf("[TUNNEL-SERVER][%s] Allocating isolated tunnel worker...", peerID)

	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", wgIP, wgPort))
	if err != nil {
		log.Printf("[UDP ERROR][%s] Failed to resolve target core address: %v", peerID, err)
		return
	}

	// DialUDP creates an outbound connected socket. The OS assigns a random local ephemeral port.
	udpConn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		log.Printf("[UDP ERROR][%s] Failed to allocate kernel socket: %v", peerID, err)
		return
	}
	defer udpConn.Close()

	wsURL, err := getWebSocketURL(relayURI, fmt.Sprintf("/ws/%s/wgserver/%s", room, peerID))
	if err != nil {
		log.Printf("[URI ERROR][%s] Bad relay path parameters: %v", peerID, err)
		return
	}

	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("[WS ERROR][%s] Connection to relay failed: %v", peerID, err)
		return
	}
	defer wsConn.Close()

	innerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-innerCtx.Done()
		wsConn.Close()
		udpConn.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// Stream 1: Go Relay (WS) ──► UDP Server (UDP)
	go func() {
		defer func() { cancel(); wg.Done() }()
		for {
			msgType, payload, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage {
				_, err = udpConn.Write(payload)
				if err != nil {
					return
				}
			}
		}
	}()

	// Stream 2: UDP Server ──► Go Relay (WS)
	go func() {
		defer func() { cancel(); wg.Done() }()
		buffer := make([]byte, 65535)
		for {
			n, err := udpConn.Read(buffer)
			if err != nil {
				return
			}
			if n > 0 {
				err = wsConn.WriteMessage(websocket.BinaryMessage, buffer[:n])
				if err != nil {
					return
				}
			}
		}
	}()

	log.Printf("[TUNNEL-SERVER][%s] Route active and processing data frames.", peerID)
	wg.Wait()
	log.Printf("[TUNNEL-SERVER][%s] Route torn down successfully.", peerID)
}

func runServerControlPlane(ctx context.Context, relayURI, room string, wgIP string, wgPort int) {
	controlURL, err := getWebSocketURL(relayURI, fmt.Sprintf("/ws/%s/control", room))
	if err != nil {
		log.Fatalf("[FATAL] Base relay configuration mapping failure: %v", err)
	}

	// Define the explicit timeout boundary for the control line.
	// Must be safely larger than the Relay's 30-second ping interval.
	const controlPongWait = 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Printf("[CONTROL] Connecting to control engine: %s", controlURL)
		ws, _, err := websocket.DefaultDialer.Dial(controlURL, nil)
		if err != nil {
			log.Printf("[CONTROL ERROR] Connection rejected: %v. Re-trying in 5s...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("[CONTROL] Connection verified. Now monitoring client entries...")

		// --- NEW: Register Ping Interception & Deadline Tracking ---
		_ = ws.SetReadDeadline(time.Now().Add(controlPongWait))
		ws.SetPingHandler(func(appData string) error {
			// Push the read deadline forward immediately upon catching a fresh ping frame
			_ = ws.SetReadDeadline(time.Now().Add(controlPongWait))

			// Generate the mandatory underlying Pong echo response back to the Relay
			return ws.WriteMessage(websocket.PongMessage, []byte(appData))
		})
		// ───────────────────────────────────────────────────────────

		// Inner frame loop handles control message processing streams
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				log.Printf("[CONTROL ERROR] Connection severed: %v. Resetting mapping states...", err)
				break
			}

			// --- NEW: Reset Read Deadline on standard incoming control TextMessages too ---
			_ = ws.SetReadDeadline(time.Now().Add(controlPongWait))

			parts := strings.Split(string(msg), " ")
			if len(parts) != 2 {
				continue
			}

			event, peerID := parts[0], parts[1]
			log.Printf("[CONTROL EVENT] Action detected: %s for peer %s", event, peerID)

			if event == "peer_connected" {
				if oldCancel, exists := activePeers.Load(peerID); exists {
					oldCancel.(context.CancelFunc)()
				}

				peerCtx, peerCancel := context.WithCancel(ctx)
				activePeers.Store(peerID, peerCancel)

				go func(pID string, pCtx context.Context) {
					runServerPeerPipeline(pCtx, relayURI, room, pID, wgIP, wgPort)
					activePeers.Delete(pID)
				}(peerID, peerCtx)

			} else if event == "peer_disconnected" {
				if cancelFunc, exists := activePeers.Load(peerID); exists {
					cancelFunc.(context.CancelFunc)()
					activePeers.Delete(peerID)
				}
			}
		}

		// Clear out all running child routing worker sub-routines on control link failure
		activePeers.Range(func(key, value interface{}) bool {
			value.(context.CancelFunc)()
			return true
		})
		ws.Close()
		time.Sleep(2 * time.Second)
	}
}

// ============================================================================
// CLIENT MODE LOGIC
// ============================================================================

func runClientPipeline(ctx context.Context, relayURI, room, basePeerID string, listenIP string, listenPort int) {
        log.Printf("[TUNNEL-CLIENT] Spinning up local listener interface on %s:%d...", listenIP, listenPort)

        localAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", listenIP, listenPort))
        if err != nil {
                log.Fatalf("[FATAL] Failed to resolve local listener constraints: %v", err)
        }

        udpConn, err := net.ListenUDP("udp", localAddr)
        if err != nil {
                log.Fatalf("[FATAL] Failed to open local listener socket: %v", err)
        }
        defer udpConn.Close()

        // Infinite loop handles drops and connection retries
        for {
                select {
                case <-ctx.Done():
                        log.Println("[TUNNEL-CLIENT] Stopping client pipeline due to context cancellation.")
                        return
                default:
                }

                // 1. Generate a cryptographically secure 6-digit random number (100000 - 999999)
                nBig, err := rand.Int(rand.Reader, big.NewInt(900000))
                if err != nil {
                        log.Printf("[TUNNEL-CLIENT ERROR] Failed to generate random number: %v. Retrying...", err)
                        time.Sleep(1 * time.Second)
                        continue
                }
                randomToken := nBig.Int64() + 100000

                // 2. Combine the base peerID with the new 6-digit random token
                currentPeerID := fmt.Sprintf("%s-%d", basePeerID, randomToken)

                // 3. Re-build the fresh WebSocket URL route inside the loop
                wsURL, err := getWebSocketURL(relayURI, fmt.Sprintf("/ws/%s/%s", room, currentPeerID))
                if err != nil {
                        log.Fatalf("[FATAL] Bad client route generation parameters: %v", err) // Kept fatal because bad URI structure won't fix itself
                }

                log.Printf("[TUNNEL-CLIENT] Establishing outbound WebSocket proxy bridge to %s", wsURL)
                wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
                if err != nil {
                        log.Printf("[TUNNEL-CLIENT ERROR] Relay handshake connection failure for peer [%s]: %v. Retrying in 5s...", currentPeerID, err)
                        time.Sleep(5 * time.Second)
                        continue
                }

                log.Printf("[TUNNEL-CLIENT] Client shim routing pipeline fully operational for peer [%s].", currentPeerID)

                // Lifecycle context for the running sockets
                connCtx, cancelConn := context.WithCancel(ctx)

                go func() {
                        <-connCtx.Done()
                        wsConn.Close()
                }()

                var localAppAddr *net.UDPAddr
                var addrLock sync.RWMutex
                var wg sync.WaitGroup
                wg.Add(2)

                // Stream 1: Local client Application (UDP) ──► Go Relay (WS)
                go func() {
                        defer func() { cancelConn(); wg.Done() }()
                        buffer := make([]byte, 65535)
                        for {
                                _ = udpConn.SetReadDeadline(time.Now().Add(1 * time.Second))
                                n, remoteAddr, err := udpConn.ReadFromUDP(buffer)
                                if err != nil {
                                        if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
                                                select {
                                                case <-connCtx.Done():
                                                        return
                                                default:
                                                        continue
                                                }
                                        }
                                        return
                                }
                                if n > 0 {
                                        addrLock.Lock()
                                        localAppAddr = remoteAddr
                                        addrLock.Unlock()

                                        err = wsConn.WriteMessage(websocket.BinaryMessage, buffer[:n])
                                        if err != nil {
                                                log.Printf("[TUNNEL-CLIENT WS WRITE ERROR]: %v", err)
                                                return
                                        }
                                }
                        }
                }()

                // Stream 2: Go Relay (WS) ──► Local client Application (UDP)
                go func() {
                        defer func() { cancelConn(); wg.Done() }()
                        for {
                                msgType, payload, err := wsConn.ReadMessage()
                                if err != nil {
                                        log.Printf("[TUNNEL-CLIENT WS READ ERROR]: %v", err)
                                        return
                                }
                                if msgType == websocket.BinaryMessage {
                                        addrLock.RLock()
                                        targetAddr := localAppAddr
                                        addrLock.RUnlock()

                                        if targetAddr != nil {
                                                _ = udpConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
                                                _, err = udpConn.WriteToUDP(payload, targetAddr)
                                                if err != nil {
                                                        return
                                                }
                                        }
                                }
                        }
                }()

                wg.Wait()
                cancelConn()
                wsConn.Close()

                log.Printf("[TUNNEL-CLIENT] Connection lost for peer [%s]. Rolling new peer ID and reconnecting in 3s...", currentPeerID)
                time.Sleep(3 * time.Second)
        }
}

// ============================================================================
// ENTRYPOINT ORCHESTRATION
// ============================================================================

func main() {
	role := flag.String("wg-role", "", "Execution Profile Role Matrix: 'server' or 'client'")
	uri := flag.String("uri", "ws://127.0.0.1:8080", "Base WebSocket Relay URL path target")
	room := flag.String("room", "", "Unique Room Identifier string")
	peerID := flag.String("peer-id", "", "Unique Peer Identification label (Required for client role)")
	wgIP := flag.String("wg-ip", "127.0.0.1", "Backend host network destination or binding point")
	wgPort := flag.Int("wg-port", 51820, "Target Application mapping port element")
	flag.Parse()

	// Verify global constraint requirements
	if *role != "server" && *role != "client" {
		log.Fatal("[INIT ERROR] Missing validation context: '--wg-role' flag must be set to 'server' or 'client'")
	}
	if *room == "" {
		log.Fatal("[INIT ERROR] Missing parameter constraint: '--room' flag is explicitly required")
	}

	// Setup clean system teardown interrupt signals
	ctx, cancel := context.WithCancel(context.Background())
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-shutdownChan
		log.Println("\n[SYSTEM] Termination signal caught. Revoking context structures...")
		cancel()
	}()

	if *role == "server" {
		log.Printf("[INIT] Spawning MULTI-PEER SERVER Core for Room ID: %s", *room)
		runServerControlPlane(ctx, *uri, *room, *wgIP, *wgPort)
	} else {
		if *peerID == "" {
			log.Fatal("[INIT ERROR] Profile mismatched: '--peer-id' flag is required when setting client operations")
		}
		log.Printf("[INIT] Spawning CLIENT listener proxy for Peer ID [%s] in Room [%s]", *peerID, *room)
		runClientPipeline(ctx, *uri, *room, *peerID, *wgIP, *wgPort)
	}

	// Ensure subroutines have time to log resource cleanup tasks cleanly before exiting
	time.Sleep(200 * time.Millisecond)
}
