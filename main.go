package main

import (
	"embed"
	"encoding/json"
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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/cors"
)

const (
	Version     = "1.3.3"
	MaxFileSize = int64(50 << 20) // 50 MB
	UploadDir   = "uploads"
)

//go:embed public
var staticFiles embed.FS

var (
	startTime = time.Now()
	clients   = make(map[*websocket.Conn]string)
	clientsMu sync.RWMutex

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
	count := len(clients)
	clientsMu.Unlock()

	// 发送初始化消息
	conn.WriteMessage(websocket.TextMessage, mustMarshal(map[string]interface{}{
		"type":   "init",
		"userId": userID,
	}))

	// 广播上线
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
		newCount := len(clients)
		clientsMu.Unlock()

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
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
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

	err := r.ParseMultipartForm(MaxFileSize)
	if err != nil {
		http.Error(w, "File too large (max 50MB)", http.StatusBadRequest)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "No file uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if handler.Size > MaxFileSize {
		http.Error(w, "File too large (max 50MB)", http.StatusBadRequest)
		return
	}

	ext := filepath.Ext(handler.Filename)
	if ext == "" {
		http.Error(w, "Invalid file", http.StatusBadRequest)
		return
	}

	savedName := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	savePath := filepath.Join(UploadDir, savedName)

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

	filePath := filepath.Join(UploadDir, savedName)
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

func ensureUploadDir() {
	if _, err := os.Stat(UploadDir); os.IsNotExist(err) {
		os.MkdirAll(UploadDir, 0755)
	}
}

func main() {

	os.MkdirAll(UploadDir, 0755)
	rand.Seed(time.Now().UnixNano())
	ensureUploadDir()

	localIP := getLocalIP()
	port := "3027"
	addr := ":" + port

	// 静态文件（含 files.html）
	// fs := http.FileServer(http.Dir("./public"))
	// http.Handle("/", http.StripPrefix("/", fs))

	// // 静态文件
	// http.Handle("/", http.FileServer(http.FS(staticFiles)))

	// 关键：将 staticFiles 的 "public" 子目录作为根
	publicFS, err := fs.Sub(staticFiles, "public")
	if err != nil {
		panic(err)
	}

	// 现在 / -> public/ 内容
	http.Handle("/", http.FileServer(http.FS(publicFS)))

	// API
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/send", sendHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/api/files", listFilesHandler)
	http.HandleFunc("/api/files/", deleteFileHandler)
	http.HandleFunc("/info", infoHandler)

	// 文件下载
	http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(UploadDir))))

	handler := cors.AllowAll().Handler(http.DefaultServeMux)

	fmt.Println("🚀 聊天服务已启动")
	fmt.Printf("   WebSocket: ws://%s:%s/ws\n", localIP, port)
	fmt.Printf("   发送消息:  POST http://%s:%s/send\n", localIP, port)
	fmt.Printf("   上传文件:  POST http://%s:%s/upload\n", localIP, port)
	fmt.Printf("   服务信息:  GET  http://%s:%s/info\n", localIP, port)
	fmt.Printf("   文件管理:  http://%s:%s/files.html\n", localIP, port)
	fmt.Printf("   前端页面:   http://%s:%s/\n", localIP, port)
	fmt.Println("   按 Ctrl+C 停止服务")

	log.Fatal(http.ListenAndServe(addr, handler))
}
