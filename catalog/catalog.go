package catalog

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"onlyflix/database"
	"onlyflix/media"
	"onlyflix/types"
)

var (
	Cache  *types.SyncCatalog
	CacheMutex sync.RWMutex
	LocalRoot  string
)

func ResolvePath(root, id string) (string, error) {
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

func FindCatalogVideo(cat *types.SyncCatalog, fileID string) (types.SyncVideo, string, string, bool) {
	if cat == nil {
		return types.SyncVideo{}, "", "", false
	}
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
	return types.SyncVideo{}, "", "", false
}

func CanonicalCatalogFileID(fileID string) string {
	CacheMutex.RLock()
	cat := Cache
	CacheMutex.RUnlock()

	if cat == nil {
		return fileID
	}

	candidates := []string{fileID}
	videoExts := []string{".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".m4v", ".ts", ".mpeg", ".mpg", ".3gp", ".m3u8"}
	for _, ext := range videoExts {
		if strings.HasSuffix(strings.ToLower(fileID), ext) {
			candidates = append(candidates, fileID[:len(fileID)-len(ext)])
		} else {
			candidates = append(candidates, fileID+ext)
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

func FindFileName(fileID string) string {
	CacheMutex.RLock()
	cat := Cache
	CacheMutex.RUnlock()

	if cat == nil {
		return fileID
	}

	for _, v := range cat.RootVideos {
		if v.Id == fileID {
			return v.Name
		}
	}

	for _, f := range cat.Folders {
		for _, v := range f.Videos {
			if v.Id == fileID {
				return v.Name
			}
		}
	}

	parts := strings.Split(fileID, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return fileID
}

func saveCatalogToDB(cat *types.SyncCatalog) {
	b, err := json.Marshal(cat)
	if err != nil {
		return
	}
	database.DB.Exec(
		"INSERT OR REPLACE INTO catalog_cache (id, data, updated_at) VALUES (1, ?, ?)",
		string(b), cat.LastSync.Format(time.RFC3339),
	)
}

func ScanLocalFolder() *types.SyncCatalog {
	log.Println("[SCAN] Iniciando scan da pasta local...")

	cat := &types.SyncCatalog{
		LastSync: time.Now(),
	}

	rootClean := filepath.Clean(LocalRoot)
	if _, err := os.Stat(rootClean); err != nil {
		log.Printf("[SCAN] Erro ao acessar diretório raiz: %v", err)
		return cat
	}

	foldersByID := make(map[string]*types.SyncFolder)

	err := filepath.WalkDir(rootClean, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			log.Printf("[SCAN] Erro ao acessar %s: %v", path, err)
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !media.IsVideoExt(entry.Name()) {
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
		video := types.SyncVideo{
			Id:      relPath,
			Name:    media.CleanName(entry.Name(), true),
			Size:    info.Size(),
			Created: info.ModTime(),
		}

		if dir == "." {
			cat.RootVideos = append(cat.RootVideos, video)
			return nil
		}

		folder := foldersByID[dir]
		if folder == nil {
			folder = &types.SyncFolder{
				Id:   dir,
				Name: media.CleanName(filepath.Base(dir), false),
			}
			foldersByID[dir] = folder
		}
		folder.Videos = append(folder.Videos, video)
		return nil
	})
	if err != nil {
		log.Printf("[SCAN] Erro ao escanear pasta local: %v", err)
	}

	sortVideoByEpisode := func(videos []types.SyncVideo) {
		sort.SliceStable(videos, func(i, j int) bool {
			ei := media.EpisodeSortKey(videos[i].Name)
			ej := media.EpisodeSortKey(videos[j].Name)
			if ei != -1 && ej != -1 {
				return ei < ej
			}
			return videos[i].Created.Before(videos[j].Created)
		})
	}

	sortVideoByEpisode(cat.RootVideos)
	media.SmartRename(cat.RootVideos, "Geral")

	for _, folder := range foldersByID {
		if len(folder.Videos) == 0 {
			continue
		}
		sortVideoByEpisode(folder.Videos)
		media.SmartRename(folder.Videos, folder.Name)
		cat.Folders = append(cat.Folders, *folder)
	}
	sort.Slice(cat.Folders, func(i, j int) bool {
		return cat.Folders[i].Name < cat.Folders[j].Name
	})

	cleanFolders := make([]types.SyncFolder, 0, len(cat.Folders))
	for _, f := range cat.Folders {
		if len(f.Videos) > 0 {
			cleanFolders = append(cleanFolders, f)
		}
	}
	cat.Folders = cleanFolders

	CacheMutex.Lock()
	Cache = cat
	CacheMutex.Unlock()

	saveCatalogToDB(cat)

	log.Printf("[SCAN] Scan finalizado: %d pastas, %d vídeos na raiz", len(cat.Folders), len(cat.RootVideos))
	return cat
}
