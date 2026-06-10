package users

import (
	"os"
	"testing"

	"onlyflix/database"
)

func TestUserManagement(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	if err := LoadUsers(); err != nil {
		t.Fatalf("Erro ao carregar usuários: %v", err)
	}

	username := "test_user"
	password := "secret123"
	user, err := CreateUser(username, password, "", 0)
	if err != nil {
		t.Fatalf("Erro ao criar usuário: %v", err)
	}
	if user.Username != username || user.Password != password {
		t.Errorf("Usuário criado incorretamente: %+v", user)
	}
	if user.MaxConnections != 1 {
		t.Errorf("Esperava max_connections=1, obteve %d", user.MaxConnections)
	}

	_, err = CreateUser(username, "another_pass", "", 0)
	if err == nil {
		t.Errorf("Esperava erro ao criar usuário duplicado, mas passou")
	}

	if !AuthenticateUser(username, password) {
		t.Errorf("Falha ao autenticar com credenciais corretas")
	}
	if AuthenticateUser(username, "wrong_password") {
		t.Errorf("Autenticou com senha incorreta")
	}
	if AuthenticateUser("non_existent", password) {
		t.Errorf("Autenticou usuário inexistente")
	}

	active, err := ToggleUser(username)
	if err != nil {
		t.Fatalf("Erro ao alterar status: %v", err)
	}
	if active {
		t.Errorf("Esperava que o status fosse desativado (false)")
	}

	if AuthenticateUser(username, password) {
		t.Errorf("Autenticou usuário inativo")
	}

	active, err = ToggleUser(username)
	if err != nil {
		t.Fatalf("Erro ao alterar status de volta: %v", err)
	}
	if !active {
		t.Errorf("Esperava que o status voltasse a ser ativo (true)")
	}

	newPass, err := ResetUserPassword(username)
	if err != nil {
		t.Fatalf("Erro ao resetar senha: %v", err)
	}
	if newPass == password {
		t.Errorf("Esperava uma nova senha diferente da anterior")
	}
	if !AuthenticateUser(username, newPass) {
		t.Errorf("Falha ao autenticar com a nova senha resetada")
	}
	if AuthenticateUser(username, password) {
		t.Errorf("Autenticou com a senha antiga após reset")
	}

	fileID := "movie1.mp4"
	TrackStreamStart(username, fileID)

	streams := getUserActiveStreams(username)
	if len(streams) != 1 {
		t.Errorf("Esperava 1 conexão ativa, obteve %d", len(streams))
	}
	if streams[0].FileID != fileID {
		t.Errorf("Esperava fileID %s, obteve %s", fileID, streams[0].FileID)
	}

	statusList := GetUsersStatusList()
	foundStatus := false
	for _, u := range statusList {
		if u.Username == username {
			foundStatus = true
			if !u.IsOnline {
				t.Errorf("Esperava que o usuário constasse como online no status")
			}
			if len(u.ActiveStreams) != 1 {
				t.Errorf("Esperava 1 stream ativo no status, obteve %d", len(u.ActiveStreams))
			}
		}
	}
	if !foundStatus {
		t.Errorf("Usuário não encontrado na lista de status")
	}

	TrackStreamEnd(username, fileID)
	streams = getUserActiveStreams(username)
	if len(streams) != 0 {
		t.Errorf("Esperava 0 conexões ativas após encerrar stream, obteve %d", len(streams))
	}

	err = DeleteUser(username)
	if err != nil {
		t.Fatalf("Erro ao deletar usuário: %v", err)
	}
	if AuthenticateUser(username, newPass) {
		t.Errorf("Autenticou usuário excluído")
	}
}

func TestCreateUserWithExpiry(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	user, err := CreateUser("expiry_user", "pass123", "2025-01-01", 3)
	if err != nil {
		t.Fatalf("Erro ao criar usuário com exp_date: %v", err)
	}
	if user.ExpDate != "2025-01-01" {
		t.Errorf("Esperava exp_date=2025-01-01, obteve %s", user.ExpDate)
	}
	if user.MaxConnections != 3 {
		t.Errorf("Esperava max_connections=3, obteve %d", user.MaxConnections)
	}

	if AuthenticateUser("expiry_user", "pass123") {
		t.Errorf("Autenticou usuário com data expirada")
	}

	info, err := GetUserInfo("expiry_user")
	if err != nil {
		t.Fatalf("Erro ao buscar usuário: %v", err)
	}
	if info.MaxConnections != 3 {
		t.Errorf("Esperava max_connections=3 no GetUserInfo, obteve %d", info.MaxConnections)
	}
	if info.ExpDate != "2025-01-01" {
		t.Errorf("Esperava exp_date=2025-01-01 no GetUserInfo, obteve %s", info.ExpDate)
	}
}

func TestCreateUserWithInvalidExpiry(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	_, err := CreateUser("bad_date", "pass", "not-a-date", 1)
	if err == nil {
		t.Errorf("Esperava erro ao criar usuário com data inválida")
	}
}

func TestUpdateUserExpiry(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	CreateUser("upd_user", "pass", "", 1)

	if err := UpdateUserExpiry("upd_user", "2030-06-15"); err != nil {
		t.Fatalf("Erro ao atualizar exp_date: %v", err)
	}

	info, _ := GetUserInfo("upd_user")
	if info.ExpDate != "2030-06-15" {
		t.Errorf("Esperava exp_date=2030-06-15, obteve %s", info.ExpDate)
	}
}

func TestUpdateUserMaxConnections(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	CreateUser("conn_user", "pass", "", 1)

	if err := UpdateUserMaxConnections("conn_user", 5); err != nil {
		t.Fatalf("Erro ao atualizar max_connections: %v", err)
	}

	info, _ := GetUserInfo("conn_user")
	if info.MaxConnections != 5 {
		t.Errorf("Esperava max_connections=5, obteve %d", info.MaxConnections)
	}
}

func TestCanStartStream(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	CreateUser("stream_user", "pass", "", 2)

	can, maxConns := CanStartStream("stream_user")
	if !can {
		t.Errorf("Esperava poder iniciar stream (limite=2, ativos=0)")
	}
	if maxConns != 2 {
		t.Errorf("Esperava maxConns=2, obteve %d", maxConns)
	}

	TrackStreamStart("stream_user", "movie1.mp4")

	can, _ = CanStartStream("stream_user")
	if !can {
		t.Errorf("Esperava poder iniciar 2o stream (limite=2, ativos=1)")
	}

	TrackStreamStart("stream_user", "movie2.mp4")

	can, _ = CanStartStream("stream_user")
	if can {
		t.Errorf("Esperava NAO poder iniciar 3o stream (limite=2, ativos=2)")
	}

	TrackStreamEnd("stream_user", "movie1.mp4")
	TrackStreamEnd("stream_user", "movie2.mp4")
}

func TestGetUserInfo(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	CreateUser("info_user", "secret", "2026-12-31", 4)

	info, err := GetUserInfo("info_user")
	if err != nil {
		t.Fatalf("Erro ao buscar informações: %v", err)
	}
	if info.Username != "info_user" {
		t.Errorf("Esperava username=info_user, obteve %s", info.Username)
	}
	if info.ExpDate != "2026-12-31" {
		t.Errorf("Esperava exp_date=2026-12-31, obteve %s", info.ExpDate)
	}
	if info.MaxConnections != 4 {
		t.Errorf("Esperava max_connections=4, obteve %d", info.MaxConnections)
	}
}

func TestExpiredUserAuth(t *testing.T) {
	if err := database.Init(":memory:"); err != nil {
		t.Fatalf("Erro ao iniciar DB: %v", err)
	}
	defer database.Close()

	CreateUser("exp_auth", "pass", "2020-01-01", 1)

	if AuthenticateUser("exp_auth", "pass") {
		t.Errorf("Autenticou usuário com exp_date expirada (2020-01-01)")
	}

	if err := UpdateUserExpiry("exp_auth", ""); err != nil {
		t.Fatalf("Erro ao limpar exp_date: %v", err)
	}

	if !AuthenticateUser("exp_auth", "pass") {
		t.Errorf("Falhou ao autenticar após limpar exp_date")
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
