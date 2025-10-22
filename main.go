package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/cors"
)

const Version = "1.3.6"

// 新增：支持人类可读单位的 ByteSize 类型
type ByteSize int64

func (b *ByteSize) String() string {
	return strconv.FormatInt(int64(*b), 10)
}

func (b *ByteSize) Set(value string) error {
	if value == "" {
		return errors.New("size cannot be empty")
	}
	value = strings.ToLower(strings.TrimSpace(value))

	var multiplier int64 = 1
	switch {
	case strings.HasSuffix(value, "g"):
		multiplier = 1 << 30
		value = strings.TrimSuffix(value, "g")
	case strings.HasSuffix(value, "m"):
		multiplier = 1 << 20
		value = strings.TrimSuffix(value, "m")
	case strings.HasSuffix(value, "k"):
		multiplier = 1 << 10
		value = strings.TrimSuffix(value, "k")
	}

	num, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("invalid size: %s", value)
	}

	*b = ByteSize(num * float64(multiplier))
	return nil
}

// 全局配置变量（由 flag 解析）
var (
	port      = flag.Int("port", 3027, "服务监听端口")
	uploadDir = flag.String("upload-dir", "uploads", "文件上传目录")
	maxSize   = ByteSize(50 << 20) // 默认 50 MiB
)

//go:embed public
var staticFiles embed.FS

var (
	startTime = time.Now()
	clients   = make(map[*websocket.Conn]string)
	clientsMu sync.RWMutex

	// 反向索引：userId -> conn，用于精确转发信令到目标对端
	userIdToConn = make(map[string]*websocket.Conn)

	fileList = make(map[string]FileInfo)
	filesMu  sync.RWMutex
)

type Message struct {
	Text string `json:"text"`
	From string `json:"from"`
	Time string `json:"time"`
}

type WSMessage struct {
	Type string  `json:"type"`
	Data Message `json:"data"`
}

type ServiceInfo struct {
	Version     string `json:"version"`
	StartTime   string `json:"startTime"`
	Uptime      string `json:"uptime"`
	OnlineUsers int    `json:"onlineUsers"`
}

type FileInfo struct {
	Name      string    `json:"name"`
	SavedName string    `json:"savedName"`
	Size      int64     `json:"size"`
	Uploaded  time.Time `json:"uploaded"`
	URL       string    `json:"url"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func printLogo() {
	logo := `
  _____        _____ _           _
 / ____|      / ____| |         | |
| |  __  ___ | |    | |__   __ _| |_ ___
| | |_ |/ _ \| |    | '_ \ / _` + "`" + ` | __/ __|
| |__| | (_) | |____| | | | (_| | |_\__ \
 \_____|\___/ \_____|_| |_|\__,_|\__|___/

         Real-time Chat & File Sharing
                 Version %s
`
	fmt.Printf(logo, Version)
}

func getLocalIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil && !strings.HasPrefix(ip4.String(), "172.17.") {
					return ip4.String()
				}
			}
		}
	}

	return "127.0.0.1"
}

func generateUserID() string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func broadcast(msg WSMessage) {
	clientsMu.RLock()
	defer clientsMu.RUnlock()

	data, _ := json.Marshal(msg)
	for client := range clients {
		if err := client.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("广播失败: %v", err)
		}
	}
}

// 简易信令消息结构（用于 WebRTC 建链）
type SignalMessage struct {
	Type    string                 `json:"type"`    // offer/answer/candidate
	From    string                 `json:"from"`    // 发送者 userId
	To      string                 `json:"to"`      // 目标 userId
	Payload map[string]interface{} `json:"payload"` // SDP/ICE
}

func forwardSignal(toUserId string, payload interface{}) error {
	clientsMu.RLock()
	defer clientsMu.RUnlock()
	conn := userIdToConn[toUserId]
	if conn == nil {
		return fmt.Errorf("target user %s not found", toUserId)
	}
	data, _ := json.Marshal(payload)
	return conn.WriteMessage(websocket.TextMessage, data)
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}
	defer conn.Close()

	userID := generateUserID()

	clientsMu.Lock()
	clients[conn] = userID
	userIdToConn[userID] = conn
	count := len(clients)
	// 更新在线用户列表
	var users []string
	for _, uid := range clients {
		users = append(users, uid)
	}
	clientsMu.Unlock()

	conn.WriteMessage(websocket.TextMessage, mustMarshal(map[string]interface{}{
		"type":   "init",
		"userId": userID,
	}))
	broadcast(WSMessage{Type: "users", Data: Message{Text: strings.Join(users, ","), From: "system", Time: time.Now().Format("15:04:05")}})

	now := time.Now().Format("15:04:05")
	broadcast(WSMessage{
		Type: "message",
		Data: Message{
			Text: fmt.Sprintf("👥 用户 %s 上线，当前在线: %d", userID, count),
			From: "system",
			Time: now,
		},
	})

	log.Printf("👥 用户 %s 上线，当前在线: %d", userID, count)

	defer func() {
		clientsMu.Lock()
		delete(clients, conn)
		delete(userIdToConn, userID)
		newCount := len(clients)
		// 更新在线用户列表
		var users []string
		for _, uid := range clients {
			users = append(users, uid)
		}
		clientsMu.Unlock()

		broadcast(WSMessage{Type: "users", Data: Message{Text: strings.Join(users, ","), From: "system", Time: time.Now().Format("15:04:05")}})
		broadcast(WSMessage{
			Type: "message",
			Data: Message{
				Text: fmt.Sprintf("👋 用户 %s 离线，当前在线: %d", userID, newCount),
				From: "system",
				Time: time.Now().Format("15:04:05"),
			},
		})
		log.Printf("👋 用户 %s 离线，当前在线: %d", userID, newCount)
	}()

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			break
		}
		// 解析消息封装
		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msgBytes, &envelope); err == nil && envelope.Type == "signal" {
			var s SignalMessage
			if err := json.Unmarshal(envelope.Data, &s); err == nil && s.Type != "" && s.To != "" {
				// 添加来源（如前端未填充）
				if s.From == "" {
					s.From = userID
				}
				payload := map[string]interface{}{
					"type": "signal",
					"data": s,
				}
				if err := forwardSignal(s.To, payload); err != nil {
					log.Printf("转发信令失败: %v", err)
				}
			}
		}
	}
}

func sendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Message string `json:"message"`
		From    string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Message == "" || req.From == "" {
		http.Error(w, "Missing 'message' or 'from'", http.StatusBadRequest)
		return
	}

	now := time.Now().Format("15:04:05")
	broadcast(WSMessage{
		Type: "message",
		Data: Message{
			Text: req.Message,
			From: req.From,
			Time: now,
		},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 使用配置的 maxSize 限制
	err := r.ParseMultipartForm(int64(maxSize))
	if err != nil {
		http.Error(w, fmt.Sprintf("File too large (max %.1f MB)", float64(maxSize)/(1<<20)), http.StatusBadRequest)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "No file uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if handler.Size > int64(maxSize) {
		http.Error(w, fmt.Sprintf("File too large (max %.1f MB)", float64(maxSize)/(1<<20)), http.StatusBadRequest)
		return
	}

	ext := filepath.Ext(handler.Filename)
	if ext == "" {
		http.Error(w, "Invalid file", http.StatusBadRequest)
		return
	}

	savedName := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	savePath := filepath.Join(*uploadDir, savedName)

	out, err := os.Create(savePath)
	if err != nil {
		log.Printf("保存文件失败: %v", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, file)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	info := FileInfo{
		Name:      handler.Filename,
		SavedName: savedName,
		Size:      handler.Size,
		Uploaded:  time.Now(),
		URL:       "/files/" + savedName,
	}

	filesMu.Lock()
	fileList[savedName] = info
	filesMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"fileUrl":  info.URL,
		"fileName": info.Name,
		"fileSize": info.Size,
	})
}

func listFilesHandler(w http.ResponseWriter, r *http.Request) {
	filesMu.RLock()
	list := make([]FileInfo, 0, len(fileList))
	for _, f := range fileList {
		list = append(list, f)
	}
	filesMu.RUnlock()

	sort.Slice(list, func(i, j int) bool {
		return list[i].Uploaded.After(list[j].Uploaded)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func deleteFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.URL.Path[len("/api/files/"):]
	savedName := filepath.Base(path)
	if savedName == "" || strings.Contains(savedName, "..") || !strings.Contains(path, savedName) {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	filesMu.RLock()
	_, exists := fileList[savedName]
	filesMu.RUnlock()

	if !exists {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	filePath := filepath.Join(*uploadDir, savedName)
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		log.Printf("删除文件失败 %s: %v", filePath, err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	filesMu.Lock()
	delete(fileList, savedName)
	filesMu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func infoHandler(w http.ResponseWriter, r *http.Request) {
	clientsMu.RLock()
	online := len(clients)
	clientsMu.RUnlock()

	uptime := time.Since(startTime).Round(time.Second)
	uptimeStr := fmt.Sprintf("%v", uptime)

	info := ServiceInfo{
		Version:     Version,
		StartTime:   startTime.Format(time.RFC3339),
		Uptime:      uptimeStr,
		OnlineUsers: online,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func main() {
	printLogo()
	// 解析命令行参数
	flag.Var(&maxSize, "max-size", "单文件最大大小，支持 100M、2G、0.5G 或字节数（默认 50M）")
	flag.Parse()

	// 创建上传目录（使用配置值）
	if err := os.MkdirAll(*uploadDir, 0755); err != nil {
		log.Fatalf("❌ 无法创建上传目录 %s: %v", *uploadDir, err)
	}

	rand.Seed(time.Now().UnixNano())
	localIP := getLocalIP()
	addr := fmt.Sprintf(":%d", *port)

	// 静态资源
	publicFS, err := fs.Sub(staticFiles, "public")
	if err != nil {
		panic(err)
	}
	http.Handle("/", http.FileServer(http.FS(publicFS)))

	// API 路由
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/send", sendHandler)
	// （保留原上传接口用于兼容），但推荐使用 WebRTC P2P 传输
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/api/files", listFilesHandler)
	http.HandleFunc("/api/files/", deleteFileHandler)
	http.HandleFunc("/info", infoHandler)

	// 文件下载服务（使用配置的 uploadDir）
	http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(*uploadDir))))

	handler := cors.AllowAll().Handler(http.DefaultServeMux)

	fmt.Println("🚀 聊天服务已启动")
	fmt.Printf("   WebSocket: ws://%s:%d/ws\n", localIP, *port)
	fmt.Printf("   发送消息:  POST http://%s:%d/send\n", localIP, *port)
	fmt.Printf("   上传文件:  POST http://%s:%d/upload\n", localIP, *port)
	fmt.Printf("   服务信息:  GET  http://%s:%d/info\n", localIP, *port)
	fmt.Printf("   文件管理:  http://%s:%d/files.html\n", localIP, *port)
	fmt.Printf("   前端页面:   http://%s:%d/\n", localIP, *port)
	fmt.Println("   按 Ctrl+C 停止服务")
	fmt.Printf("   配置: 端口=%d, 上传目录=%s, 最大大小=%.1f MB\n", *port, *uploadDir, float64(maxSize)/(1<<20))

	log.Fatal(http.ListenAndServe(addr, handler))
}
