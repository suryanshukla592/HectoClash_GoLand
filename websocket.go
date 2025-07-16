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
	"os/exec"
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
	UID         string
	Conn        *websocket.Conn
	Opponent    *Player
	Puzzle      string
	RoomID      string
	Submissions int64
	opponentID  string
	Timer       *time.Timer // Individual timer (can be removed if using a central room timer)
	Submitted   bool
}

// MatchRequest represents the initial matchmaking request from the client.
type MatchRequest struct {
    UID string `json:"uid"`
    Code string `json:"code"` // New field for private room code
}
type MessageSpec struct {
	Type   string `json:"type"` // Should be "identify"
	UID    string `json:"uid"`
	RoomID string `json:"room_id"`
}
type MatchHistory struct {
	SelfUID     string `firestore:"selfUID"`
	OpponentUID string `firestore:"opponentUID"`
	Timestamp   int64  `firestore:"timestamp"`
	Puzzle      string `firestore:"puzzle"`
	Result      string `firestore:"result"` // "win", "lose", or "draw"
	Feedback    string `firestore:"feedback"`
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
	Expr1      string // Store Player 1's current expression
	Expr2      string // Store Player 2's current expression
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
			go func() {
			defer conn.Close()
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					log.Printf("Spectator %s disconnected: %v", player.UID, err)
					mutex.Lock()
					room, exists := rooms[msg.RoomID]
					if exists {
						newSpectators := []*Player{}
						for _, spec := range room.Spectators {
							if spec.UID != player.UID {
								newSpectators = append(newSpectators, spec)
							}
						}
						room.Spectators = newSpectators
					}
					mutex.Unlock()
					break
				}
			}
		}()
		select {}
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
		conn.Close()
		return
	}
	player.UID = req.UID
	log.Printf("Created player: %s", player.UID)

	mutex.Lock()

	if req.Code == "" || req.Code == "default" { // Public matchmaking
		log.Printf("Current players in public queue: %d", len(playersQueue))
		if len(playersQueue) > 0 {
			opponent := playersQueue[0]
			playersQueue = playersQueue[1:]
			log.Printf("Found opponent: %s", opponent.UID)
			if player.UID == opponent.UID {
				log.Printf("Attempted to match player %s with themselves. Skipping self-match.", player.UID)
				playersQueue = append(playersQueue, player) // only add back once
				sendError(conn, "Could not find a suitable opponent at this time. Please wait.")
				mutex.Unlock()
				return
			}

			roomID := fmt.Sprintf("room-%d", rand.Intn(10000))
			player.Opponent = opponent
			opponent.Opponent = player
			player.RoomID = roomID
			player.Submissions = 0
			opponent.Submissions = 0
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
				Expr1:     "", // Initialize expressions
				Expr2:     "",
			}
			startRoomTimer(room)
			rooms[roomID] = room // Store the Room struct
			mutex.Unlock()       // Unlock after modifying shared data

			startGame(player, opponent, puzzle, roomID)
		} else {
			log.Printf("Player queued for public matchmaking: %s", req.UID)
			playersQueue = append(playersQueue, player)
			mutex.Unlock() // Unlock after modifying shared data
		}
	} else { // Private matchmaking
		log.Printf("Player %s is requesting private match with code: %s", req.UID, req.Code)
		privateQueue := privateQueues[req.Code]

		foundOpponent := false
		for i, opponent := range privateQueue {
			if opponent.UID != player.UID { // Ensure not matching with self
				// Found an opponent in the private queue for this code
				privateQueues[req.Code] = append(privateQueue[:i], privateQueue[i+1:]...) // Remove opponent from queue

				log.Printf("Found private opponent %s for player %s with code %s.", opponent.UID, player.UID, req.Code)

				roomID := fmt.Sprintf("private-room-%s-%d", req.Code, rand.Intn(10000))
				player.Opponent = opponent
				opponent.Opponent = player
				player.RoomID = roomID
				player.Submissions = 0
				opponent.Submissions = 0
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
					Expr1:     "",
					Expr2:     "",
				}
				startRoomTimer(room)
				rooms[roomID] = room // Store the Room struct
				foundOpponent = true
				mutex.Unlock() // Unlock after modifying shared data
				startGame(player, opponent, puzzle, roomID)
				break
			}
		}

		if !foundOpponent {
			// No opponent found, add player to the private queue for this code
			privateQueues[req.Code] = append(privateQueues[req.Code], player)
			log.Printf("Player %s queued for private matchmaking with code: %s. Current private queue size: %d", req.UID, req.Code, len(privateQueues[req.Code]))
			mutex.Unlock() // Unlock after modifying shared data
		}
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
				closeRoom(room, fmt.Sprintf("Opponent Left %s", player.UID), "", "", "", "")
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
				// Store the expression in the room
				if room.Player1 != nil && room.Player1.UID == player.UID {
					room.Expr1 = msg.Expression // You are storing in Content here
				} else if room.Player2 != nil && room.Player2.UID == player.UID {
					room.Expr2 = msg.Expression // And here
				}
				broadcastExpressionUpdate(room, player.UID, msg.Expression) // You are then passing Content to broadcast
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
func sendExpressionUpdateToSpectator(conn *websocket.Conn, playerUID, expression, roomID string) {
	log.Printf("Sending expression update to spectator %s: %s", playerUID, expression)
	message := Message{Type: "expressionUpdate", Opponent: playerUID, Expression: expression, RoomID: roomID}
	jsonMessage, _ := json.Marshal(message)
	safeSend(conn, jsonMessage)
}

func broadcastExpressionUpdate(room *Room, playerUID, expression string) {
	updateMessage := Message{
		Type:       "expressionUpdate",
		Expression: expression,
		RoomID:     room.Player1.RoomID,
		Opponent:   playerUID,
	}
	log.Printf("Sending expression update to spectator %s: %s", playerUID, expression)

	updateJSON, _ := json.Marshal(updateMessage)

	// Send to Player 1 if they are not the sender
	if room.Player1 != nil && room.Player1.UID != playerUID {
		safeSend(room.Player1.Conn, updateJSON)
	}
	// Send to Player 2 if they are not the sender
	if room.Player2 != nil && room.Player2.UID != playerUID {
		safeSend(room.Player2.Conn, updateJSON)
	}

	// Send to all spectators
	for _, spectator := range room.Spectators {
		safeSend(spectator.Conn, updateJSON)
	}
}
func sendFeedbackUpdate(room *Room, playerUID, expression string) {
	updateMessage := Message{
		Type:       "feedbackUpdate",
		Expression: expression,
		Opponent:   playerUID,
	}
	log.Printf("Sending expression update to spectator %s: %s", playerUID, expression)

	updateJSON, _ := json.Marshal(updateMessage)

	for _, spectator := range room.Spectators {
		safeSend(spectator.Conn, updateJSON)
	}
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

	// Assign the WebSocket connection to the spectator
	spectator.Conn = conn

	// Add spectator to room
	room.Spectators = append(room.Spectators, spectator)
	log.Println("Spectator joined room:", roomID)

	// Send player meta data
	sendPlayerMeta(conn, "Player1", room.Player1.UID)
	sendPlayerMeta(conn, "Player2", room.Player2.UID)
	sendPlayerMeta(conn, "Time", strconv.FormatInt(int64((gameDuration-time.Since(room.StartTime)).Seconds()), 10))

	// Send initial puzzle
	sendPuzzle(conn, room.Puzzle)

	// Send current expressions
	sendExpressionUpdateToSpectator(conn, room.Player1.UID, room.Expr1, room.Player1.RoomID)
	sendExpressionUpdateToSpectator(conn, room.Player2.UID, room.Expr2, room.Player2.RoomID)
}
func sendPlayerMeta(conn *websocket.Conn, role, uid string) {
	message := Message{Type: "playerMeta", Player: uid, Content: uid, Opponent: role} // Keep 'Opponent' for now based on your Android code
	jsonMessage, _ := json.Marshal(message)
	safeSend(conn, jsonMessage)
}

func sendPuzzle(conn *websocket.Conn, puzzle string) {
	message := Message{Type: "puzzle", Content: puzzle}
	jsonMessage, _ := json.Marshal(message)
	safeSend(conn, jsonMessage)
}

func incrementMatchesPlayed(playerID string) {
	ctx := context.Background()
	_, err := dbClient.Collection("Users").Doc(playerID).Update(ctx, []firestore.Update{
		{
			Path:  "Played",
			Value: firestore.Increment(1),
		},
	})
	if err != nil {
		log.Printf("❌ Failed to increment 'played' for %s: %v", playerID, err)
	} else {
		log.Printf("✅ Incremented 'played' for %s", playerID)
	}
}
func incrementMatchesWon(playerID string) {
	ctx := context.Background()
	_, err := dbClient.Collection("Users").Doc(playerID).Update(ctx, []firestore.Update{
		{
			Path:  "Won",
			Value: firestore.Increment(1),
		},
	})
	if err != nil {
		log.Printf("❌ Failed to increment 'Won' for %s: %v", playerID, err)
	} else {
		log.Printf("✅ Incremented 'Won' for %s", playerID)
	}
}

func isPlayerActive(uid string) bool {
	// Check queue
	for _, p := range playersQueue {
		if p.UID == uid {
			return true
		}
	}
	for _, queue := range privateQueues {
		for _, p := range queue {
			if p.UID == uid {
				return true
			}
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
	go incrementMatchesPlayed(p1.UID)
	go incrementMatchesPlayed(p2.UID)

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
		closeRoom(room, "time limit reached", "", "", "", "")
	})
	log.Printf("Room %s timer started for %v", room.Player1.RoomID, gameDuration)
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
		sendFeedbackUpdate(room, player.UID, "Invalid answer format.")
		return
	}
	player.Submissions++

	if submittedAnswer == float64(targetValue) {
		log.Printf("Player %s submitted correct answer %d in room %s", player.UID, int64(submittedAnswer), roomID)
		sendFeedback(player.Conn, "You Won (Solved) (+50)")
		sendFeedbackUpdate(room, player.UID, "Won (Solved) (+50)")
		go updatePlayerRating(player.UID, 50)
		go incrementMatchesWon(player.UID)
		duration := time.Since(room.StartTime)
		timeTaken := int64(duration.Seconds())
		go updateTime(player, timeTaken)
		declareWinner(player, "Solved", rawexpressionStr)
	} else {
		log.Printf("Player %s submitted incorrect answer %d in room %s", player.UID, int64(submittedAnswer), roomID)
		sendFeedback(player.Conn, fmt.Sprintf("Incorrect. Your answer: %d", int64(submittedAnswer)))
		sendFeedbackUpdate(room, player.UID, fmt.Sprintf("Incorrect Submission : Answer = %d", int64(submittedAnswer)))
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
	for code, queue := range privateQueues {
		for i, p := range queue {
			if p == player {
				privateQueues[code] = append(queue[:i], queue[i+1:]...)
				log.Printf("Player %s REMOVED from private queue for code %s.", player.UID, code)
				isQueued = true
				break
			}
		}
		if isQueued {
			break
		}
	}
	if isQueued {
		player.Opponent = nil
		player.RoomID = ""
		log.Printf("Disconnection handled for player %s (was in private queue).", player.UID)
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
				duration := time.Since(room.StartTime)
				timeTaken := int64(duration.Seconds())
				go updateTime(opponent, timeTaken)
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
func updateAccuracy(player *Player) {
	ctx := context.Background()
	userRef := dbClient.Collection("Users").Doc(player.UID)

	docSnap, err := userRef.Get(ctx)
	if err != nil {
		log.Printf("❌ Failed to get user %s: %v", player.UID, err)
		return
	}

	data := docSnap.Data()
	oldAccuracy, _ := data["Accuracy"].(float64)
	matchesPlayed, _ := data["Played"].(int64) // Firestore may store numbers as int64

	if matchesPlayed == 0 {
		log.Printf("⚠️ Matches played is 0 for %s — cannot update accuracy", player.UID)
		return
	}

	var currentAccuracy float64
	if player.Submissions == 0 {
		currentAccuracy = 0
	} else {
		currentAccuracy = (1.0 / float64(player.Submissions)) * 100
	}

	newAccuracy := ((oldAccuracy * float64(matchesPlayed-1)) + currentAccuracy) / float64(matchesPlayed)

	_, err = userRef.Update(ctx, []firestore.Update{
		{Path: "Accuracy", Value: newAccuracy},
	})
	if err != nil {
		log.Printf("❌ Failed to update accuracy for %s: %v", player.UID, err)
	} else {
		log.Printf("✅ Updated accuracy for %s to %.2f", player.UID, newAccuracy)
	}
}
func updateTime(player *Player, timeTaken int64) {
	go func() {
		ctx := context.Background()
		userRef := dbClient.Collection("Users").Doc(player.UID)

		docSnap, err := userRef.Get(ctx)
		if err != nil {
			log.Printf("❌ Failed to get user %s: %v", player.UID, err)
			return
		}

		data := docSnap.Data()
		oldTime, _ := data["Time"].(int64)
		matchesPlayed, _ := data["Played"].(int64)

		if matchesPlayed == 0 {
			log.Printf("⚠️ Matches played is 0 for %s — cannot update time", player.UID)
			return
		}

		// Compute new average time
		newTime := ((oldTime * (matchesPlayed - 1)) + timeTaken) / matchesPlayed

		_, err = userRef.Update(ctx, []firestore.Update{
			{Path: "Time", Value: newTime},
		})
		if err != nil {
			log.Printf("❌ Failed to update time for %s: %v", player.UID, err)
		} else {
			log.Printf("✅ Updated time for %s to %d", player.UID, newTime)
		}
	}()
}

func generatePuzzle() string {
	rand.Seed(time.Now().UnixNano())

	for {
		digits := ""
		for i := 0; i < 6; i++ {
			digits += fmt.Sprintf("%d", rand.Intn(9)+1)
		}

		// Call Python to check if digits are solvable
		cmd := exec.Command("python3", "check_hectoc.py", digits)
		output, err := cmd.Output()
		if err != nil {
			fmt.Println("Python error:", err)
			continue
		}

		if strings.TrimSpace(string(output)) == "True" {
			return digits
		}
	}
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
func SaveMatchHistory(ctx context.Context, uid1, uid2, puzzle string, result1, result2, feedback1, feedback2 string, startTime time.Time) error {
	// Create match history for both users
	milliseconds := startTime.UnixNano() / 1_000_000
	match1 := MatchHistory{
		SelfUID:     uid1,
		OpponentUID: uid2,
		Timestamp:   milliseconds, // Use startTime instead of time.Now()
		Puzzle:      puzzle,
		Result:      result1,
		Feedback:    feedback1,
	}

	match2 := MatchHistory{
		SelfUID:     uid2,
		OpponentUID: uid1,
		Timestamp:   milliseconds, // Use startTime for both
		Puzzle:      puzzle,
		Result:      result2,
		Feedback:    feedback2,
	}

	_, err := dbClient.Collection("Users").Doc(uid1).Collection("MatchHistory").Doc(startTime.Format(time.RFC3339Nano)).Set(ctx, match1)
	if err != nil {
		return fmt.Errorf("error saving match history for %s: %w", uid1, err)
	}

	_, err = dbClient.Collection("Users").Doc(uid2).Collection("MatchHistory").Doc(startTime.Format(time.RFC3339Nano)).Set(ctx, match2)
	if err != nil {
		return fmt.Errorf("error saving match history for %s: %w", uid2, err)
	}

	return nil
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

func declareWinnerInternal(winner *Player, loser *Player, reason string, expression string) (string, string, string, string) {
	sendResult(winner, "You Won !!", fmt.Sprintf("(%s) (+50)", reason))
	sendResult(loser, "You lose!", "(-50)")

	// Update Firestore ratings
	go updatePlayerRating(winner.UID, 50)
	go incrementMatchesWon(winner.UID)
	go updateAccuracy(winner)
	go updateAccuracy(loser)
	go updatePlayerRating(loser.UID, -50)

	result1 := "won"
	result2 := "lose"
	feedback1 := fmt.Sprintf("You Won !! (%s)", reason)
	feedback2 := fmt.Sprintf("You lose! ")
	var foundRoom *Room
	for _, room := range rooms {
		if room.Player1 == winner || room.Player2 == winner || room.Player1 == loser || room.Player2 == loser {
			foundRoom = room
			break
		}
	}
	closeRoom(foundRoom, reason, result1, result2, feedback1, feedback2)

	return result1, result2, feedback1, feedback2
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
	go updateAccuracy(loser)
	go updateAccuracy(winner)
	SaveMatchHistory(context.Background(), winner.UID, loser.UID, room.Puzzle, "won", "lose", fmt.Sprintf("You Won!! Solution = %s = 100", expression), fmt.Sprintf("You Lose!! Possible Solution = %s = 100", expression), room.StartTime)

	if loser != nil {
		sendResult(loser, "You lose!", fmt.Sprintf("(opponent solved) (-50) \n\n Opponent's Solution : %s = 100", expression))
		go updatePlayerRating(loser.UID, -50)
		loser.Opponent = nil
		loser.RoomID = ""
		loser.Submitted = false
		safeSend(loser.Conn, []byte(`{"type": "opponent_disconnected"}`))
	} else {
		log.Printf("Loser NOT identified in declareWinner for room %s.", roomID)
	}
	sendResult(winner, "You Won !!", fmt.Sprintf("(%s) (+50)", reason))
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
func closeRoom(room *Room, reason string, result1 string, result2 string, feedback1 string, feedback2 string) {
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
			go incrementMatchesWon(p1.UID)
			go updatePlayerRating(p2.UID, -50)
			duration := time.Since(room.StartTime)
			timeTaken := int64(duration.Seconds())
			go updateTime(p1, timeTaken)
			go updateTime(p2, 120)
			sendResult(p1, "You Won !!", "Opponent Left (+50)")
			result1 = "won"
			result2 = "lose"
			feedback1 = "Opponent Left"
			feedback2 = "You Left"
		} else if left == p1.UID {
			go updatePlayerRating(p2.UID, 50)
			go incrementMatchesWon(p2.UID)
			go updatePlayerRating(p1.UID, -50)
			duration := time.Since(room.StartTime)
			timeTaken := int64(duration.Seconds())
			go updateTime(p2, timeTaken)
			go updateTime(p1, 120)
			sendResult(p2, "You Won !!", "(Opponent Left) (+50)")
			result1 = "lose"
			result2 = "win"
			feedback1 = "You Left"
			feedback2 = "Opponent Left"
		}

	} else if reason == "time limit reached" {
		go updatePlayerRating(p1.UID, -10)
		go updatePlayerRating(p2.UID, -10)
		go updateTime(p1, 120)
		go updateTime(p2, 120)
		sendResult(p1, "Time limit reached", "Match Drawn. (-10)")
		sendResult(p2, "Time limit reached", "Match Drawn. (-10)")
		result1 = "draw"
		result2 = "draw"
		feedback1 = "Time limit reached"
		feedback2 = "Time limit reached"

	} else {
		if p1 != nil && p2 != nil {
			if p1.Submitted && !p2.Submitted {
				declareWinnerInternal(p1, p2, "opponent timed out", room.Expr1)
			} else if !p1.Submitted && p2.Submitted {
				declareWinnerInternal(p2, p1, "opponent timed out", room.Expr2)
			} else {
				// If neither submitted or both submitted (without further logic), declare a draw or no winner.
				go updatePlayerRating(p1.UID, -10)
				go updatePlayerRating(p2.UID, -10)
				go updateTime(p1, 120)
				go updateTime(p2, 120)
				sendResult(p1, "Time limit reached", "Match Drawn. (-10)")
				sendResult(p2, "Time limit reached", "Match Drawn. (-10)")
				result1 = "draw"
				result2 = "draw"
				feedback1 = "Time limit reached"
				feedback2 = "Time limit reached"
			}
		} else if p1 != nil {
			sendResult(p1, "Game Over!", reason)
			result1 = "unknown"
			result2 = "unknown"
			feedback1 = reason
			feedback2 = reason
		} else if p2 != nil {
			sendResult(p2, "Game Over!", reason)
			result1 = "unknown"
			result2 = "unknown"
			feedback1 = reason
			feedback2 = reason
		}
	}

	// Gather solutions (You might need to store these during the game)

	// Save match history
	err := SaveMatchHistory(context.Background(), p1.UID, p2.UID, room.Puzzle, result1, result2, feedback1, feedback2, room.StartTime)
	if err != nil {
		log.Printf("Error saving match history for room %s: %v", roomID, err)
	} else {
		log.Printf("Match history saved for room %s", roomID)
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
