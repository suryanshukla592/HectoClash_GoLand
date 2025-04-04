package main

import (
	"cloud.google.com/go/firestore"
	"context"
	firebase "firebase.google.com/go"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const port = "8080"

var (
	playersQueue []*Player
	rooms        = make(map[string]*Room)
	mutex        sync.Mutex
	upgrader     = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	firebaseApp *firebase.App
	dbClient    *firestore.Client
)

func main() {
	rand.Seed(time.Now().UnixNano()) // Seed randomness once
	initFirebase()

	server := &http.Server{
		Addr:    "0.0.0.0:" + port,
		Handler: setupRoutes(),
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	go func() {
		log.Println("🚀 Server running on port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("❌ Server Error: %v", err)
		}
	}()

	<-stop
	log.Println("🛑 Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("❌ Server Shutdown Failed: %v", err)
	}

	log.Println("✅ Server exited properly")
}

// Registers HTTP and WebSocket routes
func setupRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("/ws", handleConnection)               // WebSocket route
	mux.HandleFunc("/admin/cleanup", adminCleanupHandler) // Add the admin cleanup route
	return mux
}

// HTTP Handlers
func homeHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("🏠 Welcome to HectoClash!"))
}

// adminCleanupHandler handles the request to clean all rooms and the queue.
func adminCleanupHandler(w http.ResponseWriter, r *http.Request) {
	cleanAllRoomsAndQueue()
	fmt.Fprintln(w, "Cleanup initiated.")
}
