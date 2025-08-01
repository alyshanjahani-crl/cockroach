// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package pgwire

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/security/provisioning"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/hba"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/identmap"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgwirebase"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/cockroach/pkg/util/log/logpb"
	"github.com/cockroachdb/cockroach/pkg/util/log/severity"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

const (
	// authOK is the pgwire auth response code for successful authentication
	// during the connection handshake.
	authOK int32 = 0
	// authCleartextPassword is the pgwire auth response code to request
	// a plaintext password during the connection handshake.
	authCleartextPassword int32 = 3

	// authReqSASL is the begin request for a SCRAM handshake.
	authReqSASL int32 = 10
	// authReqSASLContinue is the continue request for a SCRAM handshake.
	authReqSASLContinue int32 = 11
	// authReqSASLFin is the final message for a SCRAM handshake.
	authReqSASLFin int32 = 12
)

type authOptions struct {
	// insecure indicates that all connections for existing users must
	// be allowed to go through. A password, if presented, must be
	// accepted.
	insecure bool
	// connType is the actual type of client connection (e.g. local,
	// hostssl, hostnossl).
	connType hba.ConnType
	// connDetails is the event payload common to all auth/session events.
	connDetails eventpb.CommonConnectionDetails

	// auth is the current HBA configuration as returned by
	// (*Server).GetAuthenticationConfiguration().
	auth *hba.Conf
	// identMap is used in conjunction with the HBA configuration to
	// allow system usernames (e.g. GSSAPI principals or X.509 CN's) to
	// be dynamically mapped to database usernames.
	identMap *identmap.Conf

	// The following fields are only used by tests.

	// testingSkipAuth requires to skip authentication, not even
	// allowing a password exchange.
	// Note that this different from insecure auth: with no auth, no
	// password is accepted (a protocol error is given if one is
	// presented); with insecure auth; _any_ is accepted.
	testingSkipAuth bool
	// testingAuthHook, if provided, replaces the logic in
	// handleAuthentication().
	testingAuthHook func(ctx context.Context) error
}

// handleAuthentication checks the connection's user. Errors are sent to the
// client and also returned.
//
// TODO(knz): handleAuthentication should discuss with the client to arrange
// authentication and update c.sessionArgs with the authenticated user's name,
// if different from the one given initially.
func (c *conn) handleAuthentication(
	ctx context.Context, ac AuthConn, authOpt authOptions, server *Server,
) (connClose func(), _ error) {
	if authOpt.testingSkipAuth {
		return nil, nil
	}
	if authOpt.testingAuthHook != nil {
		return nil, authOpt.testingAuthHook(ctx)
	}
	// Get execCfg from the server.
	execCfg := server.execCfg

	// To book-keep the authentication start time.
	authStartTime := timeutil.Now()

	// Retrieve the authentication method.
	tlsState, hbaEntry, authMethod, err := c.findAuthenticationMethod(authOpt)
	if err != nil {
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_METHOD_NOT_FOUND, err)
		return nil, c.sendError(ctx, pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification))
	}

	ac.SetAuthMethod(redact.SafeString(hbaEntry.Method.String()))
	ac.LogAuthInfof(ctx, redact.Sprintf("HBA rule: %s", hbaEntry.Input))

	// Populate the AuthMethod with per-connection information so that it
	// can compose the next layer of behaviors that we're going to apply
	// to the incoming connection.
	behaviors, err := authMethod(ctx, ac, c.sessionArgs.User, tlsState, execCfg, hbaEntry, authOpt.identMap)
	connClose = behaviors.ConnClose
	if err != nil {
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_UNKNOWN, err)
		return connClose, c.sendError(ctx, pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification))
	}

	// Choose the system identity that we'll use below for mapping
	// externally-provisioned principals to database users. The system identity
	// always is normalized to lower case.
	var systemIdentity string
	if found, ok := behaviors.ReplacementIdentity(); ok {
		systemIdentity = lexbase.NormalizeName(found)
		ac.SetSystemIdentity(systemIdentity)
	} else {
		if c.sessionArgs.SystemIdentity != "" {
			// This case is used in tests, which pass a system_identity
			// option directly.
			systemIdentity = lexbase.NormalizeName(c.sessionArgs.SystemIdentity)
		} else {
			systemIdentity = c.sessionArgs.User.Normalized()
		}
	}
	c.sessionArgs.SystemIdentity = systemIdentity

	// Delegate to the AuthMethod's MapRole to verify that the
	// client-provided username matches one of the mappings.
	if err := c.checkClientUsernameMatchesMapping(ctx, ac, behaviors.MapRole, systemIdentity); err != nil {
		log.Warningf(ctx, "unable to map incoming identity %q to any database user: %+v", systemIdentity, err)
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_USER_NOT_FOUND, err)
		return connClose, c.sendError(ctx, pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification))
	}

	// Once chooseDbRole() returns, we know that the actual DB username
	// will be present in c.sessionArgs.User.
	dbUser := c.sessionArgs.User

	// Check that the requested user exists and retrieve the hashed
	// password in case password authentication is needed.
	exists, canLoginSQL, _, canUseReplicationMode, isSuperuser, defaultSettings, roleSubject, provisioningSource, pwRetrievalFn, err :=
		sql.GetUserSessionInitInfo(
			ctx,
			execCfg,
			dbUser,
			c.sessionArgs.SessionDefaults["database"],
		)
	if err != nil {
		log.Warningf(ctx, "user retrieval failed for user=%q: %+v", dbUser, err)
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_USER_RETRIEVAL_ERROR, err)
		return connClose, c.sendError(ctx, pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification))
	}

	if !exists {
		if execCfg.Settings.Version.IsActive(ctx, clusterversion.V25_3) &&
			behaviors.IsProvisioningEnabled(execCfg.Settings, hbaEntry.Method.String()) {
			err := behaviors.MaybeProvisionUser(ctx, execCfg.Settings, hbaEntry.Method.String())
			if err != nil {
				log.Warningf(ctx, "user provisioning failed for user=%q: %+v", dbUser, err)
				ac.LogAuthFailed(ctx, eventpb.AuthFailReason_PROVISIONING_ERROR, err)
				return connClose, c.sendError(ctx, pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification))
			}
			exists, canLoginSQL, _, canUseReplicationMode, isSuperuser, defaultSettings, roleSubject, provisioningSource, pwRetrievalFn, err =
				sql.GetUserSessionInitInfo(
					ctx,
					execCfg,
					dbUser,
					c.sessionArgs.SessionDefaults["database"],
				)
			if err != nil {
				log.Warningf(ctx, "user retrieval failed for user=%q: %+v", dbUser, err)
				ac.LogAuthFailed(ctx, eventpb.AuthFailReason_USER_RETRIEVAL_ERROR, err)
				return connClose, c.sendError(ctx, pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification))
			}
		}
	}

	c.sessionArgs.IsSuperuser = isSuperuser

	if !exists {
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_USER_NOT_FOUND, nil)
		// If the user does not exist, we show the same error used for invalid
		// passwords, to make it harder for an attacker to determine if a user
		// exists.
		return connClose, c.sendError(ctx, pgerror.WithCandidateCode(security.NewErrPasswordUserAuthFailed(dbUser), pgcode.InvalidPassword))
	}

	if !canLoginSQL {
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_LOGIN_DISABLED, nil)
		return connClose, c.sendError(ctx, pgerror.Newf(pgcode.InvalidAuthorizationSpecification, "%s does not have login privilege", dbUser))
	}

	// At this point, we know that the requested user exists and is allowed to log
	// in. Now we can delegate to the selected AuthMethod implementation to
	// complete the authentication. We must pass in the systemIdentity here,
	// since the authenticator may use an external source to verify the
	// user and its credentials.
	if err := behaviors.Authenticate(ctx, systemIdentity, true /* public */, pwRetrievalFn, roleSubject); err != nil {
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_UNKNOWN, err)
		if pErr := (*security.PasswordUserAuthError)(nil); errors.As(err, &pErr) {
			err = pgerror.WithCandidateCode(err, pgcode.InvalidPassword)
		} else {
			err = pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification)
		}
		return connClose, c.sendError(ctx, err)
	}

	// Since authentication was successful for the user session, we try to perform
	// additional authorization for the user. This delegates to the selected
	// AuthMethod implementation to complete the authorization. Only certain
	// methods, like LDAP, will actually perform authorization here.
	if err := behaviors.MaybeAuthorize(ctx, systemIdentity, true /* public */); err != nil {
		ac.LogAuthFailed(ctx, eventpb.AuthFailReason_UNKNOWN, err)
		err = pgerror.WithCandidateCode(err, pgcode.InvalidAuthorizationSpecification)
		return connClose, c.sendError(ctx, err)
	}

	// Add all the defaults to this session's defaults. If there is an
	// error (e.g., a setting that no longer exists, or bad input),
	// log a warning instead of preventing login.
	// The defaultSettings array is ordered by precedence. This means that if
	// SessionDefaults already has an entry for a given setting name, then
	// it should not be replaced.
	for _, settingEntry := range defaultSettings {
		for _, setting := range settingEntry.Settings {
			keyVal := strings.SplitN(setting, "=", 2)
			if len(keyVal) != 2 {
				log.Ops.Warningf(ctx, "%s has malformed default setting: %q", dbUser, setting)
				continue
			}
			if err := sql.CheckSessionVariableValueValid(ctx, execCfg.Settings, keyVal[0], keyVal[1]); err != nil {
				log.Ops.Warningf(ctx, "%s has invalid default setting: %v", dbUser, err)
				continue
			}
			if _, ok := c.sessionArgs.SessionDefaults[keyVal[0]]; !ok {
				c.sessionArgs.SessionDefaults[keyVal[0]] = keyVal[1]
			}
		}
	}

	// Check replication privilege.
	if c.sessionArgs.SessionDefaults["replication"] != "" {
		m, err := sql.ReplicationModeFromString(c.sessionArgs.SessionDefaults["replication"])
		if err == nil && m != sessiondatapb.ReplicationMode_REPLICATION_MODE_DISABLED && !canUseReplicationMode {
			ac.LogAuthFailed(ctx, eventpb.AuthFailReason_NO_REPLICATION_ROLEOPTION, nil)
			return connClose, c.sendError(
				ctx,
				pgerror.Newf(
					pgcode.InsufficientPrivilege,
					"must be superuser or have REPLICATION role to start streaming replication",
				),
			)
		}
	}

	// If user has PROVISIONSRC set, increment the login success counter
	if provisioningSource != nil {
		telemetry.Inc(provisioning.ProvisionedUserLoginSuccessCounter)
	}

	// Compute the authentication latency needed to serve a SQL query.
	// The metric published is based on the authentication type.
	duration := timeutil.Since(authStartTime).Nanoseconds()
	c.publishConnLatencyMetric(duration, hbaEntry.Method.String())

	server.lastLoginUpdater.updateLastLoginTime(ctx, dbUser)

	return connClose, nil
}

// publishConnLatencyMetric publishes the latency  of the connection
// based on the authentication method.
func (c *conn) publishConnLatencyMetric(duration int64, authMethod string) {
	switch authMethod {
	case jwtHBAEntry.string():
		c.metrics.AuthJWTConnLatency.RecordValue(duration)
	case certHBAEntry.string():
		c.metrics.AuthCertConnLatency.RecordValue(duration)
	case passwordHBAEntry.string():
		c.metrics.AuthPassConnLatency.RecordValue(duration)
	case ldapHBAEntry.string():
		c.metrics.AuthLDAPConnLatency.RecordValue(duration)
	case gssHBAEntry.string():
		c.metrics.AuthGSSConnLatency.RecordValue(duration)
	case scramSHA256HBAEntry.string():
		c.metrics.AuthScramConnLatency.RecordValue(duration)
	}
}

func (c *conn) authOKMessage() error {
	c.msgBuilder.initMsg(pgwirebase.ServerMsgAuth)
	c.msgBuilder.putInt32(authOK)
	return c.msgBuilder.finishMsg(c.conn)
}

// checkClientUsernameMatchesMapping uses the provided RoleMapper to
// verify that the client-provided username matches one of the
// mappings for the system identity.
// See: https://www.postgresql.org/docs/15/auth-username-maps.html
// "There is no restriction regarding how many database users a given
// operating system user can correspond to, nor vice versa. Thus,
// entries in a map should be thought of as meaning “this operating
// system user is allowed to connect as this database user”, rather
// than implying that they are equivalent. The connection will be
// allowed if there is any map entry that pairs the user name obtained
// from the external authentication system with the database user name
// that the user has requested to connect as."
func (c *conn) checkClientUsernameMatchesMapping(
	ctx context.Context, ac AuthConn, mapper RoleMapper, systemIdentity string,
) error {
	mapped, err := mapper(ctx, systemIdentity)
	if err != nil {
		return err
	}
	if len(mapped) == 0 {
		return errors.Newf("system identity %q did not map to a database role", systemIdentity)
	}
	for _, m := range mapped {
		if m == c.sessionArgs.User {
			ac.SetDbUser(m)
			return nil
		}
	}
	return errors.Newf("requested user identity %q does not correspond to any mapping for system identity %q",
		c.sessionArgs.User, systemIdentity)
}

func (c *conn) findAuthenticationMethod(
	authOpt authOptions,
) (tlsState tls.ConnectionState, hbaEntry *hba.Entry, methodFn AuthMethod, err error) {
	if authOpt.insecure {
		// Insecure connections always use "trust" no matter what, and the
		// remaining of the configuration is ignored.
		methodFn = authTrust
		hbaEntry = &insecureEntry
		return
	}
	if c.sessionArgs.SessionRevivalToken != nil {
		methodFn = authSessionRevivalToken(c.sessionArgs.SessionRevivalToken)
		c.sessionArgs.SessionRevivalToken = nil
		hbaEntry = &sessionRevivalEntry
		return
	}

	if c.sessionArgs.JWTAuthEnabled {
		methodFn = authJwtToken
		hbaEntry = &jwtAuthEntry
		return
	}

	// Look up the method from the HBA configuration.
	var mi methodInfo
	mi, hbaEntry, err = c.lookupAuthenticationMethodUsingRules(authOpt.connType, authOpt.auth)
	if err != nil {
		return
	}
	methodFn = mi.fn

	// Check that this method can be used over this connection type.
	if authOpt.connType&mi.validConnTypes == 0 {
		err = errors.Newf("method %q required for this user, but unusable over this connection type",
			hbaEntry.Method.Value)
		return
	}

	// If the client is using SSL, retrieve the TLS state to provide as
	// input to the method.
	if authOpt.connType == hba.ConnHostSSL {
		tlsConn, ok := c.conn.(*tls.Conn)
		if !ok {
			err = errors.AssertionFailedf("server reports hostssl conn without TLS state")
			return
		}
		tlsState = tlsConn.ConnectionState()
	}

	return
}

func (c *conn) lookupAuthenticationMethodUsingRules(
	connType hba.ConnType, auth *hba.Conf,
) (mi methodInfo, entry *hba.Entry, err error) {
	var ip net.IP
	if connType != hba.ConnLocal && connType != hba.ConnInternalLoopback {
		// Extract the IP address of the client.
		tcpAddr, ok := c.sessionArgs.RemoteAddr.(*net.TCPAddr)
		if !ok {
			err = errors.AssertionFailedf("client address type %T unsupported", c.sessionArgs.RemoteAddr)
			return
		}
		ip = tcpAddr.IP
	}

	// Look up the method.
	for i := range auth.Entries {
		entry = &auth.Entries[i]
		var connMatch bool
		connMatch, err = entry.ConnMatches(connType, ip)
		if err != nil {
			// TODO(knz): Determine if an error should be reported
			// upon unknown address formats.
			// See: https://github.com/cockroachdb/cockroach/issues/43716
			return
		}
		if !connMatch {
			// The address does not match.
			continue
		}
		if !entry.UserMatches(c.sessionArgs.User) {
			// The user does not match.
			continue
		}
		return entry.MethodFn.(methodInfo), entry, nil
	}

	// No match.
	err = errors.Errorf("no %s entry for host %q, user %q", serverHBAConfSetting, ip, c.sessionArgs.User)
	return
}

// authenticatorIO is the interface used by the connection to pass password data
// to the authenticator and expect an authentication decision from it.
type authenticatorIO interface {
	// sendPwdData is used to push authentication data into the authenticator.
	// This call is blocking; authenticators are supposed to consume data hastily
	// once they've requested it.
	sendPwdData(data []byte) error
	// noMorePwdData is used to inform the authenticator that the client is not
	// sending any more authentication data. This method can be called multiple
	// times.
	noMorePwdData()
	// authResult blocks for an authentication decision. This call also informs
	// the authenticator that no more auth data is coming from the client;
	// noMorePwdData() is called internally.
	authResult() error
}

// AuthConn is the interface used by the authenticator for interacting with the
// pgwire connection.
type AuthConn interface {
	// SendAuthRequest send a request for authentication information. After
	// calling this, the authenticator needs to call GetPwdData() quickly, as the
	// connection's goroutine will be blocked on providing us the requested data.
	SendAuthRequest(authType int32, data []byte) error
	// GetPwdData returns authentication info that was previously requested with
	// SendAuthRequest. The call blocks until such data is available.
	// An error is returned if the client connection dropped or if the client
	// didn't respect the protocol. After an error has been returned, GetPwdData()
	// cannot be called any more.
	GetPwdData() ([]byte, error)
	// AuthOK declares that authentication succeeded and provides a
	// unqualifiedIntSizer, to be returned by authenticator.authResult(). Future
	// authenticator.sendPwdData() calls fail.
	AuthOK(context.Context)
	// AuthFail declares that authentication has failed and provides an error to
	// be returned by authenticator.authResult(). Future
	// authenticator.sendPwdData() calls fail. The error has already been written
	// to the client connection.
	AuthFail(err error)

	// SetAuthMethod sets the authentication method for subsequent
	// logging messages.
	SetAuthMethod(method redact.SafeString)
	// SetDbUser updates the AuthConn with the actual database username
	// the connection has authenticated to.
	SetDbUser(dbUser username.SQLUsername)
	// SetSystemIdentity updates the AuthConn with an externally-defined
	// identity for the connection. This is useful for "ambient"
	// authentication mechanisms, such as GSSAPI.
	SetSystemIdentity(systemIdentity string)
	// LogAuthInfof logs details about the progress of the
	// authentication.
	LogAuthInfof(ctx context.Context, msg redact.RedactableString)
	// LogAuthFailed logs details about an authentication failure.
	LogAuthFailed(ctx context.Context, reason eventpb.AuthFailReason, err error)
	// LogAuthOK logs when the authentication handshake has completed.
	LogAuthOK(ctx context.Context)
	// LogSessionEnd logs when the session is ended.
	LogSessionEnd(ctx context.Context, endTime time.Time)
	// GetTenantSpecificMetrics returns the tenant-specific metrics for the connection.
	GetTenantSpecificMetrics() *tenantSpecificMetrics
}

// authPipe is the implementation for the authenticator and AuthConn interfaces.
// A single authPipe will serve as both an AuthConn and an authenticator; the
// two represent the two "ends" of the pipe and we'll pass data between them.
type authPipe struct {
	c             *conn // Only used for writing, not for reading.
	log           bool
	loggedFailure bool

	connDetails eventpb.CommonConnectionDetails
	authDetails eventpb.CommonSessionDetails
	authMethod  redact.SafeString

	ch chan []byte

	// closeWriterDoneOnce wraps close(writerDone) to prevent a panic if
	// noMorePwdData is called multiple times.
	closeWriterDoneOnce sync.Once
	// writerDone is a channel closed by noMorePwdData().
	writerDone chan struct{}
	readerDone chan authRes
}

type authRes struct {
	err error
}

func newAuthPipe(c *conn, logAuthn bool, authOpt authOptions, systemIdentity string) *authPipe {
	ap := &authPipe{
		c:           c,
		log:         logAuthn,
		connDetails: authOpt.connDetails,
		authDetails: eventpb.CommonSessionDetails{
			SystemIdentity: systemIdentity,
			Transport:      authOpt.connType.String(),
		},
		ch:         make(chan []byte),
		writerDone: make(chan struct{}),
		readerDone: make(chan authRes, 1),
	}
	return ap
}

var _ authenticatorIO = &authPipe{}
var _ AuthConn = &authPipe{}

func (p *authPipe) sendPwdData(data []byte) error {
	select {
	case p.ch <- data:
		return nil
	case <-p.readerDone:
		return pgwirebase.NewProtocolViolationErrorf("unexpected auth data")
	}
}

func (p *authPipe) noMorePwdData() {
	p.closeWriterDoneOnce.Do(func() {
		// A reader blocked in GetPwdData() gets unblocked with an error.
		close(p.writerDone)
	})
}

const writerDoneError = "client didn't send required auth data"

// GetPwdData is part of the AuthConn interface.
func (p *authPipe) GetPwdData() ([]byte, error) {
	select {
	case data := <-p.ch:
		return data, nil
	case <-p.writerDone:
		return nil, pgwirebase.NewProtocolViolationErrorf(writerDoneError)
	}
}

// AuthOK is part of the AuthConn interface.
func (p *authPipe) AuthOK(ctx context.Context) {
	p.readerDone <- authRes{err: nil}
}

func (p *authPipe) AuthFail(err error) {
	p.readerDone <- authRes{err: err}
}

func (p *authPipe) SetAuthMethod(method redact.SafeString) {
	p.authMethod = method
	p.c.sessionArgs.AuthenticationMethod = method
}

func (p *authPipe) SetDbUser(dbUser username.SQLUsername) {
	p.authDetails.User = dbUser.Normalized()
}

func (p *authPipe) SetSystemIdentity(systemIdentity string) {
	p.authDetails.SystemIdentity = systemIdentity
}

func (p *authPipe) LogAuthOK(ctx context.Context) {
	// Logged unconditionally.
	ev := &eventpb.ClientAuthenticationOk{
		CommonConnectionDetails: p.connDetails,
		CommonSessionDetails:    p.authDetails,
		Method:                  p.authMethod,
	}
	log.StructuredEvent(ctx, severity.INFO, ev)
}

func (p *authPipe) LogAuthInfof(ctx context.Context, msg redact.RedactableString) {
	if p.log {
		ev := &eventpb.ClientAuthenticationInfo{
			CommonConnectionDetails: p.connDetails,
			CommonSessionDetails:    p.authDetails,
			Info:                    msg,
			Method:                  p.authMethod,
		}
		log.StructuredEvent(ctx, severity.INFO, ev)
	}
}

func (p *authPipe) LogSessionEnd(ctx context.Context, endTime time.Time) {
	// Logged unconditionally.
	ev := &eventpb.ClientSessionEnd{
		CommonEventDetails:      logpb.CommonEventDetails{Timestamp: endTime.UnixNano()},
		CommonConnectionDetails: p.connDetails,
		Duration:                endTime.Sub(p.c.startTime).Nanoseconds(),
	}
	log.StructuredEvent(ctx, severity.INFO, ev)
}

func (p *authPipe) LogAuthFailed(
	ctx context.Context, reason eventpb.AuthFailReason, detailedErr error,
) {
	if p.log && !p.loggedFailure {
		// If a failure was already logged, then don't log another one. The
		// assumption is that if an error is logged deeper in the call stack, the
		// reason is likely to be more specific than at a higher point in the stack.
		p.loggedFailure = true
		var errStr redact.RedactableString
		if detailedErr != nil {
			errStr = redact.Sprint(detailedErr)
		}
		ev := &eventpb.ClientAuthenticationFailed{
			CommonConnectionDetails: p.connDetails,
			CommonSessionDetails:    p.authDetails,
			Reason:                  reason,
			Detail:                  errStr,
			Method:                  p.authMethod,
		}
		log.StructuredEvent(ctx, severity.INFO, ev)
	}
}

// authResult is part of the authenticator interface.
func (p *authPipe) authResult() error {
	p.noMorePwdData()
	res := <-p.readerDone
	return res.err
}

// SendAuthRequest is part of the AuthConn interface.
func (p *authPipe) SendAuthRequest(authType int32, data []byte) error {
	c := p.c
	c.msgBuilder.initMsg(pgwirebase.ServerMsgAuth)
	c.msgBuilder.putInt32(authType)
	c.msgBuilder.write(data)
	return c.msgBuilder.finishMsg(c.conn)
}

// GetTenantSpecificMetrics is part of the AuthConn interface.
func (p *authPipe) GetTenantSpecificMetrics() *tenantSpecificMetrics {
	return p.c.metrics
}
