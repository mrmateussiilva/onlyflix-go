package handlers

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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"onlyflix/catalog"
	"onlyflix/media"
	"onlyflix/transcoder"
	"onlyflix/types"
	"onlyflix/users"
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

func buildBreadcrumb(folderID string) []breadcrumbItem {
	if folderID == "" {
		return nil
	}
	parts := strings.Split(folderID, "/")
	items := make([]breadcrumbItem, 0, len(parts))
	for i, part := range parts {
		id := strings.Join(parts[:i+1], "/")
		items = append(items, breadcrumbItem{Name: media.CleanName(part, false), Id: id})
	}
	return items
}

func boolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on", "sim":
		return true
	default:
		return false
	}
}

func shouldPublishVideo(v types.SyncVideo) bool {
	if !boolEnv("HIDE_UNREADY") && !boolEnv("XTREAM_HIDE_UNREADY") {
		return true
	}
	return transcoder.IsTranscodeCompleted(v.Id)
}

func streamLocalFile(w http.ResponseWriter, r *http.Request, fileID string) {
	absPath, err := catalog.ResolvePath(catalog.LocalRoot, fileID)
	if err != nil {
		log.Printf("[STREAM] Erro ao resolver path %s: %v", fileID, err)
		http.Error(w, "Arquivo não encontrado", http.StatusNotFound)
		return
	}

	log.Printf("[STREAM] Servindo %s", fileID)
	http.ServeFile(w, r, absPath)
}

func xtreamStreamURL(host, username, password, fileID string, isAdmin bool, compatibleMovieRoute bool) string {
	if transcoder.IsTranscodeCompleted(fileID) {
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
				transcoder.HashString(fileID),
			)
		}
		return fmt.Sprintf(
			"%s/hls/%s/%s/%s/index.m3u8",
			host,
			url.PathEscape(username),
			url.PathEscape(password),
			transcoder.HashString(fileID),
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

func buildVodInfoResponse(host, username, password string, v types.SyncVideo, categoryID, categoryName string, isAdmin bool) map[string]interface{} {
	containerExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(v.Id)), ".")
	if containerExt == "" {
		containerExt = "mp4"
	}
	if transcoder.IsTranscodeCompleted(v.Id) {
		containerExt = "m3u8"
	}

	duration := transcoder.GetVideoDuration(v.Id)
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

func HandleFolder(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		catalog.CacheMutex.RLock()
		cat := catalog.Cache
		catalog.CacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catálogo não disponível", http.StatusServiceUnavailable)
			return
		}

		folderID := getPathValue(r, "id")

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
					Size:     media.FormatSize(v.Size),
				})
			}
		} else {
			var found *types.SyncFolder
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
					Size:     media.FormatSize(v.Size),
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

func HandleView(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := getPathValue(r, "id")

		catalog.CacheMutex.RLock()
		cat := catalog.Cache
		catalog.CacheMutex.RUnlock()

		if cat == nil {
			http.Error(w, "Catálogo não disponível", http.StatusServiceUnavailable)
			return
		}

		var found *types.SyncVideo
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

		mimeType := media.MimeForFile(fileID)
		class, label := media.TagForExt(fileID)

		item := fileItem{
			Name:     found.Name,
			Id:       found.Id,
			MimeType: mimeType,
			Size:     media.FormatSize(found.Size),
			TagClass: class,
			TagLabel: label,
		}

		viewURL := "/file/" + urlPath(found.Id)
		videoMime := mimeType
		if transcoder.IsTranscodeCompleted(found.Id) {
			viewURL = "/hls/admin/" + transcoder.HashString(found.Id) + "/index.m3u8"
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

func HandleFile() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := getPathValue(r, "id")
		log.Printf("Servindo arquivo: %s", fileID)
		streamLocalFile(w, r, fileID)
	}
}

func HandleM3U(publicURL, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		isAdmin := ok && constantTimeEqual(u, authUser) && constantTimeEqual(p, authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = users.AuthenticateUser(u, p)
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
				isValidUser = users.AuthenticateUser(u, p)
			}
		}

		if !isAdmin && !isValidUser {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		catalog.CacheMutex.RLock()
		cat := catalog.Cache
		catalog.CacheMutex.RUnlock()

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
				if transcoder.IsTranscodeCompleted(id) {
					return fmt.Sprintf("%s/hls/admin/%s/index.m3u8", host, transcoder.HashString(id))
				}
				return fmt.Sprintf("%s/file/%s", host, urlPath(id))
			}
		} else {
			streamURL = func(id string) string {
				if transcoder.IsTranscodeCompleted(id) {
					return fmt.Sprintf("%s/hls/%s/%s/%s/index.m3u8", host, u, p, transcoder.HashString(id))
				}
				return fmt.Sprintf("%s/movie/%s/%s/%s", host, u, p, urlPath(id))
			}
		}

		writeM3UEntry := func(v types.SyncVideo, group string) {
			ep := media.ExtractEpisode(v.Id)
			tvgID := fmt.Sprintf("OF-%s", transcoder.HashString(v.Id))
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

func HandleXtream(publicURL, authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		isAdmin := ok && constantTimeEqual(u, authUser) && constantTimeEqual(p, authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = users.AuthenticateUser(u, p)
		}

		if !isAdmin && !isValidUser {
			u = r.URL.Query().Get("username")
			p = r.URL.Query().Get("password")
			isAdmin = (authUser != "" && authPass != "" && u == authUser && p == authPass)
			if !isAdmin {
				isValidUser = users.AuthenticateUser(u, p)
			}
		}

		if !isAdmin && !isValidUser {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"user_info":{"auth":0}}`))
			return
		}

		action := r.URL.Query().Get("action")

		catalog.CacheMutex.RLock()
		cat := catalog.Cache
		catalog.CacheMutex.RUnlock()

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
			vodID := catalog.CanonicalCatalogFileID(r.URL.Query().Get("vod_id"))
			video, categoryID, categoryName, ok := catalog.FindCatalogVideo(cat, vodID)
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

			addVod := func(v types.SyncVideo, cID string, order int) {
				if !shouldPublishVideo(v) {
					return
				}
				containerExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(v.Id)), ".")
				if containerExt == "" {
					containerExt = "mp4"
				}
				directSource := xtreamStreamURL(host, u, p, v.Id, isAdmin, true)
				if transcoder.IsTranscodeCompleted(v.Id) {
					containerExt = "m3u8"
				}

				var epInfo *EpisodeInfo
				ep := media.ExtractEpisode(v.Name)
				if ep == "" {
					ep = media.ExtractEpisode(v.Id)
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

func HandleXtreamFile(authUser, authPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.PathValue("user")
		p := r.PathValue("pass")

		isAdmin := (authUser != "" && authPass != "" && u == authUser && p == authPass)
		isValidUser := false
		if !isAdmin {
			isValidUser = users.AuthenticateUser(u, p)
		}

		if !isAdmin && !isValidUser {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		fileID := catalog.CanonicalCatalogFileID(getPathValue(r, "file"))

		if (boolEnv("HIDE_UNREADY") || boolEnv("XTREAM_HIDE_UNREADY")) && !transcoder.IsTranscodeCompleted(fileID) {
			http.Error(w, "Mídia ainda em processamento", http.StatusNotFound)
			return
		}

		if isValidUser {
			users.TrackStreamStart(u, fileID)
			defer users.TrackStreamEnd(u, fileID)
		}

		if transcoder.IsTranscodeCompleted(fileID) {
			log.Printf("Redirecionando Xtream para HLS: %s", fileID)
			if isAdmin {
				http.Redirect(w, r, fmt.Sprintf(
					"/hls/admin/%s/index.m3u8",
					transcoder.HashString(fileID),
				), http.StatusFound)
				return
			}
			http.Redirect(w, r, fmt.Sprintf(
				"/hls/%s/%s/%s/index.m3u8",
				url.PathEscape(u),
				url.PathEscape(p),
				transcoder.HashString(fileID),
			), http.StatusFound)
			return
		}

		log.Printf("Servindo Xtream arquivo: %s", fileID)
		streamLocalFile(w, r, fileID)
	}
}

func HandleSearch(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := sanitizeQuery(r.URL.Query().Get("q"))
		if q == "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		log.Printf("Buscando: %s", q)
		qLower := strings.ToLower(q)

		catalog.CacheMutex.RLock()
		cat := catalog.Cache
		catalog.CacheMutex.RUnlock()

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
					Size:     media.FormatSize(v.Size),
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
						Size:     media.FormatSize(v.Size),
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

func sanitizeQuery(q string) string {
	q = strings.ReplaceAll(q, "'", "")
	q = strings.ReplaceAll(q, "\\", "")
	q = strings.ReplaceAll(q, "\"", "")
	return strings.TrimSpace(q)
}

func HandleHLSStream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.PathValue("user")
		p := r.PathValue("pass")

		if !users.AuthenticateUser(u, p) {
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

		destFile := filepath.Join(transcoder.TranscodeDir, cleanFolder, cleanFile)
		if _, err := os.Stat(destFile); err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		if cleanFile == "index.m3u8" || strings.HasSuffix(cleanFile, ".ts") {
			fileID := transcoder.GetFileIDFromHash(cleanFolder)
			if fileID != "" {
				users.TrackHLSRequest(u, fileID)
			}
		}

		http.ServeFile(w, r, destFile)
	}
}

func HandleHLSAdminStream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		folder := r.PathValue("folder")
		file := r.PathValue("file")

		cleanFolder := filepath.Clean(folder)
		cleanFile := filepath.Clean(file)
		if strings.Contains(cleanFolder, "..") || strings.Contains(cleanFile, "..") {
			http.Error(w, "Access Denied", http.StatusForbidden)
			return
		}

		destFile := filepath.Join(transcoder.TranscodeDir, cleanFolder, cleanFile)
		if _, err := os.Stat(destFile); err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		http.ServeFile(w, r, destFile)
	}
}

func HandleAdmin(tmpl *template.Template, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := struct {
			Users     []users.UserStatusResponse
			PublicURL string
		}{
			Users:     users.GetUsersStatusList(),
			PublicURL: publicURL,
		}
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("Erro ao renderizar template do admin: %v", err)
		}
	}
}

func HandleAdminCreateUser() http.HandlerFunc {
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
		user, err := users.CreateUser(req.Username, req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)
	}
}

func HandleAdminToggleUser() http.HandlerFunc {
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
		active, err := users.ToggleUser(req.Username)
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

func HandleAdminResetPassword() http.HandlerFunc {
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
		newPass, err := users.ResetUserPassword(req.Username)
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

func HandleAdminDeleteUser() http.HandlerFunc {
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
		if err := users.DeleteUser(req.Username); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
		})
	}
}

func HandleAdminUsersStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(users.GetUsersStatusList())
	}
}

func HandleAdminTranscodeStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(transcoder.GetTranscodeStatusList())
	}
}

func HandleAdminTranscodeRetry() http.HandlerFunc {
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
		if err := transcoder.RetryFailedJob(req.FileID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	}
}

func HandleAdminScan() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Método não suportado", http.StatusMethodNotAllowed)
			return
		}

		cat := catalog.ScanLocalFolder()
		transcoder.EnqueueNewCatalogVideos(cat)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "success",
			"folders":     len(cat.Folders),
			"root_videos": len(cat.RootVideos),
			"last_sync":   cat.LastSync,
		})
	}
}

type diskUsageResponse struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Percent   float64 `json:"percent"`
}

func HandleAdminDiskUsage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(catalog.LocalRoot, &stat); err != nil {
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

func HandleAdminUpload() http.HandlerFunc {
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

				if !media.IsVideoExt(relPath) {
					errs = append(errs, relPath+": formato não suportado")
					continue
				}

				dest := filepath.Clean(filepath.Join(catalog.LocalRoot, filepath.FromSlash(relPath)))
				rootClean := filepath.Clean(catalog.LocalRoot)
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
			cat := catalog.ScanLocalFolder()
			transcoder.EnqueueNewCatalogVideos(cat)
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
