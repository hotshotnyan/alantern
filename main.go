package main

import (
	"crypto/rand"
	"embed"
	"fmt"
	"io"
	"net"
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

func handleSendMessage(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	message := r.FormValue("message")
	if message == "" {
		http.Error(w, "Message is required", http.StatusBadRequest)
		return
	}

	ip := getIP(r.RemoteAddr)

	if strings.HasPrefix(message, ";") {
		handleCommand(ip, message)
		return
	}

	nicknameColorsMu.Lock()
	color := nicknameColors[ip]
	nicknameColorsMu.Unlock()

	var formattedMessage string
	if color != "" {
		formattedMessage = fmt.Sprintf("@color %s [%s]: %s", color, getNickname(ip), message)
	} else {
		formattedMessage = fmt.Sprintf("[%s]: %s", getNickname(ip), message)
	}

	broadcastMessage(formattedMessage)
	fmt.Fprintf(w, "Message sent")
}

func handleCommand(ip, message string) {
	switch strings.ToLower(strings.Split(message, " ")[0]) {
	case ";help":
		sendPrivateMessage(ip, "{app}: Available commands: ;help, ;members, ;whisper <username> <message>, ;color <hexcode>")

	case ";color":
		splitted := strings.Split(message, " ")
		if len(splitted) != 2 {
			sendPrivateMessage(ip, "{app}: Usage: ;color <hexcode> (e.g., ;color #ff0000 for red)")
			return
		}
		color := splitted[1]
		if !strings.HasPrefix(color, "#") || len(color) != 7 {
			sendPrivateMessage(ip, "{app}: Invalid color format. Use hexadecimal format like #ff0000")
			return
		}
		nicknameColorsMu.Lock()
		nicknameColors[ip] = color
		nicknameColorsMu.Unlock()
		sendPrivateMessage(ip, fmt.Sprintf("{app}: Your nickname color has been changed to %s", color))

	case ";members":
		nicknamesMu.Lock()
		members := ""
		for memberIP, nickname := range nicknames {
			members = fmt.Sprintf("%s [%s] (%s)", members, memberIP, nickname)
		}
		nicknamesMu.Unlock()
		sendPrivateMessage(ip, "{app}: Online members" + members)

	case ";whisper":
		splitted := strings.Split(message, " ")
		toNickname := splitted[1]
		msg := strings.Join(splitted[2:], " ")

		var toIP string
		for k, v := range nicknames {
			if v == toNickname {
				toIP = k
			}
		}

		msgToSend := fmt.Sprintf("(whisper to ~%s) [%s]: %s", toNickname, getNickname(ip), msg)
		sendPrivateMessage(toIP, msgToSend)
		sendPrivateMessage(ip, msgToSend)

	default:
		sendPrivateMessage(ip, "{app}: Unknown command: " + message)
	}
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ip := getIP(r.RemoteAddr)
	msgCh := make(chan string)

	clientsMu.Lock()
	clients[ip] = msgCh
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, ip)
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

func getIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return ip
}

func getNickname(ip string) string {
	nicknamesMu.Lock()
	defer nicknamesMu.Unlock()
	if nickname, ok := nicknames[ip]; ok {
		return nickname
	}
	return ip
}

func handleSetNickname(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	nickname := r.FormValue("nickname")

	if nickname == "" || strings.Contains(nickname, " ") {
		http.Error(w, "Invalid nickname: either empty or contains spaces", http.StatusBadRequest)
		return
	}

	ip := getIP(r.RemoteAddr)
	nicknamesMu.Lock()
	old := nicknames[ip]
	nicknames[ip] = nickname
	nicknamesMu.Unlock()

	if old == "" {
		old = "no previous nicknames"
	} else {
		old = fmt.Sprintf("previously [%s]", old)
	}

	broadcastMessage(fmt.Sprintf("{app}: client %s (%s) changed nickname to [%s]", ip, old, nickname))
	fmt.Fprintf(w, "Nickname set to %s for IP %s", nickname, ip)
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

func sendPrivateMessage(ip, message string) {
	clientsMu.Lock()
	ch, ok := clients[ip]
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

	id := generateRandomId() // Variable declared but not used; removing or utilizing it
	imageStoreMu.Lock()
	imageStore[id] = imageBytes
	imageExpiry[id] = time.Now().Add(1 * time.Minute)
	imageStoreMu.Unlock()

	broadcastMessage(fmt.Sprintf("@image [%s] %s", getNickname(getIP(r.RemoteAddr)), id))
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
	ip := getIP(r.RemoteAddr)
	broadcastMessage(fmt.Sprintf("{app}: %s ([%s]) has joined the room", ip, getNickname(ip)))
	w.WriteHeader(http.StatusOK)
}

func handleLeave(w http.ResponseWriter, r *http.Request) {
	ip := getIP(r.RemoteAddr)
	broadcastMessage(fmt.Sprintf("{app}: [%s] (%s) has left the room", getNickname(ip), ip))
	w.WriteHeader(http.StatusOK)
}
