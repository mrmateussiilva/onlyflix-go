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
	Name     string
	Id       string
	MimeType string
	Size     string
	Created  string
	TagClass string
	TagLabel string
	ViewURL  string
	IsFolder bool
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
	for port := start; port <= start+20; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		l.Close()
		return port, nil
	}
	return 0, fmt.Errorf("nenhuma porta disponivel entre %d-%d", start, start+20)
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

	code := <-codeCh
	recvState := <-stateCh

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
		fmt.Printf("Acesse manualmente: %s\n", url)
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
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
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

	for limit := 0; limit < 20; limit++ {
		f, err := srv.Files.Get(current).Fields("id, name, parents").Do()
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

		folder, err := srv.Files.Get(folderID).Fields("id, name").Do()
		if err != nil {
			http.Error(w, "Pasta nao encontrada", http.StatusNotFound)
			return
		}

		breadcrumb := buildBreadcrumb(srv, folderID, rootID)

		query := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
		res, err := srv.Files.List().Q(query).PageSize(100).
			Fields("files(id, name, mimeType, size, createdTime)").
			OrderBy("folder,name").
			Do()
		if err != nil {
			http.Error(w, "Erro ao listar arquivos", http.StatusInternalServerError)
			return
		}

		var folders, files []fileItem
		for _, f := range res.Files {
			class, label := tagForMime(f.MimeType)
			isFolder := f.MimeType == "application/vnd.google-apps.folder"
			item := fileItem{
				Name:     cleanName(f.Name, !isFolder),
				Id:       f.Id,
				MimeType: f.MimeType,
				Size:     formatSize(f.Size),
				Created:  f.CreatedTime,
				TagClass: class,
				TagLabel: label,
				IsFolder: isFolder,
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

		tmpl.Execute(w, pageData{
			Breadcrumb: breadcrumb,
			FolderName: cleanName(folder.Name, false),
			CurrentID:  folderID,
			Folders:    folders,
			Files:      files,
			ViewType:   "folder",
		})
	}
}

func handleView(srv *drive.Service, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.PathValue("id")

		file, err := srv.Files.Get(fileID).Fields("id, name, mimeType, size, parents").Do()
		if err != nil {
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

		tmpl.Execute(w, data)
	}
}

func handleFile(srv *drive.Service, client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := r.PathValue("id")

		file, err := srv.Files.Get(fileID).Fields("mimeType, size").Do()
		if err != nil {
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

func main() {
	folderURL := "https://drive.google.com/drive/folders/1TN-7mwxXRMxLezW8BZ8TlaoSORQRng85?hl=pt-br"
	rootFolderID := extractFolderID(folderURL)
	if rootFolderID == "" {
		log.Fatal("Nao foi possivel extrair o ID da pasta da URL")
	}

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

	client := getClient(config)

	srv, err := drive.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Erro ao criar servico do Drive: %v", err)
	}

	tmpl, err := template.ParseFiles("templates/templ.html")
	if err != nil {
		log.Fatalf("Erro ao parsear template: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleFolder(srv, rootFolderID, tmpl))
	mux.HandleFunc("GET /folder/{id}", handleFolder(srv, rootFolderID, tmpl))
	mux.HandleFunc("GET /view/{id}", handleView(srv, tmpl))
	mux.HandleFunc("GET /file/{id}", handleFile(srv, client))

	port, err := findPort(8080)
	if err != nil {
		log.Fatalf("Erro ao achar porta: %v", err)
	}

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
