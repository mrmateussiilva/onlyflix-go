package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func loadEnv() {
	b, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			os.Setenv(parts[0], parts[1])
		}
	}
}

func secure(h http.HandlerFunc, authUser, authPass string) http.HandlerFunc {
	if authUser == "" || authPass == "" {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		qu := r.URL.Query().Get("username")
		if qu == "" {
			qu = r.URL.Query().Get("user")
		}
		qp := r.URL.Query().Get("password")
		if qp == "" {
			qp = r.URL.Query().Get("pass")
		}
		if qu == authUser && qp == authPass {
			h(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if ok && u == authUser && p == authPass {
			h(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

type webCredentials struct {
	Web struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		AuthURI      string `json:"auth_uri"`
		TokenURI     string `json:"token_uri"`
	} `json:"web"`
}

type breadcrumbItem struct {
	Name string
	Id   string
}

type fileItem struct {
	Name      string
	Id        string
	MimeType  string
	Size      string
	Created   string
	TagClass  string
	TagLabel  string
	ViewURL   string
	IsFolder  bool
	Thumbnail string
}

type pageData struct {
	Breadcrumb  []breadcrumbItem
	FolderName  string
	CurrentID   string
	Folders     []fileItem
	Files       []fileItem
	ViewType    string
	ViewFile    *fileItem
	ViewURL     string
	VideoMime   string
	ImageMime   string
	SearchQuery string
}

type SyncVideo struct {
	Id      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

type SyncFolder struct {
	Id     string      `json:"id"`
	Name   string      `json:"name"`
	Videos []SyncVideo `json:"videos"`
}

type SyncCatalog struct {
	LastSync   time.Time    `json:"last_sync"`
	Folders    []SyncFolder `json:"folders"`
	RootVideos []SyncVideo  `json:"root_videos"`
}

type DownloadStatus string

const (
	StatusPending     DownloadStatus = "pending"
	StatusDownloading DownloadStatus = "downloading"
	StatusCompleted   DownloadStatus = "completed"
	StatusFailed      DownloadStatus = "failed"
)

type DownloadRecord struct {
	Id       string         `json:"id"`
	Name     string         `json:"name"`
	Status   DownloadStatus `json:"status"`
	AddedAt  time.Time      `json:"added_at"`
	UpdateAt time.Time      `json:"update_at"`
}

var (
	catalogCache    *SyncCatalog
	cacheMutex      sync.RWMutex
	downloadHistory = make(map[string]*DownloadRecord)
	historyMutex    sync.RWMutex
	downloadQueue   = make(chan string, 10000)
)

func loadHistory() {
	b, err := os.ReadFile("download_history.json")
	if err == nil {
		historyMutex.Lock()
		json.Unmarshal(b, &downloadHistory)
		for _, v := range downloadHistory {
			if v.Status == StatusDownloading {
				v.Status = StatusPending
			}
		}
		historyMutex.Unlock()
	}
}

func saveHistory() {
	historyMutex.RLock()
	b, _ := json.MarshalIndent(downloadHistory, "", "  ")
	historyMutex.RUnlock()
	os.WriteFile("download_history.json", b, 0644)
}

func updateHistoryStatus(id string, name string, status DownloadStatus) {
	historyMutex.Lock()
	record, exists := downloadHistory[id]
	if !exists {
		record = &DownloadRecord{
			Id:      id,
			Name:    name,
			AddedAt: time.Now(),
		}
		downloadHistory[id] = record
	}
	record.Status = status
	record.UpdateAt = time.Now()
	historyMutex.Unlock()
	saveHistory()
}

func extractFolderID(url string) string {
	parts := strings.Split(url, "/")
	for i, part := range parts {
		if part == "folders" && i+1 < len(parts) {
			return strings.Split(parts[i+1], "?")[0]
		}
	}
	return ""
}

func randState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func findPort(start int) (int, error) {
	for port := start; port <= 65535; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		l.Close()
		return port, nil
	}
	return 0, fmt.Errorf("nenhuma porta disponivel a partir de %d", start)
}

func getTokenManual(config *oauth2.Config) *oauth2.Token {
	state := randState()
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline)

	fmt.Println("\n========== AUTENTICACAO ==========")
	fmt.Println("1. Abra este link no navegador:")
	fmt.Println(authURL)
	fmt.Println("2. Faca login e autorize o acesso")
	fmt.Println("3. Apos autorizar, o navegador vai tentar abrir localhost (vai falhar)")
	fmt.Println("4. Copie a URL COMPLETA da barra de endereco e cole abaixo")
	fmt.Println("===================================")
	fmt.Print("\nCole a URL aqui: ")

	var redirectURL string
	fmt.Scanln(&redirectURL)

	code := extractCode(redirectURL)
	if code == "" {
		log.Fatal("Nao foi possivel extrair o codigo da URL.")
	}

	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		log.Fatalf("Erro ao trocar codigo por token: %v", err)
	}

	return tok
}

func extractCode(redirectURL string) string {
	if idx := strings.Index(redirectURL, "code="); idx >= 0 {
		after := redirectURL[idx+5:]
		if end := strings.IndexAny(after, "& "); end >= 0 {
			return after[:end]
		}
		return after
	}
	return ""
}

func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	port, err := findPort(8080)
	if err != nil {
		log.Fatalf("Erro ao achar porta disponivel: %v", err)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		log.Fatalf("Erro ao iniciar servidor local: %v", err)
	}
	config.RedirectURL = fmt.Sprintf("http://localhost:%d", port)

	state := randState()
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline)

	codeCh := make(chan string)
	stateCh := make(chan string)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		codeCh <- r.URL.Query().Get("code")
		stateCh <- r.URL.Query().Get("state")
		w.Write([]byte("<html><body><h1>Autenticado!</h1><p>Voce ja pode fechar esta aba.</p></body></html>"))
	})

	server := &http.Server{Handler: mux}
	go server.Serve(listener)

	fmt.Println("Abrindo navegador para autenticacao...")
	openURL(authURL)

	var code, recvState string
	select {
	case code = <-codeCh:
		recvState = <-stateCh
	case <-time.After(2 * time.Minute):
		fmt.Println("Tempo limite excedido. Usando modo manual...")
		server.Shutdown(context.Background())
		return getTokenManual(config)
	}

	server.Shutdown(context.Background())

	if recvState != state {
		log.Fatal("State invalido. Possivel ataque CSRF.")
	}

	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		log.Fatalf("Erro ao trocar codigo por token: %v", err)
	}

	return tok
}

func openURL(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	if err != nil {
		log.Printf("Nao foi possivel abrir o navegador: %v", err)
	}
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, tok *oauth2.Token) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("Erro ao salvar token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(tok)
}

func getClient(config *oauth2.Config) *http.Client {
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		fmt.Println("Token nao encontrado, iniciando autenticacao OAuth...")
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
		fmt.Println("Token salvo com sucesso!")
	}

	ctx := context.Background()

	transport := &oauth2.Transport{
		Source: config.TokenSource(ctx, tok),
		Base: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
	return client
}

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/1024/1024)
}

func tagForMime(mime string) (class, label string) {
	switch {
	case strings.HasPrefix(mime, "video/"):
		return "video", "Video"
	case strings.HasPrefix(mime, "image/"):
		return "image", "Imagem"
	case mime == "application/pdf":
		return "pdf", "PDF"
	case mime == "application/vnd.google-apps.folder":
		return "folder", "Pasta"
	default:
		return "", "Arquivo"
	}
}

func isVideo(mime string) bool {
	return strings.HasPrefix(mime, "video/")
}

func isImage(mime string) bool {
	return strings.HasPrefix(mime, "image/")
}

func cleanName(name string, stripExt bool) string {
	if stripExt {
		if idx := strings.LastIndex(name, "."); idx > 0 {
			name = name[:idx]
		}
	}

	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")

	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	name = b.String()

	fields := strings.Fields(name)
	return strings.Join(fields, " ")
}

func buildBreadcrumb(srv *drive.Service, folderID string, rootID string) []breadcrumbItem {
	var items []breadcrumbItem
	current := folderID

	ctx := context.Background()

	for limit := 0; limit < 20; limit++ {
		f, err := srv.Files.Get(current).Context(ctx).Fields("id, name, parents").Do()
		if err != nil {
			break
		}
		items = append(items, breadcrumbItem{Name: cleanName(f.Name, false), Id: f.Id})

		if f.Id == rootID || len(f.Parents) == 0 {
			break
		}
		current = f.Parents[0]
	}

	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items
}

func isGarbageName(name string) bool {
	noSpace := strings.ReplaceAll(name, " ", "")
	
	if len(name) > 20 && !strings.Contains(name, " ") {
		return true
	}
	
	if strings.Contains(strings.ToLower(name), "source") && len(name) > 15 {
		return true
	}

	digits := 0
	for _, c := range noSpace {
		if unicode.IsDigit(c) {
			digits++
		}
	}
	if len(noSpace) > 10 && float64(digits)/float64(len(noSpace)) > 0.4 {
		return true
	}
	return false
}

func smartRename(videos []SyncVideo, prefix string) {
	counter := 1
	for i, v := range videos {
		if isGarbageName(v.Name) {
			if prefix != "" && prefix != "Geral" {
				videos[i].Name = fmt.Sprintf("%s - Vídeo %02d", prefix, counter)
			} else {
				videos[i].Name = fmt.Sprintf("Vídeo %02d", counter)
			}
			counter++
		}
	}
}

func syncDrive(srv *drive.Service, rootID string) {
	log.Println("[SYNC] Iniciando sincronizacao do Drive...")

	newCatalog := &SyncCatalog{
		LastSync: time.Now(),
	}

	ctx := context.Background()

	query := fmt.Sprintf("'%s' in parents and trashed = false", rootID)
	res, err := srv.Files.List().Q(query).PageSize(1000).
		Fields("files(id, name, mimeType, createdTime)").
		Context(ctx).
		Do()

	if err != nil {
		log.Printf("[SYNC] Erro ao listar raiz: %v", err)
		return
	}

	for _, f := range res.Files {
		if f.MimeType == "application/vnd.google-apps.folder" {
			newCatalog.Folders = append(newCatalog.Folders, SyncFolder{
				Id:     f.Id,
				Name:   cleanName(f.Name, false),
				Videos: []SyncVideo{},
			})
		} else if isVideo(f.MimeType) {
			t, _ := time.Parse(time.RFC3339, f.CreatedTime)
			newCatalog.RootVideos = append(newCatalog.RootVideos, SyncVideo{
				Id:      f.Id,
				Name:    cleanName(f.Name, true),
				Created: t,
			})
		}
	}

	sort.Slice(newCatalog.RootVideos, func(i, j int) bool {
		return newCatalog.RootVideos[i].Created.Before(newCatalog.RootVideos[j].Created)
	})
	smartRename(newCatalog.RootVideos, "Geral")

	for i, folder := range newCatalog.Folders {
		q2 := fmt.Sprintf("'%s' in parents and mimeType contains 'video/' and trashed = false", folder.Id)
		res2, err2 := srv.Files.List().Q(q2).PageSize(1000).
			Fields("files(id, name, createdTime)").
			Context(ctx).
			Do()
		if err2 != nil {
			log.Printf("[SYNC] Erro ao listar pasta %s: %v", folder.Name, err2)
			continue
		}
		for _, f := range res2.Files {
			t, _ := time.Parse(time.RFC3339, f.CreatedTime)
			newCatalog.Folders[i].Videos = append(newCatalog.Folders[i].Videos, SyncVideo{
				Id:      f.Id,
				Name:    cleanName(f.Name, true),
				Created: t,
			})
		}
		sort.Slice(newCatalog.Folders[i].Videos, func(a, b int) bool {
			return newCatalog.Folders[i].Videos[a].Created.Before(newCatalog.Folders[i].Videos[b].Created)
		})
		smartRename(newCatalog.Folders[i].Videos, folder.Name)
	}

	cacheMutex.Lock()
	catalogCache = newCatalog
	cacheMutex.Unlock()

	b, _ := json.MarshalIndent(newCatalog, "", "  ")
	os.WriteFile("catalog.json", b, 0644)

	log.Println("[SYNC] Sincronizacao finalizada com sucesso!")

	enqueue := func(v SyncVideo) {
		historyMutex.RLock()
		record, exists := downloadHistory[v.Id]
		historyMutex.RUnlock()

		if !exists || (record.Status != StatusCompleted && record.Status != StatusDownloading) {
			updateHistoryStatus(v.Id, v.Name, StatusPending)
			downloadQueue <- v.Id
		}
	}

	for _, v := range newCatalog.RootVideos {
		enqueue(v)
	}
	for _, f := range newCatalog.Folders {
		for _, v := range f.Videos {
			enqueue(v)
		}
	}
}

func startDownloadManager(client *http.Client, numWorkers int) {
	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			for fileID := range downloadQueue {
				localPath := filepath.Join("downloads", fileID+".mp4")
				tmpPath := localPath + ".tmp"

				if _, err := os.Stat(localPath); err == nil {
					updateHistoryStatus(fileID, "", StatusCompleted)
					continue
				}

				updateHistoryStatus(fileID, "", StatusDownloading)
				log.Printf("[WORKER-%d] Baixando %s...", workerID, fileID)

				downloadURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?alt=media", fileID)
				req, _ := http.NewRequest("GET", downloadURL, nil)
				resp, err := client.Do(req)
				if err != nil || resp.StatusCode != http.StatusOK {
					if resp != nil {
						resp.Body.Close()
					}
					log.Printf("[WORKER-%d] Falha HTTP para %s: %v", workerID, fileID, err)
					updateHistoryStatus(fileID, "", StatusFailed)
					continue
				}

				out, err := os.Create(tmpPath)
				if err != nil {
					resp.Body.Close()
					updateHistoryStatus(fileID, "", StatusFailed)
					continue
				}

				_, err = io.Copy(out, resp.Body)
				out.Close()
				resp.Body.Close()

				if err != nil {
					log.Printf("[WORKER-%d] Erro I/O %s: %v", workerID, fileID, err)
					os.Remove(tmpPath)
					updateHistoryStatus(fileID, "", StatusFailed)
					continue
				}

				os.Rename(tmpPath, localPath)
				updateHistoryStatus(fileID, "", StatusCompleted)
				log.Printf("[WORKER-%d] %s finalizado!", workerID, fileID)
			}
		}(i)
	}
}

func handleM3U(publicURL, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cacheMutex.RLock()
		cat := catalogCache
		cacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catalogo ainda nao sincronizado. Tente novamente em alguns segundos.", http.StatusServiceUnavailable)
			return
		}

		host := publicURL
		if host == "" {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			host = fmt.Sprintf("%s://%s", scheme, r.Host)
		}

		authQuery := ""
		if authUser != "" && authPass != "" {
			authQuery = fmt.Sprintf("?user=%s&pass=%s", authUser, authPass)
		}

		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Disposition", "attachment; filename=\"onlyflix.m3u\"")

		fmt.Fprintln(w, "#EXTM3U")

		for _, v := range cat.RootVideos {
			fmt.Fprintf(w, "#EXTINF:-1 group-title=\"Geral\", %s\n", v.Name)
			fmt.Fprintf(w, "%s/file/%s%s\n", host, v.Id, authQuery)
		}

		for _, f := range cat.Folders {
			for _, v := range f.Videos {
				fmt.Fprintf(w, "#EXTINF:-1 group-title=\"%s\", %s\n", f.Name, v.Name)
				fmt.Fprintf(w, "%s/file/%s%s\n", host, v.Id, authQuery)
			}
		}
	}
}

func handleXtream(publicURL, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("username")
		p := r.URL.Query().Get("password")
		if authUser != "" && authPass != "" {
			if u != authUser || p != authPass {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"user_info":{"auth":0}}`))
				return
			}
		}

		action := r.URL.Query().Get("action")

		cacheMutex.RLock()
		cat := catalogCache
		cacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catalogo indisponivel", 503)
			return
		}

		host := publicURL
		if host == "" {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			host = fmt.Sprintf("%s://%s", scheme, r.Host)
		}

		w.Header().Set("Content-Type", "application/json")

		if action == "" {
			resp := fmt.Sprintf(`{"user_info":{"username":"%s","password":"%s","message":"Login Success","auth":1,"status":"Active","exp_date":"null","is_trial":"0","active_cons":"1","created_at":"1600000000","max_connections":"99","allowed_output_formats":["m3u8","ts","rtmp"]},"server_info":{"url":"%s","port":"80","https_port":"443","server_protocol":"http","rtmp_port":"1935","timezone":"UTC","timestamp_now":%d,"time_now":"%s"}}`, u, p, host, time.Now().Unix(), time.Now().Format("2006-01-02 15:04:05"))
			w.Write([]byte(resp))
			return
		}

		if action == "get_vod_categories" {
			type XCat struct {
				CategoryId   string `json:"category_id"`
				CategoryName string `json:"category_name"`
				ParentId     int    `json:"parent_id"`
			}
			var cats []XCat
			cats = append(cats, XCat{CategoryId: "root", CategoryName: "Geral", ParentId: 0})
			for _, f := range cat.Folders {
				cats = append(cats, XCat{CategoryId: f.Id, CategoryName: f.Name, ParentId: 0})
			}
			b, _ := json.Marshal(cats)
			w.Write(b)
			return
		}

		if action == "get_vod_streams" {
			catID := r.URL.Query().Get("category_id")
			type XVod struct {
				Num                int    `json:"num"`
				Name               string `json:"name"`
				StreamType         string `json:"stream_type"`
				StreamId           string `json:"stream_id"`
				StreamIcon         string `json:"stream_icon"`
				Rating             int    `json:"rating"`
				Rating5based       int    `json:"rating_5based"`
				Added              string `json:"added"`
				CategoryId         string `json:"category_id"`
				ContainerExtension string `json:"container_extension"`
				CustomSid          string `json:"custom_sid"`
				DirectSource       string `json:"direct_source"`
			}
			var vods []XVod
			counter := 1

			addVod := func(v SyncVideo, cID string) {
				vods = append(vods, XVod{
					Num:                counter,
					Name:               v.Name,
					StreamType:         "movie",
					StreamId:           v.Id,
					CategoryId:         cID,
					ContainerExtension: "mp4",
					Added:              fmt.Sprintf("%d", v.Created.Unix()),
				})
				counter++
			}

			if catID == "root" || catID == "" {
				for _, v := range cat.RootVideos {
					addVod(v, "root")
				}
			}
			if catID == "" {
				for _, f := range cat.Folders {
					for _, v := range f.Videos {
						addVod(v, f.Id)
					}
				}
			} else {
				for _, f := range cat.Folders {
					if f.Id == catID {
						for _, v := range f.Videos {
							addVod(v, f.Id)
						}
						break
					}
				}
			}
			b, _ := json.Marshal(vods)
			w.Write(b)
			return
		}

		w.Write([]byte("[]"))
	}
}

func handleForceSync(srv *drive.Service, rootID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		go syncDrive(srv, rootID)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status": "sync_started", "message": "Sincronizacao iniciada em background"}`))
	}
}

func handleFolder(srv *drive.Service, rootID string, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		folderID := r.PathValue("id")
		if folderID == "" {
			folderID = rootID
		}

		log.Printf("[DEBUG] handleFolder inicio folderID=%s", folderID)

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		log.Printf("[DEBUG] chamando Files.Get...")
		folder, err := srv.Files.Get(folderID).Context(ctx).Fields("id, name").Do()
		if err != nil {
			log.Printf("[DEBUG] Files.Get erro: %v", err)
			log.Printf("Erro ao buscar pasta %s: %v", folderID, err)
			http.Error(w, "Pasta nao encontrada", http.StatusNotFound)
			return
		}
		log.Printf("[DEBUG] Files.Get OK: %s", folder.Name)

		log.Printf("[DEBUG] chamando buildBreadcrumb...")
		breadcrumb := buildBreadcrumb(srv, folderID, rootID)
		log.Printf("[DEBUG] buildBreadcrumb OK")

		query := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
		log.Printf("[DEBUG] chamando Files.List...")
		res, err := srv.Files.List().Q(query).PageSize(100).
			Fields("files(id, name, mimeType, size, createdTime, thumbnailLink)").
			OrderBy("folder,name").
			Context(ctx).
			Do()
		if err != nil {
			log.Printf("[DEBUG] Files.List erro: %v", err)
			log.Printf("Erro ao listar arquivos da pasta %s: %v", folderID, err)
			http.Error(w, "Erro ao listar arquivos", http.StatusInternalServerError)
			return
		}
		log.Printf("[DEBUG] Files.List OK: %d arquivos", len(res.Files))

		var folders, files []fileItem
		for _, f := range res.Files {
			class, label := tagForMime(f.MimeType)
			isFolder := f.MimeType == "application/vnd.google-apps.folder"
			item := fileItem{
				Name:      cleanName(f.Name, !isFolder),
				Id:        f.Id,
				MimeType:  f.MimeType,
				Size:      formatSize(f.Size),
				Created:   f.CreatedTime,
				TagClass:  class,
				TagLabel:  label,
				IsFolder:  isFolder,
				Thumbnail: f.ThumbnailLink,
			}
			if item.IsFolder {
				item.ViewURL = "/folder/" + f.Id
				folders = append(folders, item)
			} else {
				if isVideo(f.MimeType) {
					item.ViewURL = "/view/" + f.Id
				} else if isImage(f.MimeType) {
					item.ViewURL = "/file/" + f.Id
				}
				files = append(files, item)
			}
		}

		if err := tmpl.Execute(w, pageData{
			Breadcrumb: breadcrumb,
			FolderName: cleanName(folder.Name, false),
			CurrentID:  folderID,
			Folders:    folders,
			Files:      files,
			ViewType:   "folder",
		}); err != nil {
			log.Printf("Erro ao renderizar template: %v", err)
			http.Error(w, "Erro ao renderizar pagina", http.StatusInternalServerError)
		}
	}
}

func handleView(srv *drive.Service, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.PathValue("id")
		log.Printf("Carregando view do arquivo: %s", fileID)

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		file, err := srv.Files.Get(fileID).Context(ctx).Fields("id, name, mimeType, size, parents, thumbnailLink").Do()
		if err != nil {
			log.Printf("Erro ao buscar arquivo %s: %v", fileID, err)
			http.Error(w, "Arquivo nao encontrado", http.StatusNotFound)
			return
		}

		parentID := fileID
		if len(file.Parents) > 0 {
			parentID = file.Parents[0]
		}

		breadcrumb := buildBreadcrumb(srv, parentID, "")
		class, label := tagForMime(file.MimeType)

		item := fileItem{
			Name:     cleanName(file.Name, true),
			Id:       file.Id,
			MimeType: file.MimeType,
			Size:     formatSize(file.Size),
			TagClass: class,
			TagLabel: label,
		}

		data := pageData{
			Breadcrumb: breadcrumb,
			FolderName: cleanName(file.Name, true),
			CurrentID:  parentID,
			ViewType:   "video",
			ViewFile:   &item,
			ViewURL:    "/file/" + file.Id,
			VideoMime:  file.MimeType,
		}

		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("Erro ao renderizar template: %v", err)
		}
	}
}

func streamDriveFile(w http.ResponseWriter, r *http.Request, srv *drive.Service, client *http.Client, fileID string) {
	localPath := filepath.Join("downloads", fileID+".mp4")
	if stat, err := os.Stat(localPath); err == nil && !stat.IsDir() {
		log.Printf("[STREAM] Servindo %s nativamente do disco local", fileID)
		http.ServeFile(w, r, localPath)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	file, err := srv.Files.Get(fileID).Context(ctx).Fields("mimeType, size").Do()
	if err != nil {
		log.Printf("Erro ao buscar arquivo %s: %v", fileID, err)
		http.Error(w, "Arquivo nao encontrado", http.StatusNotFound)
		return
	}

	downloadURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?alt=media", fileID)

	req, _ := http.NewRequest("GET", downloadURL, nil)
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Erro ao baixar arquivo", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", file.MimeType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", "inline")

	if resp.StatusCode == http.StatusPartialContent {
		w.Header().Set("Content-Range", resp.Header.Get("Content-Range"))
		w.WriteHeader(http.StatusPartialContent)
	}

	io.Copy(w, resp.Body)
}

func handleFile(srv *drive.Service, client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.PathValue("id")
		log.Printf("Servindo arquivo: %s", fileID)
		streamDriveFile(w, r, srv, client, fileID)
	}
}

func handleXtreamFile(srv *drive.Service, client *http.Client, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.PathValue("user")
		p := r.PathValue("pass")
		if authUser != "" && authPass != "" {
			if u != authUser || p != authPass {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		fileID := r.PathValue("file")
		if idx := strings.LastIndex(fileID, "."); idx > 0 {
			fileID = fileID[:idx]
		}

		log.Printf("Servindo Xtream arquivo: %s", fileID)
		streamDriveFile(w, r, srv, client, fileID)
	}
}

func sanitizeQuery(q string) string {
	q = strings.ReplaceAll(q, "'", "")
	q = strings.ReplaceAll(q, "\\", "")
	q = strings.ReplaceAll(q, "\"", "")
	return strings.TrimSpace(q)
}

func handleSearch(srv *drive.Service, rootID string, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := sanitizeQuery(r.URL.Query().Get("q"))
		if q == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		log.Printf("Buscando: %s", q)

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		query := fmt.Sprintf("name contains '%s' and trashed = false", q)
		res, err := srv.Files.List().Q(query).PageSize(50).
			Fields("files(id, name, mimeType, size, createdTime, thumbnailLink)").
			OrderBy("folder,name").
			Context(ctx).
			Do()
		if err != nil {
			log.Printf("Erro ao buscar '%s': %v", q, err)
			http.Error(w, "Erro ao buscar", http.StatusInternalServerError)
			return
		}

		var folders, files []fileItem
		for _, f := range res.Files {
			class, label := tagForMime(f.MimeType)
			isFolder := f.MimeType == "application/vnd.google-apps.folder"
			item := fileItem{
				Name:      cleanName(f.Name, !isFolder),
				Id:        f.Id,
				MimeType:  f.MimeType,
				Size:      formatSize(f.Size),
				Created:   f.CreatedTime,
				TagClass:  class,
				TagLabel:  label,
				IsFolder:  isFolder,
				Thumbnail: f.ThumbnailLink,
			}
			if item.IsFolder {
				item.ViewURL = "/folder/" + f.Id
				folders = append(folders, item)
			} else {
				if isVideo(f.MimeType) {
					item.ViewURL = "/view/" + f.Id
				} else if isImage(f.MimeType) {
					item.ViewURL = "/file/" + f.Id
				}
				files = append(files, item)
			}
		}

		if err := tmpl.Execute(w, pageData{
			FolderName:  "Resultados para: " + q,
			CurrentID:   rootID,
			Folders:     folders,
			Files:       files,
			ViewType:    "folder",
			SearchQuery: q,
		}); err != nil {
			log.Printf("Erro ao renderizar template: %v", err)
		}
	}
}

func main() {
	fmt.Println("Iniciando OnlyFlix...")

	folderURL := "https://drive.google.com/drive/folders/1TN-7mwxXRMxLezW8BZ8TlaoSORQRng85?hl=pt-br"
	rootFolderID := extractFolderID(folderURL)
	if rootFolderID == "" {
		log.Fatal("Nao foi possivel extrair o ID da pasta da URL")
	}

	fmt.Println("Lendo credenciais...")
	b, err := os.ReadFile("credential.json")
	if err != nil {
		log.Fatalf("Erro ao ler credential.json: %v", err)
	}

	var creds webCredentials
	if err := json.Unmarshal(b, &creds); err != nil {
		log.Fatalf("Erro ao fazer parse do credential.json: %v", err)
	}

	config := &oauth2.Config{
		ClientID:     creds.Web.ClientID,
		ClientSecret: creds.Web.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  creds.Web.AuthURI,
			TokenURL: creds.Web.TokenURI,
		},
		Scopes: []string{drive.DriveReadonlyScope},
	}

	os.MkdirAll("downloads", 0755)
	loadHistory()

	fmt.Println("Preparando autenticacao...")
	client := getClient(config)

	go startDownloadManager(client, 3)

	fmt.Println("Criando servico do Google Drive...")
	srv, err := drive.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Erro ao criar servico do Drive: %v", err)
	}

	if b, err := os.ReadFile("catalog.json"); err == nil {
		var cat SyncCatalog
		if err := json.Unmarshal(b, &cat); err == nil {
			catalogCache = &cat
			fmt.Println("Catalogo local carregado da ultima sessao.")
		}
	}

	go func() {
		syncDrive(srv, rootFolderID)
		ticker := time.NewTicker(6 * time.Hour)
		for range ticker.C {
			syncDrive(srv, rootFolderID)
		}
	}()

	fmt.Println("Carregando template...")
	tmpl, err := template.ParseFiles("templates/templ.html")
	if err != nil {
		log.Fatalf("Erro ao parsear template: %v", err)
	}

	loadEnv()
	authUser := os.Getenv("AUTH_USER")
	authPass := os.Getenv("AUTH_PASS")
	publicURL := os.Getenv("PUBLIC_URL")

	if authUser != "" {
		fmt.Printf("Protecao ativada com usuario: %s\n", authUser)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", secure(handleFolder(srv, rootFolderID, tmpl), authUser, authPass))
	mux.HandleFunc("GET /folder/{id}", secure(handleFolder(srv, rootFolderID, tmpl), authUser, authPass))
	mux.HandleFunc("GET /view/{id}", secure(handleView(srv, tmpl), authUser, authPass))
	mux.HandleFunc("GET /file/{id}", secure(handleFile(srv, client), authUser, authPass))
	mux.HandleFunc("GET /search", secure(handleSearch(srv, rootFolderID, tmpl), authUser, authPass))
	mux.HandleFunc("GET /playlist.m3u", secure(handleM3U(publicURL, authUser, authPass), authUser, authPass))
	mux.HandleFunc("GET /api/sync", secure(handleForceSync(srv, rootFolderID), authUser, authPass))
	
	mux.HandleFunc("GET /player_api.php", handleXtream(publicURL, authUser, authPass))
	mux.HandleFunc("GET /movie/{user}/{pass}/{file}", handleXtreamFile(srv, client, authUser, authPass))

	port, err := findPort(8080)
	if err != nil {
		log.Fatalf("Erro ao achar porta: %v", err)
	}

	fmt.Println("Iniciando servidor...")
	server := &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", port), Handler: mux}
	go func() {
		fmt.Printf("Servidor rodando em http://0.0.0.0:%d\n", port)
		fmt.Printf("Acesse de outros dispositivos via http://SEU_IP:%d\n", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	openURL(fmt.Sprintf("http://localhost:%d", port))

	fmt.Println("\nPressione Ctrl+C para parar.")
	select {}
}
