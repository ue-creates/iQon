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

// traQ API: チャンネル情報
type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parentId"`
}

// traQ API: ユーザー情報
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// traQ API: アクテビティタイムラインのメッセージ
type ActivityMessage struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	ChannelID string    `json:"channelId"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type ChannelsResponse struct {
	Public []Channel `json:"public"`
}

// 拡張機能へ送るデータ
type ExtensionUpdate struct {
	Type        string `json:"type"`
	ChannelPath string `json:"channelPath"`
	Username    string `json:"username"`
}

type ExtensionInit struct {
	Type  string            `json:"type"`
	State map[string]string `json:"state"` // Path -> Username
}

// --- グローバル変数 ---

var (
	// データキャッシュ
	channelMap = make(map[string]Channel)
	userMap    = make(map[string]string) // UserID -> UserName
	mapMutex   sync.RWMutex

	// 状態保持 (Path -> Username)
	lastSpeakers = make(map[string]string)
	stateMutex   sync.RWMutex

	// WebSocketクライアント管理
	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex

	// ポーリング制御用
	lastCheckTime time.Time

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func main() {
	// 環境変数チェック
	token := os.Getenv("TRAQ_BOT_TOKEN")
	if token == "" {
		log.Fatal("ERROR: TRAQ_BOT_TOKEN is not set")
	}

	// 1. 起動時にチャンネルとユーザー情報を全取得
	log.Println("⏳ Fetching initial data...")
	if err := fetchData(token); err != nil {
		log.Fatalf("Failed to fetch initial data: %v", err)
	}

	// 起動時刻を記録（これより前のメッセージは無視する）
	lastCheckTime = time.Now().UTC()

	// 2. ポーリング開始 (ゴルーチンでバックグラウンド実行)
	// 招待不要で全チャンネルを見るため、/activity/timeline を定期監視します
	go startPolling(token)

	// 3. 拡張機能用WebSocketサーバー & ヘルスチェック
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

// --- ポーリング処理 (核心部分) ---

func startPolling(token string) {
	// 15秒に1回 API を叩く (API制限は 10秒に50回程度なので余裕です)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	client := &http.Client{Timeout: 5 * time.Second}

	log.Println("👀 Polling started: Watching all public channels...")

	for range ticker.C {
		// API: 全パブリックチャンネルのタイムラインを取得
		// limit=50: 直近50件取れば3秒間の会話は網羅できるはず
		url := "https://q.trap.jp/api/v3/activity/timeline?all=true&limit=50"
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)

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

		// 新着メッセージの処理
		processTimeline(timeline)
	}
}

func processTimeline(messages []ActivityMessage) {
	if len(messages) == 0 {
		return
	}

	// 今回取得した中で一番新しい時刻
	newestInBatch := lastCheckTime

	// チャンネルごとの「最新の1件」だけを保存するマップ
	// (3秒間に同じチャンネルで連投があっても、最後の1回だけ送ればいいため)
	updates := make(map[string]ExtensionUpdate)

	// APIは新しい順で返ってくることが多いが、念のためすべてチェック
	// 古いメッセージ -> 新しいメッセージ の順に処理したいので逆順にするか、マップで上書きする
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]

		// すでにチェック済みの時刻以前ならスキップ
		if !msg.CreatedAt.After(lastCheckTime) {
			continue
		}

		// 時刻更新
		if msg.CreatedAt.After(newestInBatch) {
			newestInBatch = msg.CreatedAt
		}

		// 必要な情報を解決
		username := resolveUser(msg.UserID)
		path := resolveChannelPath(msg.ChannelID)

		if username == "" || path == "" {
			continue
		}

		// 更新用マップに登録 (同じパスなら上書きされる＝最新が残る)
		updates[path] = ExtensionUpdate{
			Type:        "UPDATE",
			ChannelPath: path,
			Username:    username,
		}
	}

	// グローバルな時刻を更新
	lastCheckTime = newestInBatch

	// まとめて送信
	if len(updates) > 0 {
		stateMutex.Lock()
		for path, update := range updates {
			// サーバーのメモリ状態も更新
			lastSpeakers[path] = update.Username
			// ログ出力
			log.Printf("📢 Polled: %s -> @%s", path, update.Username)
			// 送信
			broadcastToClients(update)
		}
		stateMutex.Unlock()
	}
}

// --- ヘルパー関数 (前回と同じ) ---

func fetchData(token string) error {
	client := &http.Client{}

	// チャンネル取得
	reqCh, _ := http.NewRequest("GET", "https://q.trap.jp/api/v3/channels?include-public=true", nil)
	reqCh.Header.Set("Authorization", "Bearer "+token)
	respCh, err := client.Do(reqCh)
	if err != nil {
		return err
	}
	defer respCh.Body.Close()

	var dataCh ChannelsResponse
	if err := json.NewDecoder(respCh.Body).Decode(&dataCh); err != nil {
		return fmt.Errorf("decode channels error: %w", err)
	}

	// ユーザー取得
	reqUser, _ := http.NewRequest("GET", "https://q.trap.jp/api/v3/users?include-suspended=false", nil)
	reqUser.Header.Set("Authorization", "Bearer "+token)
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

func resolveUser(userID string) string {
	mapMutex.RLock()
	defer mapMutex.RUnlock()
	return userMap[userID]
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