package server

import (
	"net/http"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func (d *Deps) authorizeSession(w http.ResponseWriter, r *http.Request, sid string) (*storage.Session, bool) {
	if sid == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SESSION_ID", "session id required")
		return nil, false
	}
	sess, err := d.Stores.Sessions.Get(r.Context(), sid)
	if err != nil {
		writeError(w, statusForStorageErr(err), "SESSION_GET_FAILED", err.Error())
		return nil, false
	}
	if !sessionAllowed(r, sess) {
		writeError(w, http.StatusForbidden, "SESSION_FORBIDDEN", "session does not belong to current user")
		return nil, false
	}
	return sess, true
}

func (d *Deps) authorizeRun(w http.ResponseWriter, r *http.Request, rid string) (*storage.Run, bool) {
	return d.authorizeRunInSession(w, r, "", rid)
}

func (d *Deps) authorizeRunInSession(w http.ResponseWriter, r *http.Request, sid, rid string) (*storage.Run, bool) {
	if rid == "" {
		writeError(w, http.StatusBadRequest, "MISSING_RUN_ID", "run id required")
		return nil, false
	}
	run, err := d.Stores.Runs.Get(r.Context(), rid)
	if err != nil {
		writeError(w, statusForStorageErr(err), "RUN_GET_FAILED", err.Error())
		return nil, false
	}
	if sid != "" && run.SessionID != sid {
		writeError(w, http.StatusNotFound, "RUN_SESSION_MISMATCH", "run not found in session")
		return nil, false
	}
	sess, ok := d.authorizeSession(w, r, run.SessionID)
	if !ok {
		return nil, false
	}
	if run.TenantID != "" && sess.TenantID != "" && run.TenantID != sess.TenantID {
		writeError(w, http.StatusForbidden, "RUN_SESSION_TENANT_MISMATCH", "run and session tenant mismatch")
		return nil, false
	}
	return run, true
}

// authorizeSessionForOpen guards the write path that opens a turn/run on a
// session (POST /sessions/{sid}/runs and POST /ai/chat). Unlike authorizeSession
// (read path), a not-found session is allowed: it will be created with the
// caller as owner. But an EXISTING session must belong to the caller — this is
// what blocks a same-tenant user from injecting a Message/Run into another
// user's session via a known session_id. An empty sid means the server will
// mint a fresh session, so it is always allowed.
func (d *Deps) authorizeSessionForOpen(w http.ResponseWriter, r *http.Request, sid string) bool {
	if sid == "" {
		return true // server-generated session; caller becomes owner
	}
	sess, err := d.Stores.Sessions.Get(r.Context(), sid)
	if err != nil {
		if storage.IsErrorCode(err, storage.ErrNotFound) {
			return true // new session; caller becomes owner on create
		}
		writeError(w, statusForStorageErr(err), "SESSION_GET_FAILED", err.Error())
		return false
	}
	tc := observability.MustTraceContext(r.Context())
	if sess.TenantID != "" && tc.TenantID != "" && sess.TenantID != tc.TenantID {
		writeError(w, http.StatusForbidden, "SESSION_TENANT_MISMATCH", "session belongs to another tenant")
		return false
	}
	if !sessionAllowed(r, sess) {
		writeError(w, http.StatusForbidden, "SESSION_FORBIDDEN", "session does not belong to current user")
		return false
	}
	return true
}

// sessionAllowed 只信服务端鉴权得到的可信身份(tc.UserID),绝不信任客户端头。
// 安全语义:
//   - 会话有属主(sess.UserID != "")时,调用方身份必须完全匹配;匿名/空身份一律拒绝,
//     堵住此前"空 UserID 直接放行"的越权旁路。
//   - 会话无属主时普通用户一律拒绝。遗留/系统数据必须通过显式迁移或特权策略访问,
//     不能把“缺少归属事实”解释成租户内公开。
func sessionAllowed(r *http.Request, sess *storage.Session) bool {
	tc := observability.MustTraceContext(r.Context())
	if sess.UserID == "" {
		// Anonymous ownerless fixtures are accepted only in an explicitly insecure
		// path. An authenticated caller never inherits ownerless data.
		return tc.UserID == ""
	}
	return tc.UserID == sess.UserID
}
