package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

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
	Size    int64     `json:"size"`
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

var (
	catalogCache *SyncCatalog
	cacheMutex   sync.RWMutex
	localRoot    string
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
			key := parts[0]
			val := parts[1]
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
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

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/1024/1024)
}

func tagForExt(name string) (class, label string) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".m4v", ".ts", ".mpeg", ".mpg", ".3gp":
		return "video", "Vídeo"
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg":
		return "image", "Imagem"
	case ".pdf":
		return "pdf", "PDF"
	default:
		return "", "Arquivo"
	}
}

func isVideoExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".m4v", ".ts", ".mpeg", ".mpg", ".3gp":
		return true
	}
	return false
}

func isImageExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg":
		return true
	}
	return false
}

func mimeForFile(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".ts":
		return "video/mp2t"
	case ".m4v":
		return "video/x-m4v"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	default:
		return "video/mp4"
	}
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

func urlPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func getPathValue(r *http.Request, name string) string {
	v := r.PathValue(name)
	if decoded, err := url.PathUnescape(v); err == nil {
		return decoded
	}
	return v
}

func resolvePath(root, id string) (string, error) {
	clean := filepath.Clean(filepath.Join(root, filepath.FromSlash(id)))
	rootClean := filepath.Clean(root)
	if !strings.HasPrefix(clean, rootClean+string(os.PathSeparator)) && clean != rootClean {
		return "", fmt.Errorf("acesso negado: caminho fora do diretório permitido")
	}
	if _, err := os.Stat(clean); err != nil {
		return "", fmt.Errorf("arquivo não encontrado: %v", err)
	}
	return clean, nil
}

func buildBreadcrumb(folderID string) []breadcrumbItem {
	if folderID == "" {
		return nil
	}
	parts := strings.Split(folderID, "/")
	items := make([]breadcrumbItem, 0, len(parts))
	for i, part := range parts {
		id := strings.Join(parts[:i+1], "/")
		items = append(items, breadcrumbItem{Name: cleanName(part, false), Id: id})
	}
	return items
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func scanLocalFolder(root string) *SyncCatalog {
	log.Println("[SCAN] Iniciando scan da pasta local...")

	cat := &SyncCatalog{
		LastSync: time.Now(),
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		log.Printf("[SCAN] Erro ao ler diretório raiz: %v", err)
		return cat
	}

	for _, entry := range entries {
		relPath := entry.Name()

		if entry.IsDir() {
			folderID := relPath
			folder := SyncFolder{
				Id:   folderID,
				Name: cleanName(entry.Name(), false),
			}

			subEntries, err := os.ReadDir(filepath.Join(root, relPath))
			if err != nil {
				log.Printf("[SCAN] Erro ao ler pasta %s: %v", relPath, err)
				continue
			}

			for _, sub := range subEntries {
				if sub.IsDir() {
					continue
				}
				if isVideoExt(sub.Name()) {
					absPath := filepath.Join(root, relPath, sub.Name())
					info, err := os.Stat(absPath)
					if err != nil {
						continue
					}
					folder.Videos = append(folder.Videos, SyncVideo{
						Id:      filepath.ToSlash(filepath.Join(folderID, sub.Name())),
						Name:    cleanName(sub.Name(), true),
						Size:    info.Size(),
						Created: info.ModTime(),
					})
				}
			}

			sort.Slice(folder.Videos, func(i, j int) bool {
				return folder.Videos[i].Created.Before(folder.Videos[j].Created)
			})
			smartRename(folder.Videos, folder.Name)

			cat.Folders = append(cat.Folders, folder)
		} else if isVideoExt(entry.Name()) {
			absPath := filepath.Join(root, relPath)
			info, err := os.Stat(absPath)
			if err != nil {
				continue
			}
			cat.RootVideos = append(cat.RootVideos, SyncVideo{
				Id:      relPath,
				Name:    cleanName(entry.Name(), true),
				Size:    info.Size(),
				Created: info.ModTime(),
			})
		}
	}

	sort.Slice(cat.RootVideos, func(i, j int) bool {
		return cat.RootVideos[i].Created.Before(cat.RootVideos[j].Created)
	})
	smartRename(cat.RootVideos, "Geral")

	cacheMutex.Lock()
	catalogCache = cat
	cacheMutex.Unlock()

	b, _ := json.MarshalIndent(cat, "", "  ")
	os.WriteFile("catalog.json", b, 0644)

	log.Printf("[SCAN] Scan finalizado: %d pastas, %d vídeos na raiz", len(cat.Folders), len(cat.RootVideos))
	enqueueNewCatalogVideos()
	return cat
}

func handleM3U(publicURL, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("username")
		if u == "" {
			u = r.URL.Query().Get("user")
		}
		p := r.URL.Query().Get("password")
		if p == "" {
			p = r.URL.Query().Get("pass")
		}

		isAdmin := (authUser != "" && authPass != "" && u == authUser && p == authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = authenticateUser(u, p)
		}

		if !isAdmin && !isValidUser {
			// Fallback to check basic auth
			au, ap, ok := r.BasicAuth()
			if ok {
				isAdmin = (authUser != "" && authPass != "" && au == authUser && ap == authPass)
				if !isAdmin {
					isValidUser = authenticateUser(au, ap)
				}
			}
		}

		if !isAdmin && !isValidUser {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		cacheMutex.RLock()
		cat := catalogCache
		cacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catálogo ainda não sincronizado. Tente novamente em alguns segundos.", http.StatusServiceUnavailable)
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

		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Disposition", "attachment; filename=\"onlyflix.m3u\"")

		fmt.Fprintln(w, "#EXTM3U")

		var streamURL func(id string) string
		if isAdmin {
			authQuery := ""
			if authUser != "" && authPass != "" {
				authQuery = fmt.Sprintf("?user=%s&pass=%s", authUser, authPass)
			}
			streamURL = func(id string) string {
				if isTranscodeCompleted(id) {
					return fmt.Sprintf("%s/hls/admin/%s/index.m3u8%s", host, hashString(id), authQuery)
				}
				return fmt.Sprintf("%s/file/%s%s", host, urlPath(id), authQuery)
			}
		} else {
			streamURL = func(id string) string {
				if isTranscodeCompleted(id) {
					return fmt.Sprintf("%s/hls/%s/%s/%s/index.m3u8", host, u, p, hashString(id))
				}
				return fmt.Sprintf("%s/movie/%s/%s/%s", host, u, p, urlPath(id))
			}
		}

		for _, v := range cat.RootVideos {
			fmt.Fprintf(w, "#EXTINF:-1 group-title=\"Geral\", %s\n", v.Name)
			fmt.Fprintln(w, streamURL(v.Id))
		}

		for _, f := range cat.Folders {
			for _, v := range f.Videos {
				fmt.Fprintf(w, "#EXTINF:-1 group-title=\"%s\", %s\n", f.Name, v.Name)
				fmt.Fprintln(w, streamURL(v.Id))
			}
		}
	}
}

func handleXtream(publicURL, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("username")
		p := r.URL.Query().Get("password")
		
		isAdmin := (authUser != "" && authPass != "" && u == authUser && p == authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = authenticateUser(u, p)
		}

		if !isAdmin && !isValidUser {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"user_info":{"auth":0}}`))
			return
		}

		action := r.URL.Query().Get("action")

		cacheMutex.RLock()
		cat := catalogCache
		cacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catálogo indisponível", 503)
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

func streamLocalFile(w http.ResponseWriter, r *http.Request, fileID string) {
	absPath, err := resolvePath(localRoot, fileID)
	if err != nil {
		log.Printf("[STREAM] Erro ao resolver path %s: %v", fileID, err)
		http.Error(w, "Arquivo não encontrado", http.StatusNotFound)
		return
	}

	log.Printf("[STREAM] Servindo %s", fileID)
	http.ServeFile(w, r, absPath)
}

func handleFolder(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		folderID := getPathValue(r, "id")

		cacheMutex.RLock()
		cat := catalogCache
		cacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catálogo não disponível", http.StatusServiceUnavailable)
			return
		}

		var breadcrumb []breadcrumbItem
		var folderName string
		var folders, files []fileItem

		if folderID == "" {
			folderName = "Início"

			for _, f := range cat.Folders {
				folders = append(folders, fileItem{
					Name:     f.Name,
					Id:       f.Id,
					IsFolder: true,
					ViewURL:  "/folder/" + urlPath(f.Id),
					TagClass: "folder",
					TagLabel: "Pasta",
				})
			}

			for _, v := range cat.RootVideos {
				files = append(files, fileItem{
					Name:     v.Name,
					Id:       v.Id,
					ViewURL:  "/view/" + urlPath(v.Id),
					TagClass: "video",
					TagLabel: "Vídeo",
					Size:     formatSize(v.Size),
				})
			}
		} else {
			var found *SyncFolder
			for i := range cat.Folders {
				if cat.Folders[i].Id == folderID {
					found = &cat.Folders[i]
					break
				}
			}

			if found == nil {
				http.Error(w, "Pasta não encontrada", http.StatusNotFound)
				return
			}

			folderName = found.Name
			breadcrumb = buildBreadcrumb(folderID)

			for _, v := range found.Videos {
				files = append(files, fileItem{
					Name:     v.Name,
					Id:       v.Id,
					ViewURL:  "/view/" + urlPath(v.Id),
					TagClass: "video",
					TagLabel: "Vídeo",
					Size:     formatSize(v.Size),
				})
			}
		}

		if err := tmpl.Execute(w, pageData{
			Breadcrumb: breadcrumb,
			FolderName: folderName,
			CurrentID:  folderID,
			Folders:    folders,
			Files:      files,
			ViewType:   "folder",
		}); err != nil {
			log.Printf("Erro ao renderizar template: %v", err)
		}
	}
}

func handleView(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := getPathValue(r, "id")

		cacheMutex.RLock()
		cat := catalogCache
		cacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catálogo não disponível", http.StatusServiceUnavailable)
			return
		}

		var found *SyncVideo
		var parentID string

		for i := range cat.RootVideos {
			if cat.RootVideos[i].Id == fileID {
				found = &cat.RootVideos[i]
				break
			}
		}

		if found == nil {
			for _, f := range cat.Folders {
				for i := range f.Videos {
					if f.Videos[i].Id == fileID {
						found = &f.Videos[i]
						parentID = f.Id
						break
					}
				}
				if found != nil {
					break
				}
			}
		}

		if found == nil {
			http.Error(w, "Arquivo não encontrado", http.StatusNotFound)
			return
		}

		mimeType := mimeForFile(fileID)
		class, label := tagForExt(fileID)

		item := fileItem{
			Name:     found.Name,
			Id:       found.Id,
			MimeType: mimeType,
			Size:     formatSize(found.Size),
			TagClass: class,
			TagLabel: label,
		}

		viewURL := "/file/" + urlPath(found.Id)
		videoMime := mimeType
		if isTranscodeCompleted(found.Id) {
			viewURL = "/hls/admin/" + hashString(found.Id) + "/index.m3u8"
			videoMime = "application/x-mpegURL"
		}

		data := pageData{
			Breadcrumb: buildBreadcrumb(parentID),
			FolderName: found.Name,
			CurrentID:  parentID,
			ViewType:   "video",
			ViewFile:   &item,
			ViewURL:    viewURL,
			VideoMime:  videoMime,
		}

		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("Erro ao renderizar template: %v", err)
		}
	}
}

func handleFile() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := getPathValue(r, "id")
		log.Printf("Servindo arquivo: %s", fileID)
		streamLocalFile(w, r, fileID)
	}
}

func handleXtreamFile(authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.PathValue("user")
		p := r.PathValue("pass")
		
		isAdmin := (authUser != "" && authPass != "" && u == authUser && p == authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = authenticateUser(u, p)
		}

		if !isAdmin && !isValidUser {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		fileID := getPathValue(r, "file")

		if isValidUser {
			trackStreamStart(u, fileID)
			defer trackStreamEnd(u, fileID)
		}

		log.Printf("Servindo Xtream arquivo: %s", fileID)
		streamLocalFile(w, r, fileID)
	}
}

func sanitizeQuery(q string) string {
	q = strings.ReplaceAll(q, "'", "")
	q = strings.ReplaceAll(q, "\\", "")
	q = strings.ReplaceAll(q, "\"", "")
	return strings.TrimSpace(q)
}

func handleSearch(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := sanitizeQuery(r.URL.Query().Get("q"))
		if q == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		log.Printf("Buscando: %s", q)
		qLower := strings.ToLower(q)

		cacheMutex.RLock()
		cat := catalogCache
		cacheMutex.RUnlock()

		var folders, files []fileItem

		for _, f := range cat.Folders {
			if strings.Contains(strings.ToLower(f.Name), qLower) {
				folders = append(folders, fileItem{
					Name:     f.Name,
					Id:       f.Id,
					IsFolder: true,
					ViewURL:  "/folder/" + urlPath(f.Id),
					TagClass: "folder",
					TagLabel: "Pasta",
				})
			}
		}

		for _, v := range cat.RootVideos {
			if strings.Contains(strings.ToLower(v.Name), qLower) {
				files = append(files, fileItem{
					Name:     v.Name,
					Id:       v.Id,
					ViewURL:  "/view/" + urlPath(v.Id),
					TagClass: "video",
					TagLabel: "Vídeo",
					Size:     formatSize(v.Size),
				})
			}
		}

		for _, f := range cat.Folders {
			for _, v := range f.Videos {
				if strings.Contains(strings.ToLower(v.Name), qLower) {
					files = append(files, fileItem{
						Name:     v.Name,
						Id:       v.Id,
						ViewURL:  "/view/" + urlPath(v.Id),
						TagClass: "video",
						TagLabel: "Vídeo",
						Size:     formatSize(v.Size),
					})
				}
			}
		}

		if err := tmpl.Execute(w, pageData{
			FolderName:  "Resultados para: " + q,
			CurrentID:   "",
			Folders:     folders,
			Files:       files,
			ViewType:    "folder",
			SearchQuery: q,
		}); err != nil {
			log.Printf("Erro ao renderizar template: %v", err)
		}
	}
}

func handleAdmin(tmpl *template.Template, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := struct {
			Users     []UserStatusResponse
			PublicURL string
		}{
			Users:     getUsersStatusList(),
			PublicURL: publicURL,
		}
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("Erro ao renderizar template do admin: %v", err)
		}
	}
}

func handleAdminCreateUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, err := createUser(req.Username, req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)
	}
}

func handleAdminToggleUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		active, err := toggleUser(req.Username)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"username": req.Username,
			"active":   active,
		})
	}
}

func handleAdminResetPassword() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		newPass, err := resetUserPassword(req.Username)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"username": req.Username,
			"password": newPass,
		})
	}
}

func handleAdminDeleteUser() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := deleteUser(req.Username); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
		})
	}
}

func handleAdminUsersStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getUsersStatusList())
	}
}

func handleHLSStream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.PathValue("user")
		p := r.PathValue("pass")

		if !authenticateUser(u, p) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		folder := r.PathValue("folder")
		file := r.PathValue("file")

		cleanFolder := filepath.Clean(folder)
		cleanFile := filepath.Clean(file)
		if strings.Contains(cleanFolder, "..") || strings.Contains(cleanFile, "..") {
			http.Error(w, "Access Denied", http.StatusForbidden)
			return
		}

		destFile := filepath.Join(transcodeDir, cleanFolder, cleanFile)
		if _, err := os.Stat(destFile); err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		if cleanFile == "index.m3u8" || strings.HasSuffix(cleanFile, ".ts") {
			fileID := getFileIDFromHash(cleanFolder)
			if fileID != "" {
				trackHLSRequest(u, fileID)
			}
		}

		http.ServeFile(w, r, destFile)
	}
}

func handleHLSAdminStream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		folder := r.PathValue("folder")
		file := r.PathValue("file")

		cleanFolder := filepath.Clean(folder)
		cleanFile := filepath.Clean(file)
		if strings.Contains(cleanFolder, "..") || strings.Contains(cleanFile, "..") {
			http.Error(w, "Access Denied", http.StatusForbidden)
			return
		}

		destFile := filepath.Join(transcodeDir, cleanFolder, cleanFile)
		if _, err := os.Stat(destFile); err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		http.ServeFile(w, r, destFile)
	}
}

func handleAdminTranscodeStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getTranscodeStatusList())
	}
}

func handleAdminTranscodeRetry() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			FileID string `json:"file_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := retryFailedJob(req.FileID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	}
}

func getFileIDFromHash(hash string) string {
	transcodeMutex.RLock()
	defer transcodeMutex.RUnlock()
	for id := range transcodeJobs {
		if hashString(id) == hash {
			return id
		}
	}
	return ""
}

type diskUsageResponse struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Percent   float64 `json:"percent"`
}

func handleAdminDiskUsage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(localRoot, &stat); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		total := stat.Blocks * uint64(stat.Bsize)
		avail := stat.Bavail * uint64(stat.Bsize)
		used := total - avail
		var pct float64
		if total > 0 {
			pct = float64(used) / float64(total) * 100.0
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(diskUsageResponse{
			Total:     total,
			Used:      used,
			Available: avail,
			Percent:   pct,
		})
	}
}

func handleAdminUpload() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 50<<30)

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()

		var uploaded int
		var errs []string

		for _, fileHeaders := range r.MultipartForm.File {
			for _, fh := range fileHeaders {
				relPath := filepath.ToSlash(fh.Filename)

				if !isVideoExt(relPath) {
					errs = append(errs, relPath+": formato não suportado")
					continue
				}

				dest := filepath.Clean(filepath.Join(localRoot, filepath.FromSlash(relPath)))
				rootClean := filepath.Clean(localRoot)
				if !strings.HasPrefix(dest, rootClean+string(os.PathSeparator)) && dest != rootClean {
					errs = append(errs, relPath+": acesso negado")
					continue
				}

				src, err := fh.Open()
				if err != nil {
					errs = append(errs, relPath+": "+err.Error())
					continue
				}

				if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
					src.Close()
					errs = append(errs, relPath+": "+err.Error())
					continue
				}

				dst, err := os.Create(dest)
				if err != nil {
					src.Close()
					errs = append(errs, relPath+": "+err.Error())
					continue
				}

				_, err = io.Copy(dst, src)
				src.Close()
				dst.Close()

				if err != nil {
					errs = append(errs, relPath+": "+err.Error())
					continue
				}

				uploaded++
			}
		}

		if uploaded > 0 {
			go scanLocalFolder(localRoot)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uploaded": uploaded,
			"errors":   errs,
		})
	}
}

func main() {
	fmt.Println("Iniciando OnlyFlix...")

	loadEnv()

	localRoot = os.Getenv("LOCAL_PATH")
	if localRoot == "" {
		log.Fatal("LOCAL_PATH não definida. Defina a variável de ambiente LOCAL_PATH apontando para a pasta com os vídeos.")
	}

	if info, err := os.Stat(localRoot); err != nil || !info.IsDir() {
		log.Fatalf("LOCAL_PATH '%s' não é um diretório válido: %v", localRoot, err)
	}

	fmt.Printf("Pasta local: %s\n", localRoot)

	authUser := os.Getenv("AUTH_USER")
	authPass := os.Getenv("AUTH_PASS")
	publicURL := os.Getenv("PUBLIC_URL")

	if authUser != "" {
		fmt.Printf("Proteção ativada com usuário: %s\n", authUser)
	}

	fmt.Println("Carregando banco de dados de usuários...")
	if err := loadUsers(); err != nil {
		log.Printf("Erro ao carregar usuários: %v", err)
	}

	fmt.Println("Inicializando transcodificador HLS...")
	initTranscoder()
	go startTranscoderWorker()

	fmt.Println("Carregando templates...")
	tmpl, err := template.ParseFiles("templates/templ.html")
	if err != nil {
		log.Fatalf("Erro ao parsear template: %v", err)
	}
	adminTmpl, err := template.ParseFiles("templates/admin.html")
	if err != nil {
		log.Fatalf("Erro ao parsear template do admin: %v", err)
	}

	fmt.Println("Escaneando pasta local...")
	scanLocalFolder(localRoot)

	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		for range ticker.C {
			scanLocalFolder(localRoot)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", secure(handleFolder(tmpl), authUser, authPass))
	mux.HandleFunc("GET /folder/{id...}", secure(handleFolder(tmpl), authUser, authPass))
	mux.HandleFunc("GET /view/{id...}", secure(handleView(tmpl), authUser, authPass))
	mux.HandleFunc("GET /file/{id...}", secure(handleFile(), authUser, authPass))
	mux.HandleFunc("GET /search", secure(handleSearch(tmpl), authUser, authPass))
	mux.HandleFunc("GET /playlist.m3u", secure(handleM3U(publicURL, authUser, authPass), authUser, authPass))

	// HLS streaming (dinâmico e seguro para clientes)
	mux.HandleFunc("GET /hls/{user}/{pass}/{folder}/{file}", handleHLSStream())

	// HLS streaming estático (para visualização do admin)
	mux.HandleFunc("GET /hls/admin/{folder}/{file}", secure(handleHLSAdminStream(), authUser, authPass))

	// Xtream Code endpoints (autenticam contra usuários criados dinamicamente)
	mux.HandleFunc("GET /player_api.php", handleXtream(publicURL, authUser, authPass))
	mux.HandleFunc("GET /movie/{user}/{pass}/{file...}", handleXtreamFile(authUser, authPass))

	// Admin Dashboard endpoints (restritos ao administrador geral)
	mux.HandleFunc("GET /admin", secure(handleAdmin(adminTmpl, publicURL), authUser, authPass))
	mux.HandleFunc("POST /admin/users", secure(handleAdminCreateUser(), authUser, authPass))
	mux.HandleFunc("POST /admin/users/toggle", secure(handleAdminToggleUser(), authUser, authPass))
	mux.HandleFunc("POST /admin/users/reset-password", secure(handleAdminResetPassword(), authUser, authPass))
	mux.HandleFunc("DELETE /admin/users", secure(handleAdminDeleteUser(), authUser, authPass))
	mux.HandleFunc("GET /admin/users/status", secure(handleAdminUsersStatus(), authUser, authPass))

	// Transcoder admin endpoints
	mux.HandleFunc("GET /admin/transcode/status", secure(handleAdminTranscodeStatus(), authUser, authPass))
	mux.HandleFunc("POST /admin/transcode/retry", secure(handleAdminTranscodeRetry(), authUser, authPass))

	// Admin upload and disk usage
	mux.HandleFunc("POST /admin/upload", secure(handleAdminUpload(), authUser, authPass))
	mux.HandleFunc("GET /admin/disk-usage", secure(handleAdminDiskUsage(), authUser, authPass))

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	fmt.Printf("Servidor rodando em http://0.0.0.0:%s\n", port)
	fmt.Printf("Acesse de outros dispositivos via http://SEU_IP:%s\n", port)
	if err := http.ListenAndServe("0.0.0.0:"+port, mux); err != nil {
		log.Fatal(err)
	}
}
