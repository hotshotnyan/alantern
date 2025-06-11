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

type ChatServer struct {
	clients    map[string]chan string
	clientsMu  sync.Mutex

	nicknames    map[string]string
	nicknamesMu  sync.Mutex

	nicknameColors    map[string]string
	nicknameColorsMu  sync.Mutex

	imageStore    map[string][]byte
	imageExpiry   map[string]time.Time
	imageStoreMu  sync.Mutex

	lastMessageTime    map[string]time.Time
	lastMessageTimeMu  sync.Mutex

	spamCount    map[string]int
	spamCountMu  sync.Mutex
}

func NewChatServer() *ChatServer {
	return &ChatServer{
		clients:         make(map[string]chan string),
		nicknames:       make(map[string]string),
		nicknameColors:  make(map[string]string),
		imageStore:      make(map[string][]byte),
		imageExpiry:     make(map[string]time.Time),
		lastMessageTime: make(map[string]time.Time),
		spamCount:      make(map[string]int),
	}
}

func (s *ChatServer) Start() error {
	http.HandleFunc("/", s.serveChatPage)
	http.HandleFunc("/send", s.handleSendMessage)
	http.HandleFunc("/events", s.handleEvents)
	http.HandleFunc("/set-nickname", s.handleSetNickname)

	http.HandleFunc("/upload-image", s.handleImageUpload)
	http.HandleFunc("/image/", s.handleImage)

	http.HandleFunc("/join", s.handleJoin)
	http.HandleFunc("/leave", s.handleLeave)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("Server started on http://0.0.0.0:%s\n", port)
	s.startImageCleanup()
	return http.ListenAndServe(fmt.Sprintf("0.0.0.0:%s", port), nil)
}

func (s *ChatServer) serveChatPage(w http.ResponseWriter, r *http.Request) {
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

func escapeHTMLSpecialChars(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(s)
}

func (s *ChatServer) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	message := r.FormValue("message")
	if message == "" {
		http.Error(w, "Message is required", http.StatusBadRequest)
		return
	}

	sessionID := getOrCreateSession(w, r)

	s.lastMessageTimeMu.Lock()
	lastTime, exists := s.lastMessageTime[sessionID]
	if exists && time.Since(lastTime) < 2*time.Second {
		s.spamCountMu.Lock()
		s.spamCount[sessionID]++
		if s.spamCount[sessionID] >= 5 {
			s.spamCountMu.Unlock()
			s.lastMessageTimeMu.Unlock()
			s.sendPrivateMessage(sessionID, "{app}: You are sending messages too quickly!")
			return
		}
		s.spamCountMu.Unlock()
	} else {
		s.spamCountMu.Lock()
		s.spamCount[sessionID] = 0
		s.spamCountMu.Unlock()
	}
	s.lastMessageTime[sessionID] = time.Now()
	s.lastMessageTimeMu.Unlock()

	if strings.HasPrefix(message, ";") {
		s.handleCommand(sessionID, message)
		return
	}

	s.nicknameColorsMu.Lock()
	color := s.nicknameColors[sessionID]
	s.nicknameColorsMu.Unlock()

	escapedMessage := escapeHTMLSpecialChars(message)

	var formattedMessage string
	if color != "" {
		formattedMessage = fmt.Sprintf("@color %s [%s]: %s", color, s.getNickname(sessionID), escapedMessage)
	} else {
		formattedMessage = fmt.Sprintf("[%s]: %s", s.getNickname(sessionID), escapedMessage)
	}

	s.broadcastMessage(formattedMessage)
	fmt.Fprintf(w, "Message sent")
}

func (s *ChatServer) handleCommand(sessionID, message string) {
	switch strings.ToLower(strings.Split(message, " ")[0]) {
	case ";help":
		s.sendPrivateMessage(sessionID, "{app}: Available commands: ;help, ;members, ;whisper <username> <message>, ;color <hexcode|colorname>")

	case ";members":
		s.nicknamesMu.Lock()
		members := ""
		for memberSessionID, nickname := range s.nicknames {
			members = fmt.Sprintf("%s [%s] (%s)", members, escapeHTMLSpecialChars(nickname), escapeHTMLSpecialChars(memberSessionID))
		}
		s.nicknamesMu.Unlock()
		s.sendPrivateMessage(sessionID, "{app}: Online members" + members)

	case ";whisper":
		splitted := strings.Split(message, " ")
		if len(splitted) < 3 {
			s.sendPrivateMessage(sessionID, "{app}: Usage: ;whisper <username> <message>")
			return
		}
		toNickname := splitted[1]
		msg := strings.Join(splitted[2:], " ")

		var toSessionID string
		s.nicknamesMu.Lock()
		for k, v := range s.nicknames {
			if v == toNickname {
				toSessionID = k
				break
			}
		}
		s.nicknamesMu.Unlock()

		if toSessionID == "" {
			s.sendPrivateMessage(sessionID, fmt.Sprintf("{app}: User %s not found", escapeHTMLSpecialChars(toNickname)))
			return
		}
		escapedMsg := escapeHTMLSpecialChars(msg)
		msgToSend := fmt.Sprintf("(whisper to @%s) [%s]: %s", 
			escapeHTMLSpecialChars(toNickname), 
			escapeHTMLSpecialChars(s.getNickname(sessionID)), 
			escapedMsg)
		
		s.sendPrivateMessage(toSessionID, msgToSend)
		s.sendPrivateMessage(sessionID, msgToSend)

	case ";color":
		splitted := strings.Split(message, " ")
		if len(splitted) != 2 {
			s.sendPrivateMessage(sessionID, "{app}: Usage: ;color <hexcode|colorname> (e.g., ;color #ff0000 or ;color red)")
			return
		}
		color := splitted[1]

		predefinedColors := map[string]string{
			"red": "#ff0000",
			"lightred": "#ff6666",
			"darkred": "#8b0000",
			"blue": "#0000ff",
			"lightblue": "#add8e6",
			"darkblue": "#00008b",
			"green": "#008000",
			"lightgreen": "#90ee90",
			"darkgreen": "#006400",
			"yellow": "#ffff00",
			"lightyellow": "#ffffe0",
			"darkyellow": "#9b870c",
			"purple": "#800080",
			"lightpurple": "#dda0dd",
			"darkpurple": "#4b0082",
			"orange": "#ffa500",
			"lightorange": "#ffcc99",
			"darkorange": "#ff8c00",
			"pink": "#ffc0cb",
			"lightpink": "#ffb6c1",
			"darkpink": "#c71585",
			"cyan": "#00ffff",
			"lightcyan": "#e0ffff",
			"darkcyan": "#008b8b",
			"brown": "#a52a2a",
			"lightbrown": "#deb887",
			"darkbrown": "#654321",
			"black": "#000000",
			"lightblack": "#696969",
			"darkblack": "#0a0a0a",
			"white": "#ffffff",
			"lightwhite": "#f5f5f5",
			"darkwhite": "#dcdcdc",
			"gray": "#808080",
			"lightgray": "#d3d3d3",
			"darkgray": "#505050",
			"gold": "#ffd700",
			"lightgold": "#ffec8b",
			"darkgold": "#b8860b",
			"silver": "#c0c0c0",
			"lightsilver": "#e6e6e6",
			"darksilver": "#a9a9a9",
			"navy": "#000080",
			"lightnavy": "#4682b4",
			"darknavy": "#00004d",
			"lime": "#00ff00",
			"lightlime": "#bfff00",
			"darklime": "#32cd32",
			"magenta": "#ff00ff",
			"lightmagenta": "#ff77ff",
			"darkmagenta": "#8b008b",
			"beige": "#f5f5dc",
			"lightbeige": "#faf0e6",
			"darkbeige": "#d2b48c",
			"olive": "#808000",
			"lightolive": "#b5b35c",
			"darkolive": "#556b2f",
			"maroon": "#800000",
			"lightmaroon": "#b03060",
			"darkmaroon": "#5c0000",
			"violet": "#ee82ee",
			"lightviolet": "#f3e5ab",
			"darkviolet": "#9400d3",
			"indigo": "#4b0082",
			"lightindigo": "#7a5c99",
			"darkindigo": "#310062",
			"turquoise": "#40e0d0",
			"lightturquoise": "#afeeee",
			"darkturquoise": "#00ced1",
			"chocolate": "#d2691e",
			"lightchocolate": "#e6b8a2",
			"darkchocolate": "#8b4513",
			"coral": "#ff7f50",
			"lightcoral": "#f08080",
			"darkcoral": "#cd5b45",
			"salmon": "#fa8072",
			"lightsalmon": "#ffa07a",
			"darksalmon": "#e9967a",
			"khaki": "#f0e68c",
			"lightkhaki": "#fffacd",
			"darkkhaki": "#bdb76b",
			"orchid": "#da70d6",
			"lightorchid": "#e6a8d7",
			"darkorchid": "#9932cc",
			"plum": "#dda0dd",
			"lightplum": "#e6b8e6",
			"darkplum": "#8e4585",
			"tan": "#d2b48c",
			"lighttan": "#f5deb3",
			"darktan": "#a0522d",
			"lavender": "#e6e6fa",
			"lightlavender": "#f3e5f5",
			"darklavender": "#7c7c99",
			"peach": "#ffdab9",
			"lightpeach": "#ffefd5",
			"darkpeach": "#cd853f",
			"mint": "#98ff98",
			"lightmint": "#bdfcc9",
			"darkmint": "#3cb371",
			"aqua": "#00ffff",
			"lightaqua": "#e0ffff",
			"darkaqua": "#008b8b",
			"skyblue": "#87ceeb",
			"lightskyblue": "#b0e2ff",
			"darkskyblue": "#4682b4",
			"crimson": "#dc143c",
			"lightcrimson": "#ff6f61",
			"darkcrimson": "#8b0000",
			"goldenrod": "#daa520",
			"lightgoldenrod": "#ffec8b",
			"darkgoldenrod": "#b8860b",
			"seagreen": "#2e8b57",
			"lightseagreen": "#54ff9f",
			"darkseagreen": "#8fbc8f",
			"slateblue": "#6a5acd",
			"lightslateblue": "#8470ff",
			"darkslateblue": "#483d8b",
			"steelblue": "#4682b4",
			"lightsteelblue": "#b0c4de",
			"darksteelblue": "#2a4f7c",
			"tomato": "#ff6347",
			"lighttomato": "#ff7f50",
			"darktomato": "#cd5b45",
			"wheat": "#f5deb3",
			"lightwheat": "#ffe4b5",
			"darkwheat": "#d2b48c",
			"azure": "#f0ffff",
			"lightazure": "#e0ffff",
			"darkazure": "#b0e0e6",
			"ivory": "#fffff0",
			"lightivory": "#f5f5dc",
			"darkivory": "#dcdcdc",
			"lavenderblush": "#fff0f5",
			"lightlavenderblush": "#ffe4e1",
			"darklavenderblush": "#d8bfd8",
			"mistyrose": "#ffe4e1",
			"lightmistyrose": "#ffebcd",
			"darkmistyrose": "#cd5b45",
			"powderblue": "#b0e0e6",
			"lightpowderblue": "#add8e6",
			"darkpowderblue": "#4682b4",
			"rosybrown": "#bc8f8f",
			"lightrosybrown": "#deb887",
			"darkrosybrown": "#8b4513",
			"sandybrown": "#f4a460",
			"lightsandybrown": "#ffcc99",
			"darksandybrown": "#cd853f",
			"snow": "#fffafa",
			"lightsnow": "#f5f5f5",
			"darksnow": "#dcdcdc",
			"thistle": "#d8bfd8",
			"lightthistle": "#e6e6fa",
			"darkthistle": "#7c7c99",
			"yellowgreen": "#9acd32",
			"lightyellowgreen": "#adff2f",
			"darkyellowgreen": "#556b2f",
		}

		if hex, ok := predefinedColors[strings.ToLower(color)]; ok {
			color = hex
		} else if !strings.HasPrefix(color, "#") || len(color) != 7 {
			s.sendPrivateMessage(sessionID, "{app}: Invalid color format. Use hexadecimal format like #ff0000 or predefined names like red")
			return
		}

		s.nicknameColorsMu.Lock()
		s.nicknameColors[sessionID] = color
		s.nicknameColorsMu.Unlock()
		s.sendPrivateMessage(sessionID, fmt.Sprintf("{app}: Your nickname color has been changed to %s", color))

	default:
		s.sendPrivateMessage(sessionID, "{app}: Unknown command: " + escapeHTMLSpecialChars(message))
	}
}

func (s *ChatServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sessionID := getOrCreateSession(w, r)
	msgCh := make(chan string)

	s.clientsMu.Lock()
	s.clients[sessionID] = msgCh
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, sessionID)
		s.clientsMu.Unlock()
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

func (s *ChatServer) getNickname(sessionID string) string {
	s.nicknamesMu.Lock()
	defer s.nicknamesMu.Unlock()
	if nickname, ok := s.nicknames[sessionID]; ok {
		return nickname
	}
	return "anonymous"
}

func (s *ChatServer) generateRandomColor() string {
	colors := []string{"#ff0000", "#00ff00", "#0000ff", "#ffff00", "#ff00ff", "#00ffff",
		"#ff8000", "#ff0080", "#80ff00", "#00ff80", "#8000ff", "#0080ff"}
	r := make([]byte, 1)
	rand.Read(r)
	return colors[int(r[0])%len(colors)]
}

func (s *ChatServer) handleSetNickname(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	nickname := r.FormValue("nickname")

	if nickname == "" || strings.Contains(nickname, " ") {
		http.Error(w, "Invalid nickname: either empty or contains spaces", http.StatusBadRequest)
		return
	}

	sessionID := getOrCreateSession(w, r)
	
	s.nicknamesMu.Lock()
	old := s.nicknames[sessionID]
	s.nicknames[sessionID] = nickname
	s.nicknamesMu.Unlock()

	s.nicknameColorsMu.Lock()
	if _, exists := s.nicknameColors[sessionID]; !exists {
		s.nicknameColors[sessionID] = s.generateRandomColor()
	}
	s.nicknameColorsMu.Unlock()

	if old == "" {
		old = "no previous nicknames"
	} else {
		old = fmt.Sprintf("previously [%s]", old)
	}

	s.broadcastMessage(fmt.Sprintf(`<span class="highlight-admin-app">Alantern</span>: client %s (%s) changed nickname to [%s]`, sessionID, old, nickname))
	fmt.Fprintf(w, "Nickname set to %s for session %s", nickname, sessionID)
}

func (s *ChatServer) broadcastMessage(message string) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	for _, ch := range s.clients {
		go func(c chan string) {
			c <- message
		}(ch)
	}
}

func (s *ChatServer) sendPrivateMessage(sessionID, message string) {
	s.clientsMu.Lock()
	ch, ok := s.clients[sessionID]
	s.clientsMu.Unlock()
	if ok {
		go func() {
			if strings.Contains(message, "{app}") {
				message = strings.ReplaceAll(message, "{app}", `<span class="highlight-admin-app">Alantern</span>`)
			}
			ch <- `<div class="private-message">` + message + `</div>`
		}()
	}
}

func generateRandomId() string {
	t := time.Now().UnixMilli()
	r := make([]byte, 4)
	_, err := rand.Read(r)
	if err != nil {
		return fmt.Sprintf("%d", t)
	}
	return fmt.Sprintf("%d-%x", t, r)
}

func (s *ChatServer) handleImageUpload(w http.ResponseWriter, r *http.Request) {
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
	s.imageStoreMu.Lock()
	s.imageStore[id] = imageBytes
	s.imageExpiry[id] = time.Now().Add(1 * time.Minute)
	s.imageStoreMu.Unlock()

	sessionID := getOrCreateSession(w, r)
	s.broadcastMessage(fmt.Sprintf("@image [%s] %s", s.getNickname(sessionID), id))
	w.Write([]byte("Image uploaded"))
}

func (s *ChatServer) handleImage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/image/")
	s.imageStoreMu.Lock()
	data, ok := s.imageStore[id]
	s.imageStoreMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Write(data)
}

func (s *ChatServer) startImageCleanup() {
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		for range ticker.C {
			now := time.Now()
			s.imageStoreMu.Lock()
			for id, expiry := range s.imageExpiry {
				if now.After(expiry) {
					delete(s.imageStore, id)
					delete(s.imageExpiry, id)
				}
			}
			s.imageStoreMu.Unlock()
		}
	}()
}

func (s *ChatServer) handleJoin(w http.ResponseWriter, r *http.Request) {
	sessionID := getOrCreateSession(w, r)
	s.broadcastMessage(fmt.Sprintf(`<span class="highlight-admin-app">Alantern</span>: %s ([%s]) has joined the room`, sessionID, s.getNickname(sessionID)))
	w.WriteHeader(http.StatusOK)
}

func (s *ChatServer) handleLeave(w http.ResponseWriter, r *http.Request) {
	sessionID := getOrCreateSession(w, r)
	s.broadcastMessage(fmt.Sprintf(`<span class="highlight-admin-app">Alantern</span>: [%s] (%s) has left the room`, s.getNickname(sessionID), sessionID))
	w.WriteHeader(http.StatusOK)
}

func main() {
	server := NewChatServer()
	if err := server.Start(); err != nil {
		fmt.Printf("Server error: %v\n", err)
		os.Exit(1)
	}
}
