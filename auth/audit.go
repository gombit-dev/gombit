package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gombit-dev/gombit/contract"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// The security audit trail (AUTH-7, #337): one row per security-sensitive
// operation, in auth_audit_events. A row says who acted (the actor), what
// they did (the action), to what (the target), when, and which request did
// it (request id, client IP, user agent). It never holds a password, a
// token, or a hash of either: Details carries only names and ids.

// AuditAction names a security-sensitive operation.
type AuditAction string

const (
	// AuditLoginSucceeded: a user signed in (the target).
	AuditLoginSucceeded AuditAction = "auth.login.succeeded"
	// AuditLoginFailed: a sign-in failed. The target is the account when
	// the email names one (a wrong password), and empty when it names none:
	// the attempted address is not recorded.
	AuditLoginFailed AuditAction = "auth.login.failed"
	// AuditLogout: a session was ended by logout.
	AuditLogout AuditAction = "auth.logout"
	// AuditRegistered: a user registered.
	AuditRegistered AuditAction = "auth.user.registered"
	// AuditSuperuserCreated: a superuser was created (gombit
	// createsuperuser).
	AuditSuperuserCreated AuditAction = "auth.superuser.created"
	// AuditRefreshReuse: an already-rotated refresh token was presented
	// again, and every session of the user was revoked.
	AuditRefreshReuse AuditAction = "auth.refresh.reuse_detected"
	// AuditSessionRevoked: sessions were revoked (Details "scope": one,
	// others or all).
	AuditSessionRevoked AuditAction = "auth.session.revoked"
	// AuditEmailVerified: a user verified their email (POST
	// /auth/verify-email).
	AuditEmailVerified AuditAction = "auth.email.verified"
	// AuditGroupMemberAdded: a user was added to a group (Details "group").
	AuditGroupMemberAdded AuditAction = "auth.group.member_added"
	// AuditPermissionGranted: a permission was granted to a user or a group
	// (Details "permission", and "group" for a group).
	AuditPermissionGranted AuditAction = "auth.permission.granted"
	// AuditAdminCreated, AuditAdminUpdated and AuditAdminDeleted: a record
	// was written through the admin data plane (target: the model's slug and
	// the record's primary key).
	AuditAdminCreated AuditAction = "admin.record.created"
	AuditAdminUpdated AuditAction = "admin.record.updated"
	AuditAdminDeleted AuditAction = "admin.record.deleted"
)

// AuditEvent is one entry of the security audit trail.
type AuditEvent struct {
	ID uint `gorm:"primaryKey"`
	// OccurredAt is when the operation ran.
	OccurredAt time.Time `gorm:"not null;index"`
	// Action is what was done.
	Action AuditAction `gorm:"size:64;not null;index"`
	// ActorUserID is the user who did it: the request's authenticated user,
	// or the user signing in. Nil for an anonymous request or a command.
	ActorUserID *uint `gorm:"index"`
	// TargetUserID is the account the operation concerns, when it concerns
	// one.
	TargetUserID *uint `gorm:"index"`
	// TargetType and TargetID name any other target: an admin model's slug
	// and a record's primary key.
	TargetType string `gorm:"size:64"`
	TargetID   string `gorm:"size:128"`
	// RequestID, IP and UserAgent correlate it with the request, where there
	// was one: the X-Request-Id the logs carry, the client address (trusted
	// proxies respected), and the User-Agent, cut to 255 bytes.
	RequestID string `gorm:"size:64;index"`
	IP        string `gorm:"size:64"`
	UserAgent string `gorm:"size:255"`
	// Details is a JSON object of names and ids (never a secret).
	Details string `gorm:"type:text"`
}

// TableName keeps the table in the auth_ namespace.
func (AuditEvent) TableName() string { return "auth_audit_events" }

// Auditor records audit events. The default writes them to
// auth_audit_events in the service's database; an application may send
// them elsewhere as well (a SIEM) with WithAuditor. Record is called on the
// request, after the operation succeeded or failed; an error is logged, and
// does not fail the operation.
type Auditor interface {
	Record(ctx context.Context, event AuditEvent) error
}

// WithAuditor replaces the Service's Auditor.
func WithAuditor(a Auditor) Option {
	return func(s *Service) {
		if a != nil {
			s.auditor = a
		}
	}
}

// DBAuditor writes audit events to auth_audit_events in db.
func DBAuditor(db *gorm.DB) Auditor { return dbAuditor{db: db} }

type dbAuditor struct{ db *gorm.DB }

func (a dbAuditor) Record(ctx context.Context, event AuditEvent) error {
	if a.db == nil {
		return errors.New("auth: nil audit database")
	}
	// The operation's own context may be cancelled once it answered; the
	// record of it must still be written.
	db := a.db.WithContext(context.WithoutCancel(ctx))
	if committer, ok := db.Statement.ConnPool.(gorm.TxCommitter); ok && committer != nil {
		// Inside the caller's transaction (a grant helper given a tx): a
		// savepoint, so a failed insert (the table not migrated yet) does
		// not abort the caller's transaction on PostgreSQL.
		return db.Transaction(func(tx *gorm.DB) error { return tx.Create(&event).Error })
	}
	return db.Create(&event).Error
}

// requestInfo is what the audit trail correlates an event with.
type requestInfo struct {
	ip        string
	userAgent string
}

type requestInfoKey struct{}

// CaptureRequestInfo is a Huma middleware that stores the request's client
// address (gin's ClientIP, so GOMBIT_HTTP_TRUSTED_PROXIES is respected) and
// User-Agent for the audit events the operation records. Mount puts it on
// every framework auth route; an application puts it on its own operations
// that record events (RecordAuditEvent).
func CaptureRequestInfo() func(ctx huma.Context, next func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		info := requestInfo{userAgent: ctx.Header("User-Agent"), ip: clientIP(ctx)}
		next(huma.WithValue(ctx, requestInfoKey{}, info))
	}
}

// withRequestInfo puts CaptureRequestInfo ahead of op's middlewares.
func withRequestInfo(op huma.Operation) huma.Operation {
	op.Middlewares = append(huma.Middlewares{CaptureRequestInfo()}, op.Middlewares...)
	return op
}

// clientIP is gin's ClientIP for a Gin-backed API, and the remote address
// otherwise.
func clientIP(ctx huma.Context) (ip string) {
	defer func() {
		if recover() != nil { // not a humagin context
			ip = remoteHost(ctx.RemoteAddr())
		}
	}()
	return humagin.Unwrap(ctx).ClientIP()
}

func remoteHost(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 && !strings.HasSuffix(addr, "]") {
		addr = addr[:i]
	}
	return strings.Trim(addr, "[]")
}

// RecordAuditEvent writes event to auth_audit_events in db, completed from
// ctx: OccurredAt (now, when zero), the actor (the request's authenticated
// user, when ActorUserID is nil), and the request id, client IP and user
// agent (CaptureRequestInfo). It is the trail's entry point for code that
// has a database and no Service: the group and permission helpers, an
// application's own sensitive operations. It writes to the table whatever
// Auditor a Service has; Service.Audit goes through that Auditor.
func RecordAuditEvent(ctx context.Context, db *gorm.DB, event AuditEvent) error {
	return DBAuditor(db).Record(ctx, completeAuditEvent(ctx, event, time.Now()))
}

func completeAuditEvent(ctx context.Context, event AuditEvent, now time.Time) AuditEvent {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = now
	}
	if event.ActorUserID == nil {
		if user, ok := UserFromContext(ctx); ok && user.ID != 0 {
			id := user.ID
			event.ActorUserID = &id
		}
	}
	if event.RequestID == "" {
		event.RequestID = contract.RequestIDFromContext(ctx)
	}
	if info, ok := ctx.Value(requestInfoKey{}).(requestInfo); ok {
		if event.IP == "" {
			event.IP = info.ip
		}
		if event.UserAgent == "" {
			event.UserAgent = info.userAgent
		}
	}
	event.Action = AuditAction(truncate(string(event.Action), 64))
	event.TargetType = truncate(event.TargetType, 64)
	event.TargetID = truncate(event.TargetID, 128)
	event.UserAgent = truncate(event.UserAgent, 255)
	event.IP = truncate(event.IP, 64)
	event.RequestID = truncate(event.RequestID, 64)
	if event.Details == "" {
		event.Details = "{}"
	}
	return event
}

// truncate cuts s to at most n bytes, on a character boundary: PostgreSQL
// rejects a string that ends inside a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// details encodes names and ids for AuditEvent.Details.
func details(kv ...string) string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func uintPtr(v uint) *uint {
	if v == 0 {
		return nil
	}
	return &v
}

// Audit records event through the Service's Auditor, completed from ctx as
// RecordAuditEvent completes it. A failure is logged, not returned: the
// operation it records has happened.
func (s *Service) Audit(ctx context.Context, event AuditEvent) {
	event = completeAuditEvent(ctx, event, s.now())
	if err := s.auditor.Record(ctx, event); err != nil {
		s.logger.Warn("auth: record an audit event", zap.String("action", string(event.Action)), zap.Error(err))
	}
}

// AuditQuery selects audit events (ListAuditEvents). Zero fields match all.
type AuditQuery struct {
	ActorUserID  uint
	TargetUserID uint
	Action       AuditAction
	// Limit caps the events returned, newest first: 100 when zero, at most
	// 1000.
	Limit int
}

// ListAuditEvents returns the audit events q selects, newest first.
func ListAuditEvents(ctx context.Context, db *gorm.DB, q AuditQuery) ([]AuditEvent, error) {
	if db == nil {
		return nil, errors.New("auth: nil database")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	tx := db.WithContext(ctx).Model(&AuditEvent{})
	if q.ActorUserID != 0 {
		tx = tx.Where("actor_user_id = ?", q.ActorUserID)
	}
	if q.TargetUserID != 0 {
		tx = tx.Where("target_user_id = ?", q.TargetUserID)
	}
	if q.Action != "" {
		tx = tx.Where("action = ?", q.Action)
	}
	var events []AuditEvent
	// By id: rows are inserted in order, and SQLite compares times as text.
	err := tx.Order("id DESC").Limit(limit).Find(&events).Error
	return events, err
}
