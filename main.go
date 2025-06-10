package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var embeddedFiles embed.FS

var (
	clients = make(map[string]chan string)
	clientsMu sync.Mutex

	nicknames = make(map[string]string)
	nicknamesMu sync.Mutex

	nicknameColors = make(map[string]string)
	nicknameColorsMu sync.Mutex

	imageStore = make(map[string][]byte)
	imageExpiry = make(map[string]time.Time)
	imageStoreMu sync.Mutex
)

var sessionCookieName = "alantern_session"

func main() {
	http.HandleFunc("/", serveChatPage)
	http.HandleFunc("/send", handleSendMessage)
	http.HandleFunc("/events", handleEvents)
	http.HandleFunc("/set-nickname", handleSetNickname)

	http.HandleFunc("/upload-image", handleImageUpload)
	http.HandleFunc("/image/", handleImage)

	http.HandleFunc("/join", handleJoin)
	http.HandleFunc("/leave", handleLeave)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("Server started on http://0.0.0.0:%s\n", port)
	startImageCleanup()
	http.ListenAndServe(fmt.Sprintf("0.0.0.0:%s", port), nil)
}

func serveChatPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")

	if _, err := os.Stat("index.html"); err == nil {
		http.ServeFile(w, r, "index.html")
		return
	}

	data, err := embeddedFiles.ReadFile("index.html")
	if err != nil {
		http.Error(w, "Could not load chat UI", http.StatusInternalServerError)
		return
	}

	w.Write(data)
}

func generateSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.URLEncoding.EncodeToString(b)
}

func getOrCreateSession(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("session_id")
	if err == nil {
		return cookie.Value
	}

	sessionID := generateRandomId()
	http.SetCookie(w, &http.Cookie{
		Name:  "session_id",
		Value: sessionID,
		Path:  "/",
	})
	return sessionID
}

func handleSendMessage(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	message := r.FormValue("message")
	if message == "" {
		http.Error(w, "Message is required", http.StatusBadRequest)
		return
	}

	sessionID := getOrCreateSession(w, r)

	if strings.HasPrefix(message, ";") {
		handleCommand(sessionID, message)
		return
	}

	nicknameColorsMu.Lock()
	color := nicknameColors[sessionID]
	nicknameColorsMu.Unlock()

	var formattedMessage string
	if color != "" {
		formattedMessage = fmt.Sprintf("@color %s [%s]: %s", color, getNickname(sessionID), message)
	} else {
		formattedMessage = fmt.Sprintf("[%s]: %s", getNickname(sessionID), message)
	}

	broadcastMessage(formattedMessage)
	fmt.Fprintf(w, "Message sent")
}

func handleCommand(sessionID, message string) {
	switch strings.ToLower(strings.Split(message, " ")[0]) {
	case ";help":
		sendPrivateMessage(sessionID, "{app}: Available commands: ;help, ;members, ;whisper <username> <message>, ;color <hexcode>")

	case ";color":
		splitted := strings.Split(message, " ")
		if len(splitted) != 2 {
			sendPrivateMessage(sessionID, "{app}: Usage: ;color <hexcode> (e.g., ;color #ff0000 for red)")
			return
		}
		color := splitted[1]
		if !strings.HasPrefix(color, "#") || len(color) != 7 {
			sendPrivateMessage(sessionID, "{app}: Invalid color format. Use hexadecimal format like #ff0000")
			return
		}
		nicknameColorsMu.Lock()
		nicknameColors[sessionID] = color
		nicknameColorsMu.Unlock()
		sendPrivateMessage(sessionID, fmt.Sprintf("{app}: Your nickname color has been changed to %s", color))

	case ";members":
		nicknamesMu.Lock()
		members := ""
		for memberSessionID, nickname := range nicknames {
			members = fmt.Sprintf("%s [%s] (%s)", members, memberSessionID, nickname)
		}
		nicknamesMu.Unlock()
		sendPrivateMessage(sessionID, "{app}: Online members" + members)

	case ";whisper":
		splitted := strings.Split(message, " ")
		toNickname := splitted[1]
		msg := strings.Join(splitted[2:], " ")

		var toSessionID string
		for k, v := range nicknames {
			if v == toNickname {
				toSessionID = k
			}
		}

		msgToSend := fmt.Sprintf("(whisper to ~%s) [%s]: %s", toNickname, getNickname(sessionID), msg)
		sendPrivateMessage(toSessionID, msgToSend)
		sendPrivateMessage(sessionID, msgToSend)

	default:
		sendPrivateMessage(sessionID, "{app}: Unknown command: " + message)
	}
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sessionID := getOrCreateSession(w, r)
	msgCh := make(chan string)

	clientsMu.Lock()
	clients[sessionID] = msgCh
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, sessionID)
		clientsMu.Unlock()
		close(msgCh)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	for msg := range msgCh {
		fmt.Fprintf(w, "data: %s\n\n", msg)
		flusher.Flush()
	}
}

func getNickname(sessionID string) string {
	nicknamesMu.Lock()
	defer nicknamesMu.Unlock()
	if nickname, ok := nicknames[sessionID]; ok {
		return nickname
	}
	return "anonymous"
}

func handleSetNickname(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	nickname := r.FormValue("nickname")

	if nickname == "" || strings.Contains(nickname, " ") {
		http.Error(w, "Invalid nickname: either empty or contains spaces", http.StatusBadRequest)
		return
	}

	sessionID := getOrCreateSession(w, r)
	nicknamesMu.Lock()
	old := nicknames[sessionID]
	nicknames[sessionID] = nickname
	nicknamesMu.Unlock()

	if old == "" {
		old = "no previous nicknames"
	} else {
		old = fmt.Sprintf("previously [%s]", old)
	}

	broadcastMessage(fmt.Sprintf("{app}: client %s (%s) changed nickname to [%s]", sessionID, old, nickname))
	fmt.Fprintf(w, "Nickname set to %s for session %s", nickname, sessionID)
}

func broadcastMessage(message string) {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	for _, ch := range clients {
		go func(c chan string) {
			c <- message
		}(ch)
	}
}

func sendPrivateMessage(sessionID, message string) {
	clientsMu.Lock()
	ch, ok := clients[sessionID]
	clientsMu.Unlock()
	if ok {
		go func() {
			ch <- "@private " + message
		}()
	}
}

func generateRandomId() string {
	t := time.Now().UnixMilli()
	r := make([]byte, 4)
	_, err := rand.Read(r)
	if err != nil {
		broadcastMessage("{app}: PANIC, shutting down")
		panic("unable to generate random bytes: " + err.Error())
	}
	return fmt.Sprintf("%d-%x", t, r)
}

func handleImageUpload(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		http.Error(w, "Could not parse multipart form", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Invalid image", http.StatusBadRequest)
		return
	}
	defer file.Close()

	imageBytes, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Error reading image", http.StatusInternalServerError)
		return
	}

	id := generateRandomId()
	imageStoreMu.Lock()
	imageStore[id] = imageBytes
	imageExpiry[id] = time.Now().Add(1 * time.Minute)
	imageStoreMu.Unlock()

	sessionID := getOrCreateSession(w, r)
	broadcastMessage(fmt.Sprintf("@image [%s] %s", getNickname(sessionID), id))
	w.Write([]byte("Image uploaded"))
}

func handleImage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/image/")
	imageStoreMu.Lock()
	data, ok := imageStore[id]
	imageStoreMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Write(data)
}

func startImageCleanup() {
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for range ticker.C {
			now := time.Now()
			imageStoreMu.Lock()
			for id, expiry := range imageExpiry {
				if now.After(expiry) {
					delete(imageStore, id)
					delete(imageExpiry, id)
				}
			}
			imageStoreMu.Unlock()
		}
	}()
}

func handleJoin(w http.ResponseWriter, r *http.Request) {
	sessionID := getOrCreateSession(w, r)
	broadcastMessage(fmt.Sprintf("{app}: %s ([%s]) has joined the room", sessionID, getNickname(sessionID)))
	w.WriteHeader(http.StatusOK)
}

func handleLeave(w http.ResponseWriter, r *http.Request) {
	sessionID := getOrCreateSession(w, r)
	broadcastMessage(fmt.Sprintf("{app}: [%s] (%s) has left the room", getNickname(sessionID), sessionID))
	w.WriteHeader(http.StatusOK)
}
