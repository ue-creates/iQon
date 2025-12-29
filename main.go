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
	Bot  bool   `json:"bot"`
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

	// データキャッシュ (名簿)
	channelMap = make(map[string]Channel)
	userMap    = make(map[string]string)
	mapMutex   sync.RWMutex

	// 現在の状態 (Path -> Username)
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
	if err := fetchInitialData(); err != nil {
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
	log.Printf("🚀 Server started on :%s (Auto-Learning Mode)", port)
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
		// 全パブリックチャンネルのアクテビティを取得
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

	// APIは新しい順に来るので、逆順（古い順）に処理
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]

		// すでに処理済みの時刻以前ならスキップ
		if !msg.CreatedAt.After(lastCheckTime) {
			continue
		}
		// 最新時刻の更新
		if msg.CreatedAt.After(newestInBatch) {
			newestInBatch = msg.CreatedAt
		}

		// ★ ここで学習機能付きの解決関数を呼ぶ
		username := resolveUser(msg.UserID)
		path := resolveChannelPath(msg.ChannelID)

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
			log.Printf("📢 Update: %s -> @%s", path, update.Username)
			broadcastToClients(update)
		}
		stateMutex.Unlock()
	}
}

// --- 学習機能付き解決ロジック ---

// resolveUser: キャッシュになければAPIから取得して登録する
func resolveUser(userID string) string {
	// 1. キャッシュチェック (Read Lock)
	mapMutex.RLock()
	name, ok := userMap[userID]
	mapMutex.RUnlock()
	if ok {
		return name
	}

	// 2. キャッシュになければAPIへ問い合わせ
	// (ロックを外してから通信する)
	log.Printf("🔍 Unknown UserID: %s. Fetching...", userID)
	
	newUser, err := fetchSingleUser(userID)
	
	// 3. 結果を登録 (Write Lock)
	mapMutex.Lock()
	defer mapMutex.Unlock()

	// 通信中に別のゴルーチンが書き込んだかもしれないので再チェック
	if name, exists := userMap[userID]; exists {
		return name
	}

	if err != nil {
		log.Printf("⚠️ User fetch failed (%v). Treating as webhook.", err)
		// 取得に失敗したら "webhook" として登録し、次回以降のエラーを防ぐ
		userMap[userID] = "webhook"
		return "webhook"
	}

	userMap[userID] = newUser.Name
	log.Printf("✅ Learned User: %s -> @%s", userID, newUser.Name)
	return newUser.Name
}

// resolveChannelPath: 親も含めてパスを解決。知らなければ取得して登録する
func resolveChannelPath(channelID string) string {
	// パス構築用の一時キャッシュとして使うマップのコピーを持つのは非効率なので、
	// 毎回親をたどる方式にする。足りない親がいればその都度fetchする。

	path := ""
	currentID := channelID

	for {
		// 1. キャッシュチェック
		mapMutex.RLock()
		ch, ok := channelMap[currentID]
		mapMutex.RUnlock()

		// 2. 知らないチャンネルならAPIから取得
		if !ok {
			log.Printf("🔍 Unknown ChannelID: %s. Fetching...", currentID)
			fetchedCh, err := fetchSingleChannel(currentID)
			
			mapMutex.Lock()
			if err != nil {
				mapMutex.Unlock()
				log.Printf("❌ Failed to fetch channel %s: %v", currentID, err)
				return "" // 解決不能
			}
			// 登録
			channelMap[currentID] = *fetchedCh
			ch = *fetchedCh
			mapMutex.Unlock()
			log.Printf("✅ Learned Channel: %s", ch.Name)
		}

		// パスを積み上げ
		path = "/" + ch.Name + path

		// ルートまで来たら終了
		if ch.ParentID == "" || ch.ParentID == "00000000-0000-0000-0000-000000000000" {
			break
		}
		currentID = ch.ParentID
	}

	return "/channels" + path
}

// --- 単発取得用APIクライアント ---

func fetchSingleUser(userID string) (*User, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("https://q.trap.jp/api/v3/users/%s", userID)
	
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

func fetchSingleChannel(channelID string) (*Channel, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("https://q.trap.jp/api/v3/channels/%s", channelID)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var ch Channel
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// --- 初期データ一括取得 (起動時用) ---

func fetchInitialData() error {
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

	// ユーザー (include-suspended=trueで凍結ユーザーも取得)
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