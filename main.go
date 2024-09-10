package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// Upgrader is used to upgrade HTTP connections to WebSocket connections
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Client represents a connected WebSocket client
type Client struct {
	conn *websocket.Conn
	send chan []byte
}

// Hub maintains the set of active clients and broadcasts messages to the clients
type Hub struct {
	clients        map[*Client]bool
	broadcast      chan []byte
	register       chan *Client
	unregister     chan *Client
	users          map[*Client]string
	userList       []string // Store user list order
	usersMutex     sync.Mutex
	availableNames []string
}

// newHub creates a new Hub instance
func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		users:      make(map[*Client]string),
		userList:   make([]string, 0), // Initialize user list
		availableNames: []string{
			"Luffy", "Zoro", "Nami", "Usopp", "Sanji", "Chopper",
			"Robin", "Franky", "Brook",
		},
	}
}

// run handles the main logic of the Hub
func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			username := h.assignUsername(client)
			h.broadcastUserList()
			client.send <- []byte(fmt.Sprintf(`{"type":"username","username":"%s"}`, username))
			// Log the successful username assignment
			log.Printf("Username assigned to client %s: %s", client.conn.RemoteAddr(), username)
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				h.releaseUsername(client)
				close(client.send)
				h.broadcastUserList()
			}
		case message := <-h.broadcast:
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
		}
	}
}

// assignUsername assigns a unique username to a client
func (h *Hub) assignUsername(client *Client) string {
	h.usersMutex.Lock()
	defer h.usersMutex.Unlock()

	// First, try to assign an available basic username
	if len(h.availableNames) > 0 {
		username := h.availableNames[0]
		h.availableNames = h.availableNames[1:]
		h.users[client] = username
		h.userList = append(h.userList, username)
		return username
	}

	// If no basic usernames are available, use a name with a numeric suffix
	baseNames := []string{
		"Luffy", "Zoro", "Nami", "Usopp", "Sanji", "Chopper",
		"Robin", "Franky", "Brook",
	}

	for i := 1; ; i++ {
		for _, baseName := range baseNames {
			newUsername := fmt.Sprintf("%s-%d", baseName, i)
			if !h.isUsernameTaken(newUsername) {
				h.users[client] = newUsername
				h.userList = append(h.userList, newUsername)
				return newUsername
			}
		}
	}
}

// isUsernameTaken checks if a username is already taken
func (h *Hub) isUsernameTaken(username string) bool {
	for _, existingUsername := range h.users {
		if existingUsername == username {
			return true
		}
	}
	return false
}

// releaseUsername releases a username when a client disconnects
func (h *Hub) releaseUsername(client *Client) {
	h.usersMutex.Lock()
	defer h.usersMutex.Unlock()

	if username, ok := h.users[client]; ok {
		// Check if it's a basic username (doesn't contain '-')
		if !strings.Contains(username, "-") {
			h.availableNames = append(h.availableNames, username)
		}
		delete(h.users, client)
		// Remove user from the user list
		for i, name := range h.userList {
			if name == username {
				h.userList = append(h.userList[:i], h.userList[i+1:]...)
				break
			}
		}
	}
}

// broadcastUserList sends the current user list to all connected clients
func (h *Hub) broadcastUserList() {
	userListMsg, _ := json.Marshal(map[string]interface{}{
		"type":  "userList",
		"users": h.userList, // Directly use the ordered user list
	})
	for client := range h.clients {
		client.send <- userListMsg
	}
}

// readPump pumps messages from the WebSocket connection to the hub
func (c *Client) readPump(hub *Hub) {
	defer func() {
		hub.unregister <- c
		c.conn.Close()
		log.Println("Connection closed for client:", hub.users[c])
	}()
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Println("Read error for client", hub.users[c], ":", err)
			break
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Println("JSON Unmarshal error for client", hub.users[c], ":", err)
			continue
		}

		// Ensure the client has a username before processing messages
		if hub.users[c] == "" {
			log.Println("Client has no username assigned yet")
			continue
		}

		switch msg["type"] {
		case "message":
			msgBytes, _ := json.Marshal(map[string]interface{}{
				"type": "message",
				"user": hub.users[c],
				"text": msg["text"],
			})
			hub.broadcast <- msgBytes
		case "requestUserList":
			hub.broadcastUserList()
		case "ping":
			c.send <- []byte(`{"type":"pong"}`)
		}
	}
}

// writePump pumps messages from the hub to the WebSocket connection
func (c *Client) writePump() {
	defer func() {
		c.conn.Close()
		log.Println("Connection closed for client:", c.conn.RemoteAddr())
	}()
	for message := range c.send {
		err := c.conn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			log.Println("Write error for client", c.conn.RemoteAddr(), ":", err)
			break
		}
	}
}

// serveWs handles WebSocket requests from the peer
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	client := &Client{conn: conn, send: make(chan []byte, 256)}
	hub.register <- client

	// Log the successful connection
	log.Printf("New client connected from %s", conn.RemoteAddr())

	go client.writePump()
	client.readPump(hub)
}

func main() {
	// Define command line flags
	port := flag.String("port", "8080", "Port number")
	useSSL := flag.Bool("ssl", false, "Use SSL")
	certFile := flag.String("cert", "", "SSL certificate file path")
	keyFile := flag.String("key", "", "SSL private key file path")

	// Parse command line flags
	flag.Parse()

	hub := newHub()
	go hub.run()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	addr := ":" + *port
	if *useSSL {
		if *certFile == "" || *keyFile == "" {
			log.Fatal("Certificate file and private key file must be specified when using SSL")
		}
		fmt.Printf("HTTPS server started, listening on port %s\n", *port)
		log.Fatal(http.ListenAndServeTLS(addr, *certFile, *keyFile, nil))
	} else {
		fmt.Printf("HTTP server started, listening on port %s\n", *port)
		log.Fatal(http.ListenAndServe(addr, nil))
	}
}
