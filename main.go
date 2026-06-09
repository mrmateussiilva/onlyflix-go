package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

	adminSessions = make(map[string]time.Time)
	sessionMutex  sync.Mutex
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
		if hasValidAdminSession(r) {
			h(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if ok && constantTimeEqual(u, authUser) && constantTimeEqual(p, authPass) {
			h(w, r)
			return
		}

		if wantsLoginRedirect(r) {
			next := url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
			return
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

func constantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func wantsLoginRedirect(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") || strings.Contains(accept, "application/vnd.apple.mpegurl") {
		return false
	}
	return true
}

func hasValidAdminSession(r *http.Request) bool {
	cookie, err := r.Cookie("onlyflix_admin")
	if err != nil || cookie.Value == "" {
		return false
	}

	sessionMutex.Lock()
	defer sessionMutex.Unlock()

	expires, ok := adminSessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(adminSessions, cookie.Value)
		return false
	}
	return true
}

func createAdminSession(w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(24 * time.Hour)

	sessionMutex.Lock()
	adminSessions[token] = expires
	sessionMutex.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "onlyflix_admin",
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	return nil
}

func clearAdminSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("onlyflix_admin"); err == nil {
		sessionMutex.Lock()
		delete(adminSessions, cookie.Value)
		sessionMutex.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "onlyflix_admin",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
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

var noisePatternsPre = []string{
	`(?i)\b(1080p|720p|2160p|480p|4k)\b`,
	`(?i)\b(amzn|web[.-]dl|dsnp|nf|pmtp|hmax|hbo|max)\b`,
	`(?i)\b(webrip|bluray|hdrip|bdrip|hdtv|dvdrip)\b`,
	`(?i)\b(h\.?264|x264|h\.?265|x265|hevc|avc|av1)\b`,
	`(?i)\b(ddp5\.?1|dd5\.?1|dd2\.?0|aac|ac3|eac3|dts|mp3|opus)\b`,
	`(?i)\b(atmos|hdr10?|hdr|dolby|vision|sdr|hlg)\b`,
	`(?i)\b(dual|dublado|legendado|multi)\b`,
	`(?i)\b(1080|720|2160|480)p\b`,
	`(?i)\b(complete|proper|repack|internal|readnfo)\b`,
}

var trailGroupPre = regexp.MustCompile(`(?i)[\s.-]+-\s+(SiGLA|FLUX|PiA|C76|RMB|FGT|NTb|PD_Dinho|SMURF)\s*$`)
var trailGroupPost = regexp.MustCompile(`(?i)\s+(SiGLA|FLUX|PiA|C76|RMB|FGT|NTb|PD_Dinho|SMURF)\s*$`)

func stripNoise(name string) string {
	for _, pat := range noisePatternsPre {
		name = regexp.MustCompile(pat).ReplaceAllString(name, "")
	}
	name = trailGroupPre.ReplaceAllString(name, "")
	return name
}

func cleanName(name string, stripExt bool) string {
	if stripExt {
		if idx := strings.LastIndex(name, "."); idx > 0 {
			name = name[:idx]
		}
	}

	name = strings.ReplaceAll(name, "_", " ")

	name = stripNoise(name)

	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, ".", " ")

	name = stripNoise(name)

	name = trailGroupPost.ReplaceAllString(name, "")

	fields := strings.Fields(name)
	name = strings.Join(fields, " ")

	if len(name) > 80 {
		name = name[:80]
	}

	if stripExt {
		name = formatVideoName(name)
	}

	return name
}

func formatVideoName(name string) string {
	re := regexp.MustCompile(`(?i)^(.*?)\s*(S\d{2}E\d{2})\s*(.*)$`)
	m := re.FindStringSubmatch(name)
	if len(m) == 4 {
		ep := strings.ToUpper(m[2])
		title := strings.TrimSpace(m[3])
		if title != "" {
			return fmt.Sprintf("%s - %s", ep, title)
		}
		return ep
	}
	return name
}

func episodeSortKey(name string) int {
	re := regexp.MustCompile(`S(\d{2})E(\d{2})`)
	m := re.FindStringSubmatch(name)
	if len(m) == 3 {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		return s*10000 + e
	}
	return -1
}

func isGarbageName(name string) bool {
	if len(name) < 3 {
		return true
	}

	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "sample") && len(name) < 20 {
		return true
	}

	letterRatio := 0
	for _, c := range name {
		if unicode.IsLetter(c) {
			letterRatio++
		}
	}
	if len(name) > 0 && float64(letterRatio)/float64(len(name)) < 0.3 {
		return true
	}

	return false
}

func extractEpisode(name string) string {
	re := regexp.MustCompile(`(?i)(S\d{2}(E\d{2})*)`)
	m := re.FindStringSubmatch(name)
	if len(m) > 1 {
		return strings.ToUpper(m[1])
	}
	return ""
}

func smartRename(videos []SyncVideo, prefix string) {
	episodeCount := 1
	prefixText := prefix
	if prefix == "" || prefix == "Geral" {
		prefixText = ""
	}

	for i, v := range videos {
		if isGarbageName(v.Name) {
			ep := extractEpisode(v.Id)
			if ep != "" {
				if prefixText != "" {
					videos[i].Name = fmt.Sprintf("%s %s", prefixText, ep)
				} else {
					videos[i].Name = ep
				}
			} else {
				if prefixText != "" {
					videos[i].Name = fmt.Sprintf("%s - Episódio %02d", prefixText, episodeCount)
				} else {
					videos[i].Name = fmt.Sprintf("Episódio %02d", episodeCount)
				}
			}
			episodeCount++
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

	rootClean := filepath.Clean(root)
	if _, err := os.Stat(rootClean); err != nil {
		log.Printf("[SCAN] Erro ao acessar diretório raiz: %v", err)
		return cat
	}

	foldersByID := make(map[string]*SyncFolder)

	err := filepath.WalkDir(rootClean, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			log.Printf("[SCAN] Erro ao acessar %s: %v", path, err)
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !isVideoExt(entry.Name()) {
			return nil
		}

		relPath, err := filepath.Rel(rootClean, path)
		if err != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		info, err := entry.Info()
		if err != nil {
			return nil
		}

		dir := filepath.ToSlash(filepath.Dir(relPath))
		video := SyncVideo{
			Id:      relPath,
			Name:    cleanName(entry.Name(), true),
			Size:    info.Size(),
			Created: info.ModTime(),
		}

		if dir == "." {
			cat.RootVideos = append(cat.RootVideos, video)
			return nil
		}

		folder := foldersByID[dir]
		if folder == nil {
			folder = &SyncFolder{
				Id:   dir,
				Name: cleanName(filepath.Base(dir), false),
			}
			foldersByID[dir] = folder
		}
		folder.Videos = append(folder.Videos, video)
		return nil
	})
	if err != nil {
		log.Printf("[SCAN] Erro ao escanear pasta local: %v", err)
	}

	sortVideoByEpisode := func(videos []SyncVideo) {
		sort.SliceStable(videos, func(i, j int) bool {
			ei := episodeSortKey(videos[i].Name)
			ej := episodeSortKey(videos[j].Name)
			if ei != -1 && ej != -1 {
				return ei < ej
			}
			return videos[i].Created.Before(videos[j].Created)
		})
	}

	sortVideoByEpisode(cat.RootVideos)
	smartRename(cat.RootVideos, "Geral")

	for _, folder := range foldersByID {
		if len(folder.Videos) == 0 {
			continue
		}
		sortVideoByEpisode(folder.Videos)
		smartRename(folder.Videos, folder.Name)
		cat.Folders = append(cat.Folders, *folder)
	}
	sort.Slice(cat.Folders, func(i, j int) bool {
		return cat.Folders[i].Name < cat.Folders[j].Name
	})

	cleanFolders := make([]SyncFolder, 0, len(cat.Folders))
	for _, f := range cat.Folders {
		if len(f.Videos) > 0 {
			cleanFolders = append(cleanFolders, f)
		}
	}
	cat.Folders = cleanFolders

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
		u, p, ok := r.BasicAuth()
		isAdmin := ok && constantTimeEqual(u, authUser) && constantTimeEqual(p, authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = authenticateUser(u, p)
		}

		if !isAdmin && !isValidUser {
			u = r.URL.Query().Get("username")
			if u == "" {
				u = r.URL.Query().Get("user")
			}
			p = r.URL.Query().Get("password")
			if p == "" {
				p = r.URL.Query().Get("pass")
			}
			isAdmin = (authUser != "" && authPass != "" && u == authUser && p == authPass)
			if !isAdmin {
				isValidUser = authenticateUser(u, p)
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
		fmt.Fprintln(w, "#PLAYLIST: OnlyFlix")

		var streamURL func(id string) string
		if isAdmin {
			streamURL = func(id string) string {
				if isTranscodeCompleted(id) {
					return fmt.Sprintf("%s/hls/admin/%s/index.m3u8", host, hashString(id))
				}
				return fmt.Sprintf("%s/file/%s", host, urlPath(id))
			}
		} else {
			streamURL = func(id string) string {
				if isTranscodeCompleted(id) {
					return fmt.Sprintf("%s/hls/%s/%s/%s/index.m3u8", host, u, p, hashString(id))
				}
				return fmt.Sprintf("%s/movie/%s/%s/%s", host, u, p, urlPath(id))
			}
		}

		writeM3UEntry := func(v SyncVideo, group string) {
			ep := extractEpisode(v.Id)
			tvgID := fmt.Sprintf("OF-%s", hashString(v.Id))
			tvgName := v.Name
			if ep != "" {
				tvgName = fmt.Sprintf("%s %s", group, ep)
			}
			fmt.Fprintf(w, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\" group-title=\"%s\",%s\n",
				tvgID, tvgName, group, v.Name)
			fmt.Fprintln(w, streamURL(v.Id))
		}

		for _, v := range cat.RootVideos {
			if !shouldPublishVideo(v) {
				continue
			}
			writeM3UEntry(v, "Geral")
		}

		for _, f := range cat.Folders {
			for _, v := range f.Videos {
				if !shouldPublishVideo(v) {
					continue
				}
				writeM3UEntry(v, f.Name)
			}
		}
	}
}

func handleXtream(publicURL, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		isAdmin := ok && constantTimeEqual(u, authUser) && constantTimeEqual(p, authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = authenticateUser(u, p)
		}

		if !isAdmin && !isValidUser {
			// Fallback: query params (protocolo Xtream)
			u = r.URL.Query().Get("username")
			p = r.URL.Query().Get("password")
			isAdmin = (authUser != "" && authPass != "" && u == authUser && p == authPass)
			if !isAdmin {
				isValidUser = authenticateUser(u, p)
			}
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

		if action == "get_vod_info" {
			vodID := canonicalCatalogFileID(r.URL.Query().Get("vod_id"))
			video, categoryID, categoryName, ok := findCatalogVideo(cat, vodID)
			if !ok || !shouldPublishVideo(video) {
				w.Write([]byte(`{"info":{},"movie_data":{}}`))
				return
			}
			b, _ := json.Marshal(buildVodInfoResponse(host, u, p, video, categoryID, categoryName, isAdmin))
			w.Write(b)
			return
		}

		if action == "get_vod_streams" {
			catID := r.URL.Query().Get("category_id")
			type EpisodeInfo struct {
				Season  int `json:"season"`
				Episode int `json:"episode"`
			}
			type XVod struct {
				Num                int          `json:"num"`
				Name               string       `json:"name"`
				StreamType         string       `json:"stream_type"`
				StreamId           string       `json:"stream_id"`
				StreamIcon         string       `json:"stream_icon"`
				Rating             int          `json:"rating"`
				Rating5based       int          `json:"rating_5based"`
				Added              string       `json:"added"`
				CategoryId         string       `json:"category_id"`
				ContainerExtension string       `json:"container_extension"`
				CustomSid          string       `json:"custom_sid"`
				DirectSource       string       `json:"direct_source"`
				EpisodeInfo        *EpisodeInfo `json:"episode_info,omitempty"`
			}
			var vods []XVod

			addVod := func(v SyncVideo, cID string, order int) {
				if !shouldPublishVideo(v) {
					return
				}
				containerExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(v.Id)), ".")
				if containerExt == "" {
					containerExt = "mp4"
				}
				directSource := xtreamStreamURL(host, u, p, v.Id, isAdmin, true)
				if isTranscodeCompleted(v.Id) {
					containerExt = "m3u8"
				}

				var epInfo *EpisodeInfo
				ep := extractEpisode(v.Name)
				if ep == "" {
					ep = extractEpisode(v.Id)
				}
				if ep != "" {
					re := regexp.MustCompile(`S(\d{2})E(\d{2})`)
					m := re.FindStringSubmatch(ep)
					if len(m) == 3 {
						s, _ := strconv.Atoi(m[1])
						e, _ := strconv.Atoi(m[2])
						epInfo = &EpisodeInfo{Season: s, Episode: e}
					}
				}

				vods = append(vods, XVod{
					Num:                order,
					Name:               v.Name,
					StreamType:         "movie",
					StreamId:           v.Id,
					CategoryId:         cID,
					ContainerExtension: containerExt,
					Added:              fmt.Sprintf("%d", v.Created.Unix()),
					DirectSource:       directSource,
					EpisodeInfo:        epInfo,
				})
			}

			order := 1
			if catID == "root" || catID == "" {
				for _, v := range cat.RootVideos {
					addVod(v, "root", order)
					order++
				}
			}
			if catID == "" {
				for _, f := range cat.Folders {
					for _, v := range f.Videos {
						addVod(v, f.Id, order)
						order++
					}
				}
			} else {
				for _, f := range cat.Folders {
					if f.Id == catID {
						for _, v := range f.Videos {
							addVod(v, f.Id, order)
							order++
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

func shouldPublishVideo(v SyncVideo) bool {
	if !boolEnv("HIDE_UNREADY") && !boolEnv("XTREAM_HIDE_UNREADY") {
		return true
	}
	return isTranscodeCompleted(v.Id)
}

func boolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on", "sim":
		return true
	default:
		return false
	}
}

func xtreamStreamURL(host, username, password, fileID string, isAdmin bool, compatibleMovieRoute bool) string {
	if isTranscodeCompleted(fileID) {
		if compatibleMovieRoute {
			return fmt.Sprintf(
				"%s/movie/%s/%s/%s.m3u8",
				host,
				url.PathEscape(username),
				url.PathEscape(password),
				urlPath(fileID),
			)
		}
		if isAdmin {
			return fmt.Sprintf(
				"%s/hls/admin/%s/index.m3u8",
				host,
				hashString(fileID),
			)
		}
		return fmt.Sprintf(
			"%s/hls/%s/%s/%s/index.m3u8",
			host,
			url.PathEscape(username),
			url.PathEscape(password),
			hashString(fileID),
		)
	}
	if isAdmin {
		return fmt.Sprintf(
			"%s/file/%s",
			host,
			urlPath(fileID),
		)
	}
	return fmt.Sprintf(
		"%s/movie/%s/%s/%s",
		host,
		url.PathEscape(username),
		url.PathEscape(password),
		urlPath(fileID),
	)
}

func findCatalogVideo(cat *SyncCatalog, fileID string) (SyncVideo, string, string, bool) {
	if cat == nil {
		return SyncVideo{}, "", "", false
	}
	fileID = canonicalCatalogFileID(fileID)
	for _, v := range cat.RootVideos {
		if v.Id == fileID {
			return v, "root", "Geral", true
		}
	}
	for _, f := range cat.Folders {
		for _, v := range f.Videos {
			if v.Id == fileID {
				return v, f.Id, f.Name, true
			}
		}
	}
	return SyncVideo{}, "", "", false
}

func getVideoDuration(fileID string) float64 {
	transcodeMutex.RLock()
	defer transcodeMutex.RUnlock()
	if job, ok := transcodeJobs[fileID]; ok && job.Duration > 0 {
		return job.Duration
	}
	return 0
}

func buildVodInfoResponse(host, username, password string, v SyncVideo, categoryID, categoryName string, isAdmin bool) map[string]interface{} {
	containerExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(v.Id)), ".")
	if containerExt == "" {
		containerExt = "mp4"
	}
	if isTranscodeCompleted(v.Id) {
		containerExt = "m3u8"
	}

	duration := getVideoDuration(v.Id)
	var durationStr string
	if duration > 0 {
		secs := int(duration)
		h := secs / 3600
		m := (secs % 3600) / 60
		s := secs % 60
		if h > 0 {
			durationStr = fmt.Sprintf("%dh %dm %ds", h, m, s)
		} else {
			durationStr = fmt.Sprintf("%dm %ds", m, s)
		}
	}

	return map[string]interface{}{
		"info": map[string]interface{}{
			"name":          v.Name,
			"movie_image":   "",
			"plot":          "",
			"genre":         categoryName,
			"releasedate":   "",
			"rating":        "0",
			"duration_secs": int(duration),
			"duration":      durationStr,
			"video":         map[string]interface{}{},
			"audio":         map[string]interface{}{},
			"bitrate":       0,
		},
		"movie_data": map[string]interface{}{
			"stream_id":           v.Id,
			"name":                v.Name,
			"added":               fmt.Sprintf("%d", v.Created.Unix()),
			"category_id":         categoryID,
			"container_extension": containerExt,
			"custom_sid":          "",
			"direct_source":       xtreamStreamURL(host, username, password, v.Id, isAdmin, true),
		},
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

		fileID := canonicalCatalogFileID(getPathValue(r, "file"))

		if (boolEnv("HIDE_UNREADY") || boolEnv("XTREAM_HIDE_UNREADY")) && !isTranscodeCompleted(fileID) {
			http.Error(w, "Mídia ainda em processamento", http.StatusNotFound)
			return
		}

		if isValidUser {
			trackStreamStart(u, fileID)
			defer trackStreamEnd(u, fileID)
		}

		if isTranscodeCompleted(fileID) {
			log.Printf("Redirecionando Xtream para HLS: %s", fileID)
			if isAdmin {
				http.Redirect(w, r, fmt.Sprintf(
					"/hls/admin/%s/index.m3u8",
					hashString(fileID),
				), http.StatusFound)
				return
			}
			http.Redirect(w, r, fmt.Sprintf(
				"/hls/%s/%s/%s/index.m3u8",
				url.PathEscape(u),
				url.PathEscape(p),
				hashString(fileID),
			), http.StatusFound)
			return
		}

		log.Printf("Servindo Xtream arquivo: %s", fileID)
		streamLocalFile(w, r, fileID)
	}
}

func canonicalCatalogFileID(fileID string) string {
	cacheMutex.RLock()
	cat := catalogCache
	cacheMutex.RUnlock()

	if cat == nil {
		return fileID
	}

	candidates := []string{fileID}
	for _, ext := range []string{".m3u8", ".ts"} {
		if strings.HasSuffix(strings.ToLower(fileID), ext) {
			candidates = append(candidates, fileID[:len(fileID)-len(ext)])
		}
	}

	for _, candidate := range candidates {
		for _, v := range cat.RootVideos {
			if v.Id == candidate {
				return v.Id
			}
		}
		for _, f := range cat.Folders {
			for _, v := range f.Videos {
				if v.Id == candidate {
					return v.Id
				}
			}
		}
	}

	return fileID
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

func handleAdminScan() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}

		cat := scanLocalFolder(localRoot)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "success",
			"folders":     len(cat.Folders),
			"root_videos": len(cat.RootVideos),
			"last_sync":   cat.LastSync,
		})
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
		category := sanitizeUploadPath(r.FormValue("category"))

		for _, fileHeaders := range r.MultipartForm.File {
			for _, fh := range fileHeaders {
				relPath := sanitizeUploadPath(fh.Filename)
				if relPath == "" {
					errs = append(errs, fh.Filename+": nome inválido")
					continue
				}
				if category != "" {
					relPath = filepath.ToSlash(filepath.Join(category, relPath))
				}

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

func sanitizeUploadPath(path string) string {
	path = strings.TrimSpace(filepath.ToSlash(path))
	if path == "" {
		return ""
	}

	var parts []string
	for _, part := range strings.Split(path, "/") {
		part = strings.TrimSpace(part)
		if part == "" || part == "." || part == ".." {
			continue
		}
		part = strings.ReplaceAll(part, "\\", "")
		part = strings.Trim(part, "/")
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "/")
}

func handleLogin(authUser, authPass string) http.HandlerFunc {
	const loginHTML = `<!DOCTYPE html>
<html lang="pt-br">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>OnlyFlix - Login Admin</title>
	<style>
		:root { --bg:#09090b; --surface:#18181b; --border:#27272a; --text:#f4f4f5; --muted:#a1a1aa; --accent:#e11d48; --danger:#ef4444; }
		* { box-sizing: border-box; }
		body { margin:0; min-height:100vh; display:grid; place-items:center; background:var(--bg); color:var(--text); font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
		.login { width:min(420px, calc(100vw - 32px)); background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:28px; box-shadow:0 24px 60px rgba(0,0,0,.45); }
		h1 { margin:0 0 6px; color:var(--accent); font-size:28px; }
		p { margin:0 0 24px; color:var(--muted); }
		label { display:block; margin:16px 0 8px; color:var(--muted); font-size:14px; }
		input { width:100%; border:1px solid var(--border); border-radius:8px; background:#09090b; color:var(--text); padding:12px 14px; font-size:16px; outline:none; }
		input:focus { border-color:var(--accent); }
		button { width:100%; margin-top:22px; border:0; border-radius:8px; background:var(--accent); color:white; padding:12px 14px; font-size:15px; font-weight:700; cursor:pointer; }
		.error { margin-top:16px; color:var(--danger); font-size:14px; }
	</style>
</head>
<body>
	<form class="login" method="post" action="/login">
		<h1>OnlyFlix</h1>
		<p>Painel administrativo</p>
		<input type="hidden" name="next" value="{{.Next}}">
		<label for="username">Usuário</label>
		<input id="username" name="username" autocomplete="username" autofocus>
		<label for="password">Senha</label>
		<input id="password" name="password" type="password" autocomplete="current-password">
		<button type="submit">Entrar</button>
		{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
	</form>
</body>
</html>`

	tmpl := template.Must(template.New("login").Parse(loginHTML))
	return func(w http.ResponseWriter, r *http.Request) {
		if authUser == "" || authPass == "" {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}

		next := r.URL.Query().Get("next")
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			next = r.Form.Get("next")
			if next == "" {
				next = "/admin"
			}
			if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
				next = "/admin"
			}

			username := r.Form.Get("username")
			password := r.Form.Get("password")
			if constantTimeEqual(username, authUser) && constantTimeEqual(password, authPass) {
				if err := createAdminSession(w, r); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				http.Redirect(w, r, next, http.StatusSeeOther)
				return
			}

			w.WriteHeader(http.StatusUnauthorized)
			tmpl.Execute(w, map[string]string{
				"Next":  next,
				"Error": "Usuário ou senha inválidos.",
			})
			return
		}

		if next == "" {
			next = "/admin"
		}
		tmpl.Execute(w, map[string]string{"Next": next})
	}
}

func handleLogout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clearAdminSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
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

	if authPass == "123456" || len(authPass) < 6 {
		log.Println("[AVISO] A senha do admin é muito fraca! Altere para uma senha mais segura.")
	}
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
	mux.HandleFunc("GET /login", handleLogin(authUser, authPass))
	mux.HandleFunc("POST /login", handleLogin(authUser, authPass))
	mux.HandleFunc("GET /logout", handleLogout())
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
	mux.HandleFunc("POST /admin/scan", secure(handleAdminScan(), authUser, authPass))

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
