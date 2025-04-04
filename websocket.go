package main

import (
	"cloud.google.com/go/firestore"
	"context"
	"encoding/json"
	firebase "firebase.google.com/go"
	"fmt"
	"github.com/gorilla/websocket"
	"google.golang.org/api/option"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	gameDuration = 2 * time.Minute
	targetValue  = 100
)

// Player represents a connected client.
type Player struct {
	UID        string
	Conn       *websocket.Conn
	Opponent   *Player
	Puzzle     string
	RoomID     string
	opponentID string
	Timer      *time.Timer // Individual timer (can be removed if using a central room timer)
	Submitted  bool
}

// MatchRequest represents the initial matchmaking request from the client.
type MatchRequest struct {
	UID string `json:"uid"`
}
type MessageSpec struct {
	Type   string `json:"type"` // Should be "identify"
	UID    string `json:"uid"`
	RoomID string `json:"room_id"`
}

// Message represents a message sent between the server and the client.
type Message struct {
	Type       string `json:"type"`
	Content    string `json:"content"`
	RoomID     string `json:"room_id,omitempty"`
	Opponent   string `json:"opponent,omitempty"`
	Player     string `json:"player,omitempty"`
	Expression string `json:"expression,omitempty"` // Add expression field
}

type SubmitRequest struct {
	Type          string `json:"type"`
	Expression    string `json:"answer"`
	UID           string `json:"uid"`
	RawExpression string `json:"expression"`
	RoomID        string `json:"room_id"`
}

// Room represents an active game room.
type Room struct {
	Player1    *Player
	Player2    *Player
	Puzzle     string
	StartTime  time.Time
	Timer      *time.Timer
	Spectators []*Player
}

func initFirebase() {
	ctx := context.Background()
	// **Important:** Replace "path/to/serviceAccountKey.json" with the actual path to your Firebase service account key file.
	opt := option.WithCredentialsFile("/home/ubuntu/hectoclash-e0c38-firebase-adminsdk-fbsvc-e247d71dd0.json")
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		log.Fatalf("Error initializing Firebase app: %v", err)
	}

	client, err := app.Firestore(ctx)
	if err != nil {
		log.Fatalf("Error initializing Firestore client: %v", err)
	}

	firebaseApp = app
	dbClient = client
	log.Println("Firebase initialized successfully.")
}

// handleConnection handles the WebSocket connection for a new client.
func handleConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket Upgrade Failed:", err)
		return
	}
	defer conn.Close()
	log.Println("Client connected.")

	player := &Player{Conn: conn}

	_, p, err := conn.ReadMessage()
	if err != nil {
		log.Printf("Error reading initial message: %v", err)
		handleDisconnection(player)
		return
	}
	var msg MessageSpec
	if err := json.Unmarshal(p, &msg); err != nil {
		log.Printf("Error unmarshalling message: %v", err)
		sendError(conn, "Invalid message format")
		return
	}

	if msg.Type == "requestRoomList" {
		log.Println("Handling room list request.")
		sendRoomList(conn)
		return
	}
	if msg.Type == "spectateRoom" {
		log.Println("Handling spectateRoom request.")
		if msg.UID == "" || msg.RoomID == "" {
			sendError(conn, "Spectator UID and RoomID required")
			return
		}
		player = &Player{Conn: conn, UID: msg.UID}
		handleSpectateRoom(conn, player, msg.RoomID)
		return
	}

	var req MatchRequest
	if err := json.Unmarshal(p, &req); err != nil {
		log.Printf("Error unmarshalling MatchRequest: %v", err)
		sendError(conn, "Invalid MatchRequest format")
		return
	}

	if req.UID == "" {
		log.Println("Received empty UID. Rejecting connection.")
		sendError(conn, "UID cannot be empty")
		return
	}
	if isPlayerActive(player.UID) {
		log.Printf("Player %s is already active (in queue or room). Rejecting new connection.", player.UID)
		sendError(conn, "You are already in matchmaking or in a game.")
		return
	}
	player.UID = req.UID
	log.Printf("Created player: %s", player.UID)

	mutex.Lock()

	// Check if the player is already in a room or queue
	log.Printf("Current players in queue: %d", len(playersQueue))
	if len(playersQueue) > 0 {
		opponent := playersQueue[0]
		playersQueue = playersQueue[1:]
		log.Printf("Found opponent: %s", opponent.UID)
		if player.UID == opponent.UID {
			log.Printf("Attempted to match player %s with themselves. Re-queuing opponent.", player.UID)
			playersQueue = append(playersQueue, player)   // Re-queue the current player
			playersQueue = append(playersQueue, opponent) // Re-queue the opponent
			sendError(conn, "Could not find a suitable opponent at this time. Please wait.")
			return
		}
		roomID := fmt.Sprintf("room-%d", rand.Intn(10000))
		player.Opponent = opponent
		opponent.Opponent = player
		player.RoomID = roomID
		opponent.RoomID = roomID
		puzzle := generatePuzzle()
		player.Puzzle = puzzle
		opponent.Puzzle = puzzle
		player.opponentID = opponent.UID
		opponent.opponentID = player.UID

		room := &Room{
			Player1:   player,
			Player2:   opponent,
			Puzzle:    puzzle,
			StartTime: time.Now(),
		}
		startRoomTimer(room)
		rooms[roomID] = room // Store the Room struct
		mutex.Unlock()

		startGame(player, opponent, puzzle, roomID)
	} else {
		log.Printf("Player queued for matchmaking: %s", req.UID)
		playersQueue = append(playersQueue, player)
		mutex.Unlock()
	}
	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message for player %s: %v", player.UID, err)

			// Lock before modifying shared data
			mutex.Lock()
			room, roomExists := rooms[player.RoomID]
			mutex.Unlock()

			if roomExists {
				log.Printf("Closing room %s because player %s disconnected.", player.RoomID, player.UID)
				closeRoom(room, fmt.Sprintf("Opponent Left %s", player.UID))
			}

			handleDisconnection(player)
			return
		}
		log.Printf("Received message: %s", string(msgBytes))

		var msg Message
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			log.Printf("Error unmarshalling message: %v", err)
			continue
		}

		switch msg.Type {
		case "expressionUpdate":
			if room, ok := rooms[msg.RoomID]; ok {
				if room.Player1.UID == player.UID && room.Player2 != nil {
					safeSend(room.Player2.Conn, msgBytes)
				} else if room.Player2.UID == player.UID && room.Player1 != nil {
					safeSend(room.Player1.Conn, msgBytes)
				}
			}
		case "submit":
			var submitReq SubmitRequest
			if err := json.Unmarshal(msgBytes, &submitReq); err == nil {
				handleSubmission(player, submitReq.Expression, submitReq.RawExpression, submitReq.RoomID)
			}
		default:
			log.Printf("Unknown message type: %s", msg.Type)
		}
	}

}
func cleanAllRoomsAndQueue() {
	mutex.Lock()
	defer mutex.Unlock()

	log.Println("--- Starting forced cleanup of all rooms and queue ---")

	// Clear the players queue
	numPlayersInQueue := len(playersQueue)
	playersQueue = []*Player{}
	log.Printf("Cleared %d players from the matchmaking queue.", numPlayersInQueue)

	// Close and clear all active rooms
	numRooms := len(rooms)
	for roomID, room := range rooms {
		log.Printf("Forcefully closing room: %s", roomID)
		if room.Timer != nil {
			room.Timer.Stop()
			log.Printf("Stopped timer for room: %s", roomID)
		}

		if room.Player1 != nil && room.Player1.Conn != nil {
			if err := room.Player1.Conn.Close(); err != nil {
				log.Printf("Error closing connection for player %s in room %s: %v", room.Player1.UID, roomID, err)
			} else {
				log.Printf("Closed connection for player %s in room %s.", room.Player1.UID, roomID)
			}
		}
		if room.Player2 != nil && room.Player2.Conn != nil {
			if err := room.Player2.Conn.Close(); err != nil {
				log.Printf("Error closing connection for player %s in room %s: %v", room.Player2.UID, roomID, err)
			} else {
				log.Printf("Closed connection for player %s in room %s.", room.Player2.UID, roomID)
			}
		}
	}
	rooms = make(map[string]*Room) // Create a new empty map to clear all rooms
	log.Printf("Closed and cleared %d active rooms.", numRooms)

	log.Println("--- Forced cleanup of all rooms and queue completed ---")
}

func handleSpectateRoom(conn *websocket.Conn, spectator *Player, roomID string) {
	mutex.Lock()
	defer mutex.Unlock()

	room, ok := rooms[roomID]
	if !ok {
		sendError(conn, "Room not found")
		return
	}

	if room.Player1 == nil || room.Player2 == nil {
		sendError(conn, "Room is incomplete")
		return
	}

	// Send initial expressions (if any)
	if room.Player1 != nil {
		sendExpressionUpdate(conn, room.Player1.UID, room.Player1.RoomID, room.Player1.Puzzle)
	}
	if room.Player2 != nil {
		sendExpressionUpdate(conn, room.Player2.UID, room.Player2.RoomID, room.Player2.Puzzle)
	}

	// Store spectator in the room for future updates
	room.Spectators = append(room.Spectators, spectator)
}
func isPlayerActive(uid string) bool {
	// Check queue
	for _, p := range playersQueue {
		if p.UID == uid {
			return true
		}
	}

	// Check rooms
	for _, room := range rooms {
		if (room.Player1 != nil && room.Player1.UID == uid) || (room.Player2 != nil && room.Player2.UID == uid) {
			return true
		}
	}
	return false
}
func sendRoomList(conn *websocket.Conn) {
	mutex.Lock()
	log.Printf("sendroom list")
	roomList := make(map[string]map[string]string)
	for id, r := range rooms {
		roomInfo := make(map[string]string)
		if r.Player1 != nil {
			roomInfo["Player1"] = r.Player1.UID
		} else {
			roomInfo["Player1"] = ""
		}
		if r.Player2 != nil {
			roomInfo["Player2"] = r.Player2.UID
		} else {
			roomInfo["Player2"] = ""
		}
		roomList[id] = roomInfo
	}
	mutex.Unlock()

	roomListJSON, _ := json.Marshal(roomList)
	safeSend(conn, []byte(`{"type": "roomList", "content": `+string(roomListJSON)+`}`))
}
func startGame(p1, p2 *Player, puzzle string, roomID string) {
	msg := Message{Type: "start", Content: puzzle, RoomID: roomID, Opponent: p1.opponentID, Player: p2.opponentID}
	jsonMsg, _ := json.Marshal(msg)

	if err := safeSend(p1.Conn, jsonMsg); err != nil {
		handleDisconnection(p1)
		return
	}
	if err := safeSend(p2.Conn, jsonMsg); err != nil {
		handleDisconnection(p2)
		return
	}
}
func startRoomTimer(room *Room) {
	room.Timer = time.AfterFunc(gameDuration, func() {
		closeRoom(room, "time limit reached")
	})
	log.Printf("Room %s timer started for %v", room.Player1.RoomID, gameDuration)
}
func closeRoom(room *Room, reason string) {
	mutex.Lock()
	defer mutex.Unlock()

	roomID := room.Player1.RoomID
	p1 := room.Player1
	p2 := room.Player2

	if _, ok := rooms[roomID]; !ok {
		log.Printf("Room %s already closed or doesn't exist.", roomID)
		return
	}

	log.Printf("Closing room %s: %s", roomID, reason)

	// Determine winner based on submission status or lack thereof
	if strings.HasPrefix(reason, "Opponent Left") {
		// Extract the UID of the player who left
		left := strings.TrimPrefix(reason, "Opponent Left ")
		if left == p2.UID {
			go updatePlayerRating(p1.UID, 50)
			go updatePlayerRating(p2.UID, -50)
			sendResult(p1, "You Won !!", "Opponent Left (+50)")
		} else if left == p1.UID {
			go updatePlayerRating(p2.UID, 50)
			go updatePlayerRating(p1.UID, -50)
			sendResult(p2, "You Won !!", "(Opponent Left) (+50)")
		}

	} else {
		if p1 != nil && p2 != nil {
			if p1.Submitted && !p2.Submitted {
				declareWinnerInternal(p1, p2, "opponent timed out")
			} else if !p1.Submitted && p2.Submitted {
				declareWinnerInternal(p2, p1, "opponent timed out")
			} else {
				// If neither submitted or both submitted (without further logic), declare a draw or no winner.
				updatePlayerRating(p1.UID, -10)
				updatePlayerRating(p2.UID, -10)
				sendResult(p1, "Game Over!", "Time limit reached - No clear winner. (-10)")
				sendResult(p2, "Game Over!", "Time limit reached - No clear winner. (-10)")

			}
		} else if p1 != nil {
			sendResult(p1, "Game Over!", reason)
		} else if p2 != nil {
			sendResult(p2, "Game Over!", reason)
		}
	}

	// Clean up
	if p1 != nil {
		p1.Opponent = nil
		p1.RoomID = ""
	}
	if p2 != nil {
		p2.Opponent = nil
		p2.RoomID = ""
	}
	delete(rooms, roomID)
	log.Printf("Room %s closed.", roomID)
}
func handleSubmission(player *Player, expression string, rawexpression string, roomID string) {
	room, ok := rooms[roomID]
	if !ok || (room.Player1 != player && room.Player2 != player) {
		log.Printf("Invalid room or player for submission in room: %s, player: %s", roomID, player.UID)
		return
	}
	rawexpressionStr := rawexpression

	submittedAnswerStr := expression // 'expression' now holds the 'answer' string from Kotlin
	submittedAnswer, err := strconv.ParseFloat(submittedAnswerStr, 64)
	if err != nil {
		log.Printf("Error parsing submitted answer '%s': %v", submittedAnswerStr, err)
		sendFeedback(player.Conn, "Invalid answer format.")
		return
	}

	if submittedAnswer == float64(targetValue) {
		log.Printf("Player %s submitted correct answer %f in room %s", player.UID, submittedAnswer, roomID)
		sendFeedback(player.Conn, "You Won (Solved) (+50)")
		updatePlayerRating(player.UID, 50)
		declareWinner(player, "Solved", rawexpressionStr)
	} else {
		log.Printf("Player %s submitted incorrect answer %f in room %s", player.UID, submittedAnswer, roomID)
		sendFeedback(player.Conn, fmt.Sprintf("Incorrect. Your answer: %f", submittedAnswer))
	}
}
func handleDisconnection(player *Player) {
	mutex.Lock()
	defer mutex.Unlock()
	log.Printf("--- Handling disconnection for player: %s, RoomID: %s ---", player.UID, player.RoomID)

	// Check if in queue
	isQueued := false
	for i, p := range playersQueue {
		if p == player {
			playersQueue = append(playersQueue[:i], playersQueue[i+1:]...)
			log.Printf("Player %s REMOVED from queue.", player.UID)
			isQueued = true
			break
		}
	}
	if isQueued {
		player.Opponent = nil
		player.RoomID = ""
		log.Printf("Disconnection handled for player %s (was in queue).", player.UID)
		return
	}

	// Check if in a room
	if player.RoomID != "" {
		if room, ok := rooms[player.RoomID]; ok {
			log.Printf("Player %s WAS in room: %s", player.UID, player.RoomID)
			var opponent *Player
			if room.Player1 == player {
				opponent = room.Player2
			} else if room.Player2 == player {
				opponent = room.Player1
			}

			if opponent != nil {
				log.Printf("Opponent %s FOUND in room %s with disconnected player %s.", opponent.UID, player.RoomID, player.UID)
				declareWinner(opponent, "opponent disconnected", "0.0") // Rely on declareWinner for cleanup
			} else {
				log.Printf("Opponent NOT FOUND in room %s for disconnected player %s (potential issue).", player.RoomID, player.UID)
				delete(rooms, player.RoomID) // Clean up the room if no opponent
				log.Printf("Room %s DELETED because opponent not found.", player.RoomID)
			}
			player.Opponent = nil
			player.RoomID = ""
			log.Printf("Disconnection handled for player %s (was in room).", player.UID)
			return
		} else {
			log.Printf("Room %s NOT FOUND for disconnected player %s (inconsistent state!).", player.RoomID, player.UID)
			player.Opponent = nil
			player.RoomID = ""
		}
	}

	log.Printf("Disconnection handling COMPLETED for player %s.", player.UID)
}
func sendExpressionUpdate(conn *websocket.Conn, uid string, roomID string, expression string) {
	msg := Message{
		Type:     "expressionUpdate",
		Content:  expression,
		RoomID:   roomID,
		Opponent: uid, //Use opponent to send the UID of the player whose expression is being sent.
	}
	jsonMsg, _ := json.Marshal(msg)
	safeSend(conn, jsonMsg)
}
func generatePuzzle() string {
	digits := ""
	for i := 0; i < 6; i++ {
		digits += fmt.Sprintf("%d", rand.Intn(9)+1) // Digits 1-9
	}
	return digits
}
func safeSend(conn *websocket.Conn, msg []byte) error {
	if conn == nil {
		return fmt.Errorf("connection is nil")
	}
	return conn.WriteMessage(websocket.TextMessage, msg)
}
func sendError(conn *websocket.Conn, message string) {
	msg := Message{Type: "error", Content: message}
	jsonMsg, _ := json.Marshal(msg)
	if err := safeSend(conn, jsonMsg); err != nil {
		log.Println("Error sending error message:", err)
	}
}

func sendResult(player *Player, title, message string) {
	if player != nil && player.Conn != nil {
		msg := Message{Type: "result", Content: fmt.Sprintf("%s %s", title, message)}
		jsonMsg, _ := json.Marshal(msg)
		if err := safeSend(player.Conn, jsonMsg); err != nil {
			log.Println("Error sending result message:", err)
		}
	}
}

func sendFeedback(conn *websocket.Conn, message string) {
	msg := Message{Type: "feedback", Content: message}
	jsonMsg, _ := json.Marshal(msg)
	if err := safeSend(conn, jsonMsg); err != nil {
		log.Println("Error sending feedback message:", err)
	}
}

func updatePlayerRating(uid string, change int) {
	ctx := context.Background()
	docRef := dbClient.Collection("Users").Doc(uid)

	doc, err := docRef.Get(ctx)
	if err != nil {
		log.Printf("Error getting user %s: %v", uid, err)
		return
	}

	var userData map[string]interface{}
	if err := doc.DataTo(&userData); err != nil {
		log.Printf("Error decoding user data for %s: %v", uid, err)
		return
	}

	currentRating, ok := userData["Rating"].(int64)
	if !ok {
		log.Printf("Error: Rating field not found or not an integer for user %s", uid)
		return
	}

	newRating := currentRating + int64(change)
	if newRating < 0 {
		newRating = 0
	}

	_, err = docRef.Update(ctx, []firestore.Update{
		{Path: "Rating", Value: newRating},
	})

	if err != nil {
		log.Printf("Error updating rating for player %s: %v", uid, err)
	} else {
		log.Printf("Updated rating for player %s from %d to %d", uid, currentRating, newRating)
	}
}

func declareWinnerInternal(winner *Player, loser *Player, reason string) {
	sendResult(winner, "You win!", fmt.Sprintf("(%s) (+50)", reason))
	sendResult(loser, "You lose!", "(-50)")

	// Update Firestore ratings
	go updatePlayerRating(winner.UID, 50)
	go updatePlayerRating(loser.UID, -50)
}

func declareWinner(winner *Player, reason string, expression string) {
	mutex.Lock()
	defer mutex.Unlock()
	log.Printf("--- declareWinner called. Winner: %s, Reason: %s ---", winner.UID, reason)

	roomID := winner.RoomID // Get room ID from winner (should be the same)
	room, ok := rooms[roomID]
	if !ok {
		log.Printf("Room %s NOT FOUND for winner %s in declareWinner (potential issue).", roomID, winner.UID)
		return
	}

	var loser *Player
	if room.Player1 == winner {
		loser = room.Player2
	} else {
		loser = room.Player1
	}

	if loser != nil {
		log.Printf("Loser identified: %s", loser.UID)
		sendResult(loser, "You lose!", fmt.Sprintf("(opponent solved) (-50) \n\n Possible Solution : %s = 100", expression))
		go updatePlayerRating(loser.UID, -50)
		loser.Opponent = nil
		loser.RoomID = ""
		loser.Submitted = false
		safeSend(loser.Conn, []byte(`{"type": "opponent_disconnected"}`)) // Inform client
	} else {
		log.Printf("Loser NOT identified in declareWinner for room %s.", roomID)
	}

	sendResult(winner, "You win!", fmt.Sprintf("(%s) (+50)", reason))
	go updatePlayerRating(winner.UID, 50)
	winner.Opponent = nil
	winner.RoomID = ""
	winner.Submitted = false

	if room.Timer != nil {
		room.Timer.Stop()
		log.Printf("Timer stopped for room %s.", roomID)
	}
	delete(rooms, roomID)
	log.Printf("Room %s CLOSED in declareWinner.", roomID)
	log.Printf("--- declareWinner completed ---")
}
