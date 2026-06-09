package users

import (
	"os"
	"testing"
)

func TestUserManagement(t *testing.T) {
	usersFile = "users_test.json"
	defer os.Remove(usersFile)

	usersList = []User{}

	err := LoadUsers()
	if err != nil {
		t.Fatalf("Erro ao carregar usuários: %v", err)
	}
	if len(usersList) != 0 {
		t.Errorf("Esperava 0 usuários, obteve %d", len(usersList))
	}

	username := "test_user"
	password := "secret123"
	user, err := CreateUser(username, password)
	if err != nil {
		t.Fatalf("Erro ao criar usuário: %v", err)
	}
	if user.Username != username || user.Password != password {
		t.Errorf("Usuário criado incorretamente: %+v", user)
	}

	_, err = CreateUser(username, "another_pass")
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
