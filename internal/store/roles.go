package store

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// ListRoles returns the custom (non-built-in) roles ordered by creation time.
// Built-in roles live in code and are added by the caller.
func (s *Store) ListRoles() ([]domain.Role, error) {
	rows, err := s.reader().Query(`SELECT id, name, description, scope, permissions, created_at, updated_at
		FROM roles ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := []domain.Role{}
	for rows.Next() {
		var role domain.Role
		var scope, permissions string
		if err := rows.Scan(&role.ID, &role.Name, &role.Description, &scope, &permissions, &role.CreatedAt, &role.UpdatedAt); err != nil {
			return nil, err
		}
		role.Scope = domain.ResourceScope(strings.TrimSpace(scope))
		for _, raw := range strings.Split(permissions, ",") {
			if key := strings.TrimSpace(raw); key != "" {
				role.Permissions = append(role.Permissions, domain.Permission(key))
			}
		}
		if role.Permissions == nil {
			role.Permissions = []domain.Permission{}
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

// RoleByID returns one custom role. Built-in roles are not in the table.
func (s *Store) RoleByID(id string) (domain.Role, error) {
	rows, err := s.reader().Query(`SELECT id, name, description, scope, permissions, created_at, updated_at
		FROM roles WHERE id = ?`, id)
	if err != nil {
		return domain.Role{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return domain.Role{}, err
		}
		return domain.Role{}, sql.ErrNoRows
	}
	var role domain.Role
	var scope, permissions string
	if err := rows.Scan(&role.ID, &role.Name, &role.Description, &scope, &permissions, &role.CreatedAt, &role.UpdatedAt); err != nil {
		return domain.Role{}, err
	}
	role.Scope = domain.ResourceScope(strings.TrimSpace(scope))
	for _, raw := range strings.Split(permissions, ",") {
		if key := strings.TrimSpace(raw); key != "" {
			role.Permissions = append(role.Permissions, domain.Permission(key))
		}
	}
	if role.Permissions == nil {
		role.Permissions = []domain.Permission{}
	}
	return role, nil
}

// SaveRole inserts or updates a custom role.
func (s *Store) SaveRole(role domain.Role) error {
	if domain.IsBuiltinRole(role.ID) {
		return errors.New("built-in roles cannot be modified")
	}
	keys := make([]string, 0, len(role.Permissions))
	for _, permission := range role.Permissions {
		if trimmed := strings.TrimSpace(string(permission)); trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	_, err := s.db.Exec(`INSERT INTO roles (id, name, description, scope, permissions, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, description = excluded.description,
			scope = excluded.scope, permissions = excluded.permissions, updated_at = excluded.updated_at`,
		role.ID, strings.TrimSpace(role.Name), strings.TrimSpace(role.Description), string(role.Scope),
		strings.Join(keys, ","), role.CreatedAt, role.UpdatedAt)
	return err
}

func (s *Store) DeleteRole(id string) error {
	if domain.IsBuiltinRole(id) {
		return errors.New("built-in roles cannot be deleted")
	}
	_, err := s.db.Exec(`DELETE FROM roles WHERE id = ?`, id)
	return err
}
