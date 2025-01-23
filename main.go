package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

// ChatRequest represents the structure of incoming chat messages
type ChatRequest struct {
	Type       string `json:"type"`
	SenderID   string `json:"senderID"`
	OwnerID    string `json:"ownerID"`
	SenderName string `json:"senderName"`
	Data       struct {
		Content string `json:"content"`
		URL     string `json:"url"`
		Image   string `json:"image"`
	} `json:"data"`
}

// ClientManager manages WebSocket connections
type ClientManager struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mutex      sync.RWMutex
	limiter    *semaphore.Weighted
}

// Client represents a WebSocket client connection
type Client struct {
	socket    *websocket.Conn
	send      chan []byte
	partnerID string
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Adjust origin checking in production
	},
}

// JWT claims for authentication
type Claims struct {
	PartnerID string `json:"partner_id"`
	jwt.RegisteredClaims
}

func NewClientManager() *ClientManager {
	return &ClientManager{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 1000),
		register:   make(chan *Client, 1000),
		unregister: make(chan *Client, 1000),
		limiter:    semaphore.NewWeighted(1000000), // Support 1 million concurrent connections
	}
}

// Start manages client connections and broadcasting
func (manager *ClientManager) Start() {
	for {
		select {
		case client := <-manager.register:
			manager.mutex.Lock()
			manager.clients[client] = true
			manager.mutex.Unlock()

		case client := <-manager.unregister:
			manager.mutex.Lock()
			if _, ok := manager.clients[client]; ok {
				delete(manager.clients, client)
				close(client.send)
			}
			manager.mutex.Unlock()

		case message := <-manager.broadcast:
			manager.mutex.RLock()
			for client := range manager.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(manager.clients, client)
				}
			}
			manager.mutex.RUnlock()
		}
	}
}

// HandleWebSocket manages individual WebSocket connections
func (manager *ClientManager) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Acquire connection semaphore
	err := manager.limiter.Acquire(r.Context(), 1)
	if err != nil {
		http.Error(w, "Too many connections", http.StatusTooManyRequests)
		return
	}
	defer manager.limiter.Release(1)

	// Extract token and validate
	tokenString := extractBearerToken(r)
	claims, err := validateToken(tokenString)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	// Upgrade connection
	socket, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade error: %v", err)
		return
	}

	client := &Client{
		socket:    socket,
		send:      make(chan []byte, 256),
		partnerID: claims.PartnerID,
	}

	manager.register <- client

	// Start read and write goroutines
	go client.writePump()
	go client.readPump(manager)
}

// Extract bearer token from Authorization header
func extractBearerToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
		return authHeader[7:]
	}
	return ""
}

// Validate JWT token and extract claims
func validateToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		// Replace with your actual secret key
		return []byte("your-secret-key"), nil
	})

	if err != nil || !token.Valid {
		return nil, err
	}

	return claims, nil
}

// Read messages from WebSocket
func (c *Client) readPump(manager *ClientManager) {
	defer func() {
		manager.unregister <- c
		c.socket.Close()
	}()

	for {
		_, message, err := c.socket.ReadMessage()
		if err != nil {
			break
		}

		var chatRequest ChatRequest
		if err := json.Unmarshal(message, &chatRequest); err != nil {
			log.Printf("Invalid message format: %v", err)
			continue
		}

		// Validate and process chat request
		chatRequest.OwnerID = c.partnerID

		// Broadcast processed message
		broadcastMessage, _ := json.Marshal(chatRequest)
		manager.broadcast <- broadcastMessage
	}
}

// Write messages to WebSocket
func (c *Client) writePump() {
	ticker := time.NewTicker(ping)
	defer func() {
		ticker.Stop()
		c.socket.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				c.socket.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.socket.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.socket.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.socket.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

var (
	writeWait = 10 * time.Second
	ping      = (writeWait * 9) / 10
)

func main() {
	// Configure logging
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.SetOutput(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})

	// Create client manager
	manager := NewClientManager()
	go manager.Start()

	// WebSocket endpoint
	http.HandleFunc("/ws", manager.HandleWebSocket)

	// Start server
	server := &http.Server{
		Addr:         ":8081",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("Server starting on port 8081")
	log.Fatal(server.ListenAndServe())
}
