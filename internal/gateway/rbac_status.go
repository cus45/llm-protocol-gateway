package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// AuthStatusFull is what /__auth/status returns once authenticated: the session
// basics plus the resolved RBAC view the console needs to render the sidebar
// and to hide actions it may not perform.
type AuthStatusFull struct {
	adminAuthStatus
	RoleID      string   `json:"roleId,omitempty"`
	RoleName    string   `json:"roleName,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	AvatarURL   string   `json:"avatarUrl,omitempty"`
	Scope       string   `json:"scope,omitempty"`
	Permissions []string `json:"permissions"`
}

// permissionKeys flattens a principal's permissions into the wire format.
// Administrators report the wildcard; the console treats it as "everything".
func permissionKeys(p principal) []string {
	if p.IsAdmin {
		return []string{string(domain.PermAll)}
	}
	out := make([]string, 0, len(p.perms))
	for _, perm := range domain.AllPermissions() {
		if p.perms[perm] {
			out = append(out, string(perm))
		}
	}
	return out
}

// authStatusFor builds the status payload for a request, resolving identity
// and principal exactly once.
func (s *Server) authStatusFor(r *http.Request) AuthStatusFull {
	localBypass := isLocalAdminBypass(r)
	status := AuthStatusFull{
		adminAuthStatus: adminAuthStatus{
			Configured:  s.adminAuthConfigured(),
			RequireAuth: !localBypass,
			LocalBypass: localBypass,
		},
		Permissions: []string{},
	}
	var identity sessionIdentity
	var ok bool
	if localBypass {
		identity, ok = adminIdentity(), true
	} else {
		identity, ok = s.sessionIdentity(r)
	}
	if !ok {
		return status
	}
	p := s.principalFor(identity)
	role := s.roleByID(p.RoleID)
	status.Authenticated = true
	status.Role = string(p.Role)
	status.RoleID = p.RoleID
	status.RoleName = role.Name
	status.Scope = string(p.scopeOf())
	status.Username = identity.Username
	status.UserID = identity.UserID
	status.DisplayName = s.resolveUserName(identity.UserID)
	status.Permissions = permissionKeys(p)
	if identity.UserID != legacyAdminUserID && s.userStore != nil {
		if user, err := s.userStore.UserByID(identity.UserID); err == nil {
			status.AvatarURL = user.AvatarURL
		}
	}
	return status
}

// handleProfileUpdate lets the signed-in user change their own display name and
// avatar. Username and role stay an administrator's call.
func (s *Server) handleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	localBypass := isLocalAdminBypass(r)
	identity, ok := s.sessionIdentity(r)
	if !ok && !localBypass {
		writeOpenAIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if identity.UserID == "" || identity.UserID == legacyAdminUserID || s.userStore == nil {
		writeOpenAIError(w, http.StatusBadRequest, "the implicit administrator account cannot be edited here; create a named administrator account instead")
		return
	}
	user, err := s.userStore.UserByID(identity.UserID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "user not found")
		return
	}
	var payload struct {
		DisplayName *string `json:"displayName"`
		AvatarURL   *string `json:"avatarUrl"`
	}
	if !decodeJSONBody(w, r, &payload) {
		return
	}
	if payload.DisplayName != nil {
		name := strings.TrimSpace(*payload.DisplayName)
		if len([]rune(name)) > 40 {
			writeOpenAIError(w, http.StatusBadRequest, "display name is too long")
			return
		}
		user.DisplayName = name
	}
	if payload.AvatarURL != nil {
		avatar, err := normalizeAvatar(*payload.AvatarURL)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, err.Error())
			return
		}
		user.AvatarURL = avatar
	}
	if err := s.userStore.UpdateUser(user); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logs.AddApp("info", "profile updated", user.Username)
	writeJSON(w, http.StatusOK, s.authStatusFor(r))
}

// avatarMaxBytes caps a stored avatar so the users table and session payload
// stay small. 512 KiB of base64 decodes to ~384 KiB of image.
const avatarMaxBytes = 512 * 1024

// avatarDataURLPrefixes are the only image formats accepted inline. SVG is
// deliberately excluded: it can carry script and would be served from the
// console origin.
var avatarDataURLPrefixes = []string{
	"data:image/png;base64,",
	"data:image/jpeg;base64,",
	"data:image/webp;base64,",
	"data:image/gif;base64,",
}

// normalizeAvatar validates an avatar value: either a same-origin path served
// by the gateway or an inline data URL of an allowed raster format.
func normalizeAvatar(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "/") {
		if strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("invalid avatar path")
		}
		return value, nil
	}
	for _, prefix := range avatarDataURLPrefixes {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		payload := strings.TrimPrefix(value, prefix)
		if len(payload) > avatarMaxBytes {
			return "", fmt.Errorf("avatar is larger than 384KB")
		}
		if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
			return "", fmt.Errorf("avatar is not valid base64")
		}
		return value, nil
	}
	return "", fmt.Errorf("avatar must be a PNG, JPEG, WebP or GIF data URL")
}

// handleListRoleOptions is a compact role list for user-edit dropdowns: id,
// name and scope only.
func (s *Server) handleListRoleOptions(w http.ResponseWriter, r *http.Request) {
	roles, err := s.listRoles()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type option struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Builtin     bool   `json:"builtin"`
		Scope       string `json:"scope"`
	}
	out := make([]option, 0, len(roles))
	for _, role := range roles {
		out = append(out, option{ID: role.ID, Name: role.Name, Description: role.Description, Builtin: role.Builtin, Scope: string(role.EffectiveScope())})
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeRawJSON is a small helper kept so handlers can report a clear 400.
func decodeRawJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}
