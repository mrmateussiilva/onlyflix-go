package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"onlyflix/catalog"
	"onlyflix/database"
	"onlyflix/handlers"
	"onlyflix/transcoder"
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

func main() {
	fmt.Println("Iniciando OnlyFlix...")

	loadEnv()

	fmt.Println("Inicializando banco de dados...")
	if err := database.Init("data/onlyflix.db"); err != nil {
		log.Fatalf("Erro ao inicializar banco de dados: %v", err)
	}
	defer database.Close()

	catalog.LocalRoot = os.Getenv("LOCAL_PATH")
	if catalog.LocalRoot == "" {
		log.Fatal("LOCAL_PATH não definida. Defina a variável de ambiente LOCAL_PATH apontando para a pasta com os vídeos.")
	}

	if info, err := os.Stat(catalog.LocalRoot); err != nil || !info.IsDir() {
		log.Fatalf("LOCAL_PATH '%s' não é um diretório válido: %v", catalog.LocalRoot, err)
	}

	fmt.Printf("Pasta local: %s\n", catalog.LocalRoot)

	authUser := os.Getenv("AUTH_USER")
	authPass := os.Getenv("AUTH_PASS")
	publicURL := os.Getenv("PUBLIC_URL")

	if authPass == "123456" || len(authPass) < 6 {
		log.Println("[AVISO] A senha do admin é muito fraca! Altere para uma senha mais segura.")
	}
	if authUser != "" {
		fmt.Printf("Proteção ativada com usuário: %s\n", authUser)
	}

	fmt.Println("Inicializando transcodificador HLS...")
	transcoder.InitTranscoder()
	go transcoder.StartTranscoderWorker()

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
	cat := catalog.ScanLocalFolder()
	transcoder.EnqueueNewCatalogVideos(cat)

	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		for range ticker.C {
			cat := catalog.ScanLocalFolder()
			transcoder.EnqueueNewCatalogVideos(cat)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", handlers.HandleLogin(authUser, authPass))
	mux.HandleFunc("POST /login", handlers.HandleLogin(authUser, authPass))
	mux.HandleFunc("GET /logout", handlers.HandleLogout())
	mux.HandleFunc("GET /", handlers.Secure(handlers.HandleFolder(tmpl), authUser, authPass))
	mux.HandleFunc("GET /folder/{id...}", handlers.Secure(handlers.HandleFolder(tmpl), authUser, authPass))
	mux.HandleFunc("GET /view/{id...}", handlers.Secure(handlers.HandleView(tmpl), authUser, authPass))
	mux.HandleFunc("GET /file/{id...}", handlers.Secure(handlers.HandleFile(), authUser, authPass))
	mux.HandleFunc("GET /search", handlers.Secure(handlers.HandleSearch(tmpl), authUser, authPass))
	mux.HandleFunc("GET /playlist.m3u", handlers.Secure(handlers.HandleM3U(publicURL, authUser, authPass), authUser, authPass))

	mux.HandleFunc("GET /hls/{user}/{pass}/{folder}/{file}", handlers.HandleHLSStream())
	mux.HandleFunc("GET /hls/admin/{folder}/{file}", handlers.Secure(handlers.HandleHLSAdminStream(), authUser, authPass))

	mux.HandleFunc("GET /player_api.php", handlers.HandleXtream(publicURL, authUser, authPass))
	mux.HandleFunc("GET /get.php", handlers.HandleM3U(publicURL, authUser, authPass))
	mux.HandleFunc("GET /movie/{user}/{pass}/{file...}", handlers.HandleXtreamFile(authUser, authPass))

	mux.HandleFunc("GET /admin", handlers.Secure(handlers.HandleAdmin(adminTmpl, publicURL), authUser, authPass))
	mux.HandleFunc("POST /admin/users", handlers.Secure(handlers.HandleAdminCreateUser(), authUser, authPass))
	mux.HandleFunc("POST /admin/users/toggle", handlers.Secure(handlers.HandleAdminToggleUser(), authUser, authPass))
	mux.HandleFunc("POST /admin/users/reset-password", handlers.Secure(handlers.HandleAdminResetPassword(), authUser, authPass))
	mux.HandleFunc("DELETE /admin/users", handlers.Secure(handlers.HandleAdminDeleteUser(), authUser, authPass))
	mux.HandleFunc("GET /admin/users/status", handlers.Secure(handlers.HandleAdminUsersStatus(), authUser, authPass))

	mux.HandleFunc("GET /admin/transcode/status", handlers.Secure(handlers.HandleAdminTranscodeStatus(), authUser, authPass))
	mux.HandleFunc("POST /admin/transcode/retry", handlers.Secure(handlers.HandleAdminTranscodeRetry(), authUser, authPass))
	mux.HandleFunc("POST /admin/scan", handlers.Secure(handlers.HandleAdminScan(), authUser, authPass))

	mux.HandleFunc("POST /admin/upload", handlers.Secure(handlers.HandleAdminUpload(), authUser, authPass))
	mux.HandleFunc("GET /admin/disk-usage", handlers.Secure(handlers.HandleAdminDiskUsage(), authUser, authPass))

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	fmt.Printf("Servidor rodando em http://0.0.0.0:%s\n", port)
	fmt.Printf("Acesse de outros dispositivos via http://SEU_IP:%s\n", port)
	if err := http.ListenAndServe("0.0.0.0:"+port, corsMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
