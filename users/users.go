package users

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"onlyflix/catalog"
	"onlyflix/database"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	Username       string    `json:"username"`
	Password       string    `json:"password"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"created_at"`
	ExpDate        string    `json:"exp_date"`
	MaxConnections int       `json:"max_connections"`
}

type StreamInfo struct {
	FileID    string    `json:"file_id"`
	FileName  string    `json:"file_name"`
	StartedAt time.Time `json:"started_at"`
}

type UserStatusResponse struct {
	Username       string       `json:"username"`
	Password       string       `json:"password"`
	Active         bool         `json:"active"`
	CreatedAt      time.Time    `json:"created_at"`
	ExpDate        string       `json:"exp_date"`
	MaxConnections int          `json:"max_connections"`
	IsOnline       bool         `json:"is_online"`
	ActiveStreams  []StreamInfo `json:"active_streams"`
}

var (
	activeConnections = make(map[string]map[string]time.Time)
	connMutex         sync.Mutex

	activeHLSSessions = make(map[string]map[string]time.Time)
	hlsMutex          sync.Mutex
)

const bcryptCost = 10

func hashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(bytes), err
}

func isBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") || strings.HasPrefix(s, "$2y$")
}

func upgradePassword(username, password string) {
	hash, err := hashPassword(password)
	if err != nil {
		return
	}
	database.DB.Exec("UPDATE users SET password=? WHERE username=?", hash, username)
}

func scanUser(scanner interface {
	Scan(dest ...interface{}) error
}) (User, error) {
	var u User
	var active int
	var createdStr string
	var expDate sql.NullString
	var maxConns int
	err := scanner.Scan(&u.Username, &u.Password, &active, &createdStr, &expDate, &maxConns)
	if err != nil {
		return User{}, err
	}
	u.Active = active == 1
	u.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	u.ExpDate = expDate.String
	u.MaxConnections = maxConns
	if u.MaxConnections < 1 {
		u.MaxConnections = 1
	}
	return u, nil
}

func isExpired(expDate string) bool {
	if expDate == "" {
		return false
	}
	t, err := time.Parse("2006-01-02", expDate)
	if err != nil {
		return false
	}
	return time.Now().After(t)
}

func LoadUsers() error {
	return nil
}

func AuthenticateUser(username, password string) bool {
	var storedPassword string
	var active int
	var expDate sql.NullString
	err := database.DB.QueryRow(
		"SELECT password, active, exp_date FROM users WHERE username=?", username,
	).Scan(&storedPassword, &active, &expDate)
	if err != nil || active != 1 {
		return false
	}

	if isExpired(expDate.String) {
		return false
	}

	if isBcryptHash(storedPassword) {
		if bcrypt.CompareHashAndPassword([]byte(storedPassword), []byte(password)) == nil {
			return true
		}
		return false
	}

	if storedPassword == password {
		upgradePassword(username, password)
		return true
	}
	return false
}

func allUsers() ([]User, error) {
	rows, err := database.DB.Query("SELECT username, password, active, created_at, exp_date, max_connections FROM users ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			continue
		}
		users = append(users, u)
	}
	return users, nil
}

func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
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

func CreateUser(username, password, expDate string, maxConnections int) (User, error) {
	if username == "" {
		username = generateRandomUsername()
	}
	if password == "" {
		password = generateRandomString(8)
	}

	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)

	if username == "" || password == "" {
		return User{}, fmt.Errorf("usuário e senha não podem ser vazios")
	}

	var exists int
	database.DB.QueryRow("SELECT COUNT(*) FROM users WHERE username=?", username).Scan(&exists)
	if exists > 0 {
		return User{}, fmt.Errorf("usuário '%s' já existe", username)
	}

	hash, err := hashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("erro ao criar hash: %v", err)
	}

	if maxConnections < 1 {
		maxConnections = 1
	}

	if expDate != "" {
		if _, err := time.Parse("2006-01-02", expDate); err != nil {
			return User{}, fmt.Errorf("data de expiração inválida (use formato AAAA-MM-DD)")
		}
	}

	now := time.Now()
	_, err = database.DB.Exec(
		"INSERT INTO users (username, password, active, created_at, exp_date, max_connections) VALUES (?, ?, 1, ?, ?, ?)",
		username, hash, now.Format(time.RFC3339), expDate, maxConnections,
	)
	if err != nil {
		return User{}, fmt.Errorf("erro ao criar usuário: %v", err)
	}

	return User{
		Username:       username,
		Password:       password,
		Active:         true,
		CreatedAt:      now,
		ExpDate:        expDate,
		MaxConnections: maxConnections,
	}, nil
}

func ToggleUser(username string) (bool, error) {
	res, err := database.DB.Exec(
		"UPDATE users SET active = CASE WHEN active = 1 THEN 0 ELSE 1 END WHERE username=?",
		username,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, fmt.Errorf("usuário '%s' não encontrado", username)
	}

	var active int
	database.DB.QueryRow("SELECT active FROM users WHERE username=?", username).Scan(&active)
	return active == 1, nil
}

func ResetUserPassword(username string) (string, error) {
	newPassword := generateRandomString(8)
	hash, err := hashPassword(newPassword)
	if err != nil {
		return "", fmt.Errorf("erro ao gerar hash: %v", err)
	}

	res, err := database.DB.Exec(
		"UPDATE users SET password=? WHERE username=?",
		hash, username,
	)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", fmt.Errorf("usuário '%s' não encontrado", username)
	}
	return newPassword, nil
}

func DeleteUser(username string) error {
	res, err := database.DB.Exec("DELETE FROM users WHERE username=?", username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("usuário '%s' não encontrado", username)
	}

	connMutex.Lock()
	delete(activeConnections, username)
	connMutex.Unlock()

	hlsMutex.Lock()
	delete(activeHLSSessions, username)
	hlsMutex.Unlock()

	return nil
}

func UpdateUserExpiry(username, expDate string) error {
	if expDate != "" {
		if _, err := time.Parse("2006-01-02", expDate); err != nil {
			return fmt.Errorf("data de expiração inválida (use formato AAAA-MM-DD)")
		}
	}
	res, err := database.DB.Exec("UPDATE users SET exp_date=? WHERE username=?", expDate, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("usuário '%s' não encontrado", username)
	}
	return nil
}

func UpdateUserMaxConnections(username string, maxConns int) error {
	if maxConns < 1 {
		maxConns = 1
	}
	res, err := database.DB.Exec("UPDATE users SET max_connections=? WHERE username=?", maxConns, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("usuário '%s' não encontrado", username)
	}
	return nil
}

func GetUserInfo(username string) (User, error) {
	row := database.DB.QueryRow(
		"SELECT username, password, active, created_at, exp_date, max_connections FROM users WHERE username=?",
		username,
	)
	return scanUser(row)
}

func CanStartStream(username string) (bool, int) {
	connMutex.Lock()
	streams := len(activeConnections[username])
	connMutex.Unlock()

	hlsMutex.Lock()
	for fileID, lastReq := range activeHLSSessions[username] {
		if time.Now().Sub(lastReq) <= 20*time.Second {
			streams++
		} else {
			delete(activeHLSSessions[username], fileID)
		}
	}
	hlsMutex.Unlock()

	maxConns := 1
	var dbMax int
	err := database.DB.QueryRow("SELECT max_connections FROM users WHERE username=?", username).Scan(&dbMax)
	if err == nil && dbMax > 0 {
		maxConns = dbMax
	}

	return streams < maxConns, maxConns
}

func TrackStreamStart(username, fileID string) {
	connMutex.Lock()
	if activeConnections[username] == nil {
		activeConnections[username] = make(map[string]time.Time)
	}
	activeConnections[username][fileID] = time.Now()
	connMutex.Unlock()
	log.Printf("[STREAM] Usuário %s iniciou streaming de: %s", username, fileID)
}

func TrackStreamEnd(username, fileID string) {
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

func TrackHLSRequest(username, fileID string) {
	hlsMutex.Lock()
	if activeHLSSessions[username] == nil {
		activeHLSSessions[username] = make(map[string]time.Time)
	}
	activeHLSSessions[username][fileID] = time.Now()
	hlsMutex.Unlock()
}

func getUserActiveStreams(username string) []StreamInfo {
	var streams []StreamInfo

	connMutex.Lock()
	if filesMap, ok := activeConnections[username]; ok {
		for fileID, startedAt := range filesMap {
			streams = append(streams, StreamInfo{
				FileID:    fileID,
				FileName:  catalog.FindFileName(fileID),
				StartedAt: startedAt,
			})
		}
	}
	connMutex.Unlock()

	hlsMutex.Lock()
	if hlsMap, ok := activeHLSSessions[username]; ok {
		now := time.Now()
		for fileID, lastReq := range hlsMap {
			if now.Sub(lastReq) <= 20*time.Second {
				streams = append(streams, StreamInfo{
					FileID:    fileID,
					FileName:  catalog.FindFileName(fileID),
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

func GetUsersStatusList() []UserStatusResponse {
	users, err := allUsers()
	if err != nil {
		log.Printf("[USERS] Erro ao listar usuários: %v", err)
		return nil
	}

	res := make([]UserStatusResponse, 0, len(users))
	for _, u := range users {
		streams := getUserActiveStreams(u.Username)
		pwdDisplay := u.Password
		if isBcryptHash(pwdDisplay) {
			pwdDisplay = "********"
		}
		res = append(res, UserStatusResponse{
			Username:       u.Username,
			Password:       pwdDisplay,
			Active:         u.Active,
			CreatedAt:      u.CreatedAt,
			ExpDate:        u.ExpDate,
			MaxConnections: u.MaxConnections,
			IsOnline:       len(streams) > 0,
			ActiveStreams:  streams,
		})
	}
	return res
}
