package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

type User struct {
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

type StreamInfo struct {
	FileID    string    `json:"file_id"`
	FileName  string    `json:"file_name"`
	StartedAt time.Time `json:"started_at"`
}

type UserStatusResponse struct {
	Username      string       `json:"username"`
	Password      string       `json:"password"`
	Active        bool         `json:"active"`
	CreatedAt     time.Time    `json:"created_at"`
	IsOnline      bool         `json:"is_online"`
	ActiveStreams []StreamInfo `json:"active_streams"`
}

var (
	usersList   []User
	userMutex   sync.RWMutex
	usersFile   = "users.json"

	// activeConnections: username -> fileID -> start time (progressive streams)
	activeConnections = make(map[string]map[string]time.Time)
	connMutex         sync.Mutex

	// activeHLSSessions: username -> fileID -> last segment request time
	activeHLSSessions = make(map[string]map[string]time.Time)
	hlsMutex          sync.Mutex
)

func loadUsers() error {
	userMutex.Lock()
	defer userMutex.Unlock()

	if _, err := os.Stat(usersFile); os.IsNotExist(err) {
		usersList = []User{}
		return nil
	}

	b, err := os.ReadFile(usersFile)
	if err != nil {
		return err
	}

	return json.Unmarshal(b, &usersList)
}

func saveUsersNoLock() error {
	b, err := json.MarshalIndent(usersList, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(usersFile, b, 0644)
}

func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			// Fallback if crypto fails
			b[i] = charset[time.Now().UnixNano()%int64(len(charset))]
			continue
		}
		b[i] = charset[num.Int64()]
	}
	return string(b)
}

func generateRandomUsername() string {
	return fmt.Sprintf("flix_%s", generateRandomString(5))
}

func authenticateUser(username, password string) bool {
	userMutex.RLock()
	defer userMutex.RUnlock()
	for _, u := range usersList {
		if u.Username == username && u.Password == password {
			return u.Active
		}
	}
	return false
}

func createUser(username, password string) (User, error) {
	userMutex.Lock()
	defer userMutex.Unlock()

	if username == "" {
		username = generateRandomUsername()
	}
	if password == "" {
		password = generateRandomString(8)
	}

	// Clean fields
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)

	if username == "" || password == "" {
		return User{}, fmt.Errorf("usuário e senha não podem ser vazios")
	}

	// Check duplicate
	for _, u := range usersList {
		if u.Username == username {
			return User{}, fmt.Errorf("usuário '%s' já existe", username)
		}
	}

	newUser := User{
		Username:  username,
		Password:  password,
		Active:    true,
		CreatedAt: time.Now(),
	}

	usersList = append(usersList, newUser)
	if err := saveUsersNoLock(); err != nil {
		log.Printf("[USERS] Erro ao salvar usuários: %v", err)
	}

	return newUser, nil
}

func toggleUser(username string) (bool, error) {
	userMutex.Lock()
	defer userMutex.Unlock()

	for i, u := range usersList {
		if u.Username == username {
			usersList[i].Active = !usersList[i].Active
			if err := saveUsersNoLock(); err != nil {
				log.Printf("[USERS] Erro ao salvar usuários: %v", err)
			}
			return usersList[i].Active, nil
		}
	}
	return false, fmt.Errorf("usuário '%s' não encontrado", username)
}

func resetUserPassword(username string) (string, error) {
	userMutex.Lock()
	defer userMutex.Unlock()

	newPassword := generateRandomString(8)
	for i, u := range usersList {
		if u.Username == username {
			usersList[i].Password = newPassword
			if err := saveUsersNoLock(); err != nil {
				log.Printf("[USERS] Erro ao salvar usuários: %v", err)
			}
			return newPassword, nil
		}
	}
	return "", fmt.Errorf("usuário '%s' não encontrado", username)
}

func deleteUser(username string) error {
	userMutex.Lock()
	defer userMutex.Unlock()

	found := false
	for i, u := range usersList {
		if u.Username == username {
			usersList = append(usersList[:i], usersList[i+1:]...)
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("usuário '%s' não encontrado", username)
	}

	// Clean up connections
	connMutex.Lock()
	delete(activeConnections, username)
	connMutex.Unlock()

	hlsMutex.Lock()
	delete(activeHLSSessions, username)
	hlsMutex.Unlock()

	if err := saveUsersNoLock(); err != nil {
		log.Printf("[USERS] Erro ao salvar usuários: %v", err)
	}
	return nil
}

func trackStreamStart(username, fileID string) {
	connMutex.Lock()
	if activeConnections[username] == nil {
		activeConnections[username] = make(map[string]time.Time)
	}
	activeConnections[username][fileID] = time.Now()
	connMutex.Unlock()
	log.Printf("[STREAM] Usuário %s iniciou streaming de: %s", username, fileID)
}

func trackStreamEnd(username, fileID string) {
	connMutex.Lock()
	if activeConnections[username] != nil {
		delete(activeConnections[username], fileID)
		if len(activeConnections[username]) == 0 {
			delete(activeConnections, username)
		}
	}
	connMutex.Unlock()
	log.Printf("[STREAM] Usuário %s encerrou streaming de: %s", username, fileID)
}

func trackHLSRequest(username, fileID string) {
	hlsMutex.Lock()
	if activeHLSSessions[username] == nil {
		activeHLSSessions[username] = make(map[string]time.Time)
	}
	activeHLSSessions[username][fileID] = time.Now()
	hlsMutex.Unlock()
}

func getUserActiveStreams(username string) []StreamInfo {
	var streams []StreamInfo

	// Add progressive streams
	connMutex.Lock()
	if filesMap, ok := activeConnections[username]; ok {
		for fileID, startedAt := range filesMap {
			streams = append(streams, StreamInfo{
				FileID:    fileID,
				FileName:  getFileNameFromCache(fileID),
				StartedAt: startedAt,
			})
		}
	}
	connMutex.Unlock()

	// Add HLS streams (pruning expired ones > 20s)
	hlsMutex.Lock()
	if hlsMap, ok := activeHLSSessions[username]; ok {
		now := time.Now()
		for fileID, lastReq := range hlsMap {
			if now.Sub(lastReq) <= 20*time.Second {
				streams = append(streams, StreamInfo{
					FileID:    fileID,
					FileName:  getFileNameFromCache(fileID),
					StartedAt: lastReq,
				})
			} else {
				delete(hlsMap, fileID)
			}
		}
		if len(hlsMap) == 0 {
			delete(activeHLSSessions, username)
		}
	}
	hlsMutex.Unlock()

	return streams
}

func getFileNameFromCache(fileID string) string {
	cacheMutex.RLock()
	cat := catalogCache
	cacheMutex.RUnlock()

	if cat == nil {
		return fileID
	}

	// Check root
	for _, v := range cat.RootVideos {
		if v.Id == fileID {
			return v.Name
		}
	}

	// Check folders
	for _, f := range cat.Folders {
		for _, v := range f.Videos {
			if v.Id == fileID {
				return v.Name
			}
		}
	}

	// Fallback
	parts := strings.Split(fileID, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return fileID
}

func getUsersStatusList() []UserStatusResponse {
	userMutex.RLock()
	defer userMutex.RUnlock()

	res := make([]UserStatusResponse, 0, len(usersList))
	for _, u := range usersList {
		streams := getUserActiveStreams(u.Username)
		res = append(res, UserStatusResponse{
			Username:      u.Username,
			Password:      u.Password,
			Active:        u.Active,
			CreatedAt:     u.CreatedAt,
			IsOnline:      len(streams) > 0,
			ActiveStreams: streams,
		})
	}
	return res
}
