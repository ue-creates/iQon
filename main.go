package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- データ構造 ---

type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parentId"`
}

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ActivityMessage struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	ChannelID string    `json:"channelId"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

type ChannelsResponse struct {
	Public []Channel `json:"public"`
}

type ExtensionUpdate struct {
	Type        string `json:"type"`
	ChannelPath string `json:"channelPath"`
	Username    string `json:"username"`
}

type ExtensionInit struct {
	Type  string            `json:"type"`
	State map[string]string `json:"state"`
}

// --- グローバル変数 ---

var (
	botToken string

	channelMap = make(map[string]Channel)
	userMap    = make(map[string]string)
	mapMutex   sync.RWMutex

	lastSpeakers = make(map[string]string)
	stateMutex   sync.RWMutex

	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex

	lastCheckTime time.Time

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func main() {
	botToken = os.Getenv("TRAQ_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("ERROR: TRAQ_BOT_TOKEN is not set")
	}

	log.Println("⏳ Fetching initial data...")
	if err := fetchData(); err != nil {
		log.Fatalf("Failed to fetch initial data: %v", err)
	}

	lastCheckTime = time.Now().UTC()

	// ポーリング開始
	go startPolling()

	// サーバー起動
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("🚀 Server started on :%s (Polling Mode)", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

// --- ポーリング処理 ---

func startPolling() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	client := &http.Client{Timeout: 5 * time.Second}
	log.Println("👀 Polling started...")

	for range ticker.C {
		url := "https://q.trap.jp/api/v3/activity/timeline?all=true&limit=50"
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+botToken)

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Polling error: %v", err)
			continue
		}
		
		if resp.StatusCode != 200 {
			log.Printf("Polling failed: Status %d", resp.StatusCode)
			resp.Body.Close()
			continue
		}

		var timeline []ActivityMessage
		if err := json.NewDecoder(resp.Body).Decode(&timeline); err != nil {
			log.Printf("JSON decode error: %v", err)
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		processTimeline(timeline)
	}
}

func processTimeline(messages []ActivityMessage) {
	if len(messages) == 0 {
		return
	}

	newestInBatch := lastCheckTime
	updates := make(map[string]ExtensionUpdate)

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]

		if !msg.CreatedAt.After(lastCheckTime) {
			continue
		}
		if msg.CreatedAt.After(newestInBatch) {
			newestInBatch = msg.CreatedAt
		}

		username := resolveUser(msg.UserID)
		path := resolveChannelPath(msg.ChannelID)

		// ユーザー解決できなかった場合(webhookなど)も "webhook" という名前で返ってくるので続行可能
		if username == "" || path == "" {
			continue
		}

		updates[path] = ExtensionUpdate{
			Type:        "UPDATE",
			ChannelPath: path,
			Username:    username,
		}
	}

	lastCheckTime = newestInBatch

	if len(updates) > 0 {
		stateMutex.Lock()
		for path, update := range updates {
			lastSpeakers[path] = update.Username
			// log.Printf("📢 Polled: %s -> @%s", path, update.Username) // ログがうるさければコメントアウト
			broadcastToClients(update)
		}
		stateMutex.Unlock()
	}
}

// --- データ解決ロジック ---

func resolveUser(userID string) string {
	mapMutex.RLock()
	name, ok := userMap[userID]
	mapMutex.RUnlock()
	
	if ok {
		// 名簿にある(普通のユーザー)
		return name
	}
	
	// 名簿にない -> Webhookとみなして固定文字列を返す
	// クライアント側でこれを受け取ったら固定画像を表示する
	return "webhook"
}

// ... fetchData, resolveChannelPath, handleConnections, broadcastToClients は以前と同じなので省略可 ...
// (以前のコードの fetchData以降 をそのまま使ってください。fetchSingleUserは削除してOKです)

func fetchData() error {
	client := &http.Client{}

	// チャンネル
	reqCh, _ := http.NewRequest("GET", "https://q.trap.jp/api/v3/channels?include-public=true", nil)
	reqCh.Header.Set("Authorization", "Bearer "+botToken)
	respCh, err := client.Do(reqCh)
	if err != nil {
		return err
	}
	defer respCh.Body.Close()

	var dataCh ChannelsResponse
	if err := json.NewDecoder(respCh.Body).Decode(&dataCh); err != nil {
		return fmt.Errorf("decode channels error: %w", err)
	}

	// ユーザー
	reqUser, _ := http.NewRequest("GET", "https://q.trap.jp/api/v3/users?include-suspended=true", nil)
	reqUser.Header.Set("Authorization", "Bearer "+botToken)
	respUser, err := client.Do(reqUser)
	if err != nil {
		return err
	}
	defer respUser.Body.Close()

	var dataUser []User
	if err := json.NewDecoder(respUser.Body).Decode(&dataUser); err != nil {
		return fmt.Errorf("decode users error: %w", err)
	}

	mapMutex.Lock()
	defer mapMutex.Unlock()
	
	for _, ch := range dataCh.Public {
		channelMap[ch.ID] = ch
	}
	for _, u := range dataUser {
		userMap[u.ID] = u.Name
	}
	
	log.Printf("✅ Data Loaded: %d channels, %d users", len(channelMap), len(userMap))
	return nil
}

func resolveChannelPath(channelID string) string {
	mapMutex.RLock()
	defer mapMutex.RUnlock()

	path := ""
	currentID := channelID

	for {
		ch, ok := channelMap[currentID]
		if !ok {
			return "" 
		}
		path = "/" + ch.Name + path
		if ch.ParentID == "" || ch.ParentID == "00000000-0000-0000-0000-000000000000" {
			break
		}
		currentID = ch.ParentID
	}
	return "/channels" + path
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer ws.Close()

	clientsMu.Lock()
	clients[ws] = true
	clientsMu.Unlock()

	stateMutex.RLock()
	initMsg := ExtensionInit{
		Type:  "INIT",
		State: lastSpeakers,
	}
	stateMutex.RUnlock()
	ws.WriteJSON(initMsg)

	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			clientsMu.Lock()
			delete(clients, ws)
			clientsMu.Unlock()
			break
		}
	}
}

func broadcastToClients(data interface{}) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for client := range clients {
		if err := client.WriteJSON(data); err != nil {
			client.Close()
			delete(clients, client)
		}
	}
}