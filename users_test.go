package main

import (
	"os"
	"testing"
)

func TestUserManagement(t *testing.T) {
	// Use a temporary file for tests
	usersFile = "users_test.json"
	defer os.Remove(usersFile)

	// Clean up environment
	usersList = []User{}

	// Test 1: Load users (should be empty)
	err := loadUsers()
	if err != nil {
		t.Fatalf("Erro ao carregar usuários: %v", err)
	}
	if len(usersList) != 0 {
		t.Errorf("Esperava 0 usuários, obteve %d", len(usersList))
	}

	// Test 2: Create a user with specified credentials
	username := "test_user"
	password := "secret123"
	user, err := createUser(username, password)
	if err != nil {
		t.Fatalf("Erro ao criar usuário: %v", err)
	}
	if user.Username != username || user.Password != password {
		t.Errorf("Usuário criado incorretamente: %+v", user)
	}

	// Test 3: Create duplicate user (should fail)
	_, err = createUser(username, "another_pass")
	if err == nil {
		t.Errorf("Esperava erro ao criar usuário duplicado, mas passou")
	}

	// Test 4: Authenticate user (success & failure)
	if !authenticateUser(username, password) {
		t.Errorf("Falha ao autenticar com credenciais corretas")
	}
	if authenticateUser(username, "wrong_password") {
		t.Errorf("Autenticou com senha incorreta")
	}
	if authenticateUser("non_existent", password) {
		t.Errorf("Autenticou usuário inexistente")
	}

	// Test 5: Toggle user active status
	active, err := toggleUser(username)
	if err != nil {
		t.Fatalf("Erro ao alterar status: %v", err)
	}
	if active {
		t.Errorf("Esperava que o status fosse desativado (false)")
	}

	// Authenticate inactive user (should fail)
	if authenticateUser(username, password) {
		t.Errorf("Autenticou usuário inativo")
	}

	// Toggle back to active
	active, err = toggleUser(username)
	if err != nil {
		t.Fatalf("Erro ao alterar status de volta: %v", err)
	}
	if !active {
		t.Errorf("Esperava que o status voltasse a ser ativo (true)")
	}

	// Test 6: Reset user password
	newPass, err := resetUserPassword(username)
	if err != nil {
		t.Fatalf("Erro ao resetar senha: %v", err)
	}
	if newPass == password {
		t.Errorf("Esperava uma nova senha diferente da anterior")
	}
	if !authenticateUser(username, newPass) {
		t.Errorf("Falha ao autenticar com a nova senha resetada")
	}
	if authenticateUser(username, password) {
		t.Errorf("Autenticou com a senha antiga após reset")
	}

	// Test 7: Stream connection tracking
	fileID := "movie1.mp4"
	trackStreamStart(username, fileID)

	streams := getUserActiveStreams(username)
	if len(streams) != 1 {
		t.Errorf("Esperava 1 conexão ativa, obteve %d", len(streams))
	}
	if streams[0].FileID != fileID {
		t.Errorf("Esperava fileID %s, obteve %s", fileID, streams[0].FileID)
	}

	statusList := getUsersStatusList()
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

	// End streaming
	trackStreamEnd(username, fileID)
	streams = getUserActiveStreams(username)
	if len(streams) != 0 {
		t.Errorf("Esperava 0 conexões ativas após encerrar stream, obteve %d", len(streams))
	}

	// Test 8: Delete user
	err = deleteUser(username)
	if err != nil {
		t.Fatalf("Erro ao deletar usuário: %v", err)
	}
	if authenticateUser(username, newPass) {
		t.Errorf("Autenticou usuário excluído")
	}
}
