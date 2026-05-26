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
	"runtime"
	"strings"
	"time"
	"unicode"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

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

func handleFile(srv *drive.Service, client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.PathValue("id")
		log.Printf("Servindo arquivo: %s", fileID)

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

	fmt.Println("Preparando autenticacao...")
	client := getClient(config)

	fmt.Println("Criando servico do Google Drive...")
	srv, err := drive.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Erro ao criar servico do Drive: %v", err)
	}

	fmt.Println("Carregando template...")
	tmpl, err := template.ParseFiles("templates/templ.html")
	if err != nil {
		log.Fatalf("Erro ao parsear template: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleFolder(srv, rootFolderID, tmpl))
	mux.HandleFunc("GET /folder/{id}", handleFolder(srv, rootFolderID, tmpl))
	mux.HandleFunc("GET /view/{id}", handleView(srv, tmpl))
	mux.HandleFunc("GET /file/{id}", handleFile(srv, client))
	mux.HandleFunc("GET /search", handleSearch(srv, rootFolderID, tmpl))

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
