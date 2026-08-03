package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionCookieName = "cf_st_session"

// session 单个登录会话
type session struct {
	username string
	expiry   time.Time
}

// sessionStore 内存会话存储
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*session
	ttl      time.Duration
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		sessions: make(map[string]*session),
		ttl:      ttl,
	}
}

// create 创建新会话，返回 token
func (s *sessionStore) create(username string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 退化: 用时间戳兜底（极端情况）
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	token := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	// 顺带清理过期会话
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.expiry) {
			delete(s.sessions, k)
		}
	}
	s.sessions[token] = &session{
		username: username,
		expiry:   now.Add(s.ttl),
	}
	return token
}

// validate 校验 token 并续期
func (s *sessionStore) validate(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[token]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.expiry) {
		return "", false
	}
	return sess.username, true
}

// destroy 注销会话
func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// setTTL 更新会话有效期（配置热更新后调用）
func (s *sessionStore) setTTL(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = ttl
}

// loginHandler POST /api/login
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (srv *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}

	// 实时读取配置（支持 web 修改密码后立即生效）
	if username != srv.deps.Cfg.WebUsername || req.Password != srv.deps.Cfg.WebPassword {
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	token := srv.sessions.create(username)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(srv.sessions.ttl),
	})

	srv.deps.Logger.Info("WEB-AUTH", "用户 %s 登录成功", username)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"username": username,
		"ttl":      srv.sessions.ttl.String(),
	})
}

// logoutHandler POST /api/logout
func (srv *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		srv.sessions.destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// authMiddleware 校验会话，未登录返回 401
func (srv *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "未登录")
			return
		}
		if _, ok := srv.sessions.validate(c.Value); !ok {
			writeError(w, http.StatusUnauthorized, "会话已过期，请重新登录")
			return
		}
		next(w, r)
	}
}

// authStatusHandler GET /api/auth/status 检查登录状态
func (srv *Server) authStatusHandler(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	loggedIn := false
	username := ""
	if err == nil {
		if u, ok := srv.sessions.validate(c.Value); ok {
			loggedIn = true
			username = u
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"logged_in": loggedIn,
		"username":  username,
	})
}
