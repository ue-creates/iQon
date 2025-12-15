package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	traqwsbot "github.com/traPtitech/traq-ws-bot"
	"github.com/traPtitech/traq-ws-bot/payload"
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
	State map[string]string `json:"state"` // Path -> Username
}

// --- グローバル変数 ---

var (
	channelMap = make(map[string]Channel)
	userMap    = make(map[string]string)
	mapMutex   sync.RWMutex

	lastSpeakers = make(map[string]string)
	stateMutex   sync.RWMutex

	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex

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

	// 1. 起動時にAPIを2回だけ叩いて全データをメモリに乗せる
	if err := fetchData(token); err != nil {
		log.Fatalf("Failed to fetch initial data: %v", err)
	}

	// 2. traQ Bot (WebSocket Mode) の設定
	bot, err := traqwsbot.NewBot(&traqwsbot.Options{
		AccessToken: token,
	})
	if err != nil {
		log.Fatal(err)
	}

	// メッセージ受信時の処理
	bot.OnMessageCreated(func(p *payload.MessageCreated) {
		// メモリ上の辞書から名前解決 (API通信なし)
		username := resolveUser(p.Message.User.ID)
		if username == "" {
			// 新入部員などで辞書にない場合は無視するか、必要ならここで単発fetchを入れる
			return 
		}

		path := resolveChannelPath(p.Message.ChannelID)
		if path == "" {
			return
		}

		// 状態更新
		stateMutex.Lock()
		lastSpeakers[path] = username
		stateMutex.Unlock()

		log.Printf("📢 Update: %s -> @%s", path, username)

		// 拡張機能へブロードキャスト
		broadcastToClients(ExtensionUpdate{
			Type:        "UPDATE",
			ChannelPath: path,
			Username:    username,
		})
	})

	// Bot接続開始
	go func() {
		log.Println("🤖 Starting traQ Bot client...")
		if err := bot.Start(); err != nil {
			log.Fatal(err)
		}
	}()

	// 3. HTTPサーバー (拡張機能用WebSocket + ヘルスチェック)
	http.HandleFunc("/ws", handleConnections)
	
	// NeoShowcaseなどの死活監視用
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("🚀 Server started on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

// --- ヘルパー関数 ---

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