// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package pgwire_test

import (
	"context"
	gosql "database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/identmap"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/datapathutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/pgurlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/channel"
	"github.com/cockroachdb/cockroach/pkg/util/log/logconfig"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/stdstrings"
	"github.com/cockroachdb/redact"
	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// TestAuthenticationAndHBARules exercises the authentication code
// using datadriven testing.
//
// It supports the following DSL:
//
// config [secure] [insecure]
//
//	Only run the test file if the server is in the specified
//	security mode. (The default is `config secure insecure` i.e.
//	the test file is applicable to both.)
//
// accept_sql_without_tls
//
//	Enable TCP connections without TLS in secure mode.
//
// set_hba
// <hba config>
//
//	Load the provided HBA configuration via the cluster setting
//	server.host_based_authentication.configuration.
//	The expected output is the configuration after parsing
//	and reloading in the server.
//
// set_identity_map
// <identity map>
//
//	Load the provided identity map via the cluster setting
//	server.identity_map.configuration.
//	The expected output is the configuration after parsing
//	and reloading in the server.
//
// sql
// <sql input>
//
//	Execute the specified SQL statement using the default root
//	connection provided by StartServer().
//
// authlog N
// <regexp>
//
//	Expect <regexp> at the end of the auth log then report the
//	N entries before that.
//
// connect [key=value ...]
//
//	Attempt a SQL connection using the provided connection
//	parameters using the pg "DSN notation": k/v pairs separated
//	by spaces.
//	The following standard pg keys are recognized:
//	     user - the username
//	     password - the password
//	     host - the server name/address
//	     port - the server port
//	     force_certs - force the use of baked-in certificates
//	     sslmode, sslrootcert, sslcert, sslkey - SSL parameters.
//
//	The order of k/v pairs matters: if the same key is specified
//	multiple times, the first occurrence takes priority.
//
//	Additionally, the test runner will always _append_ a default
//	value for user (root), host/port/sslrootcert from the
//	initialized test server. This default configuration is placed
//	at the end so that each test can override the values.
//
//	The test runner also adds a default value for sslcert and
//	sslkey based on the value of "user" — either when provided by
//	the test, or root by default.
//
//	When the user is either "root" or "testuser" (those are the
//	users for which the test server generates certificates),
//	sslmode also gets a default of "verify-full". For other
//	users, sslmode is initialized by default to "verify-ca".
//
// For the directives "sql" and "connect", the expected output can be
// either "ok" (no error) or "ERROR:" followed by the expected error
// string.
// The auth and connection log entries, if any, are also produced
// alongside the "ok" or "ERROR" message.
func TestAuthenticationAndHBARules(t *testing.T) {
	defer leaktest.AfterTest(t)()
	skip.UnderRace(t, "takes >1min under race")

	testutils.RunTrueAndFalse(t, "insecure", func(t *testing.T, insecure bool) {
		defer leaktest.AfterTest(t)()
		hbaRunTest(t, insecure)
	})
}

const socketConnVirtualPort = "6"

func makeSocketFile(t *testing.T) (socketDir, socketFile string, cleanupFn func()) {
	if runtime.GOOS == "windows" {
		// Unix sockets not supported on windows.
		return "", "", func() {}
	}
	socketName := ".s.PGSQL." + socketConnVirtualPort
	// We need a temp directory in which we'll create the unix socket.
	//
	// On BSD, binding to a socket is limited to a path length of 104 characters
	// (including the NUL terminator). In glibc, this limit is 108 characters.
	//
	// macOS has a tendency to produce very long temporary directory names, so
	// we are careful to keep all the constants involved short.
	baseTmpDir := os.TempDir()
	if len(baseTmpDir) >= 104-1-len(socketName)-1-len("TestAuth")-10 {
		t.Logf("temp dir name too long: %s", baseTmpDir)
		t.Logf("using /tmp instead.")
		// Note: /tmp might fail in some systems, that's why we still prefer
		// os.TempDir() if available.
		baseTmpDir = "/tmp"
	}
	tempDir, err := os.MkdirTemp(baseTmpDir, "TestAuth")
	require.NoError(t, err)
	// ".s.PGSQL.NNNN" is the standard unix socket name supported by pg clients.
	return tempDir,
		filepath.Join(tempDir, socketName),
		func() { _ = os.RemoveAll(tempDir) }
}

func hbaRunTest(t *testing.T, insecure bool) {
	httpScheme := "http://"
	if !insecure {
		httpScheme = "https://"
	}
	ctx := context.Background()

	datadriven.Walk(t, datapathutils.TestDataPath(t, "auth"), func(t *testing.T, path string) {
		defer leaktest.AfterTest(t)()

		maybeSocketDir, maybeSocketFile, cleanup := makeSocketFile(t)
		defer cleanup()

		// We really need to have the logs go to files, so that -show-logs
		// does not break the "authlog" directives.
		sc := log.ScopeWithoutShowLogs(t)
		defer sc.Close(t)

		// Enable logging channels.
		log.TestingResetActive()
		cfg := logconfig.DefaultConfig()
		// Make a sink for just the session log.
		bt := true
		cfg.Sinks.FileGroups = map[string]*logconfig.FileSinkConfig{
			"auth": {
				FileDefaults: logconfig.FileDefaults{
					CommonSinkConfig: logconfig.CommonSinkConfig{Auditable: &bt},
				},
				Channels: logconfig.SelectChannels(channel.SESSIONS),
			},
			"dev": {
				FileDefaults: logconfig.FileDefaults{
					CommonSinkConfig: logconfig.CommonSinkConfig{Auditable: &bt},
				},
				Channels: logconfig.SelectChannels(channel.DEV),
			},
		}
		dir := sc.GetDirectory()
		if err := cfg.Validate(&dir); err != nil {
			t.Fatal(err)
		}
		cleanup, err := log.ApplyConfig(cfg, nil /* fileSinkMetricsForDir */, nil /* fatalOnLogStall */)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		s := serverutils.StartServerOnly(t,
			base.TestServerArgs{
				DefaultTestTenant: base.TestIsForStuffThatShouldWorkWithSecondaryTenantsButDoesntYet(107310),
				Insecure:          insecure,
				SocketFile:        maybeSocketFile,
			})
		defer s.Stopper().Stop(ctx)

		pgURL, cleanup := s.PGUrl(t, serverutils.User(username.RootUser), serverutils.ClientCerts(!insecure))
		defer cleanup()
		rootConn, err := pgx.Connect(ctx, pgURL.String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rootConn.Close(ctx) }()

		// Enable conn/auth logging.
		// We can't use the cluster settings to do this, because
		// cluster settings propagate asynchronously.
		testServer := s.ApplicationLayer()
		pgServer := testServer.PGServer().(*pgwire.Server)
		pgServer.TestingEnableConnLogging()
		pgServer.TestingEnableAuthLogging()
		testServer.PGPreServer().(*pgwire.PreServeConnHandler).TestingAcceptSystemIdentityOption(true)

		httpClient, err := s.GetAdminHTTPClient()
		if err != nil {
			t.Fatal(err)
		}
		httpHBAUrl := httpScheme + s.HTTPAddr() + "/debug/hba_conf"

		if _, err := rootConn.Exec(ctx, fmt.Sprintf(`CREATE USER %s`, username.TestUser)); err != nil {
			t.Fatal(err)
		}

		// This counter ensures that new log messages have become available
		// between calls to the authlog command. It avoids the case where
		// the last line from the previous authlog block also happens to
		// match the regex for the current authlog block, leading to a
		// desynchronization between the test script and the logfiles.
		lastAuthLogCounter := uint64(0)

		datadriven.RunTest(t, path, func(t *testing.T, td *datadriven.TestData) string {
			resultString, err := func() (string, error) {
				switch td.Cmd {
				case "config":
					allowed := false
					for _, a := range td.CmdArgs {
						switch a.Key {
						case "secure":
							allowed = allowed || !insecure
						case "insecure":
							allowed = allowed || insecure
						default:
							t.Fatalf("unknown configuration: %s", a.Key)
						}
					}
					if !allowed {
						skip.IgnoreLint(t, "Test file not applicable at this security level.")
					}

				case "accept_sql_without_tls":
					testServer.SetAcceptSQLWithoutTLS(true)

				case "reject_sql_without_tls":
					testServer.SetAcceptSQLWithoutTLS(false)

				case "set_hba":
					_, err := rootConn.Exec(ctx,
						`SET CLUSTER SETTING server.host_based_authentication.configuration = $1`, td.Input)
					if err != nil {
						return "", err
					}

					// Wait until the configuration has propagated back to the
					// test client. We need to wait because the cluster setting
					// change propagates asynchronously.
					expConf := pgwire.DefaultHBAConfig
					if td.Input != "" {
						expConf, err = pgwire.ParseAndNormalize(td.Input)
						if err != nil {
							// The SET above succeeded so we don't expect a problem here.
							t.Fatal(err)
						}
					}
					testutils.SucceedsSoon(t, func() error {
						curConf, _ := pgServer.GetAuthenticationConfiguration()
						if expConf.String() != curConf.String() {
							return errors.Newf(
								"HBA config not yet loaded\ngot:\n%s\nexpected:\n%s",
								curConf, expConf)
						}
						return nil
					})

					// Verify the HBA configuration was processed properly by
					// reporting the resulting cached configuration.
					resp, err := httpClient.Get(httpHBAUrl)
					if err != nil {
						return "", err
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						return "", err
					}
					return string(body), nil

				case "set_identity_map":
					_, err := rootConn.Exec(ctx,
						`SET CLUSTER SETTING server.identity_map.configuration = $1`, td.Input)
					if err != nil {
						return "", err
					}

					// Wait until the configuration has propagated back to the
					// test client. We need to wait because the cluster setting
					// change propagates asynchronously.
					expConf := identmap.Empty()
					if td.Input != "" {
						expConf, err = identmap.From(strings.NewReader(td.Input))
						if err != nil {
							// The SET above succeeded so we don't expect a problem here.
							t.Fatal(err)
						}
					}
					testutils.SucceedsSoon(t, func() error {
						_, curConf := pgServer.GetAuthenticationConfiguration()
						if expConf.String() != curConf.String() {
							return errors.Newf(
								"identity map not yet loaded\ngot:\n%s\nexpected:\n%s",
								curConf, expConf)
						}
						return nil
					})

					// Verify the HBA configuration was processed properly by
					// reporting the resulting cached configuration.
					resp, err := httpClient.Get(httpHBAUrl)
					if err != nil {
						return "", err
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						return "", err
					}
					return string(body), nil

				case "sql":
					_, err := rootConn.Exec(ctx, td.Input)
					return "ok", err

				case "authlog":
					if len(td.CmdArgs) < 1 {
						t.Fatal("not enough arguments")
					}
					numEntries, err := strconv.Atoi(td.CmdArgs[0].Key)
					if err != nil {
						t.Fatal(err)
					}
					re, err := regexp.Compile(td.Input)
					if err != nil {
						t.Fatal(err)
					}

					var buf strings.Builder
					if err := testutils.SucceedsSoonError(func() error {
						buf.Reset()
						// t.Logf("attempting to scan logs...")

						// Note: even though FetchEntriesFromFiles advertises a mechanism
						// to filter entries by timestamp or just retrieve the last N entries,
						// this is currently broken for secondary loggers.
						// See: https://github.com/cockroachdb/cockroach/issues/45745
						// So instead we need to do the filtering ourselves.
						entries, err := log.FetchEntriesFromFiles(0, math.MaxInt64, 10000, authLogFileRe,
							log.WithMarkedSensitiveData)
						if err != nil {
							t.Fatal(err)
						}
						if len(entries) == 0 {
							return errors.New("no log entries")
						} else {
							// Note: entries are delivered by Fetch in reverse order.
							i := numEntries - 1
							if i < 0 || i >= len(entries) {
								i = len(entries) - 1
							}
							for ; i >= 0; i-- {
								entry := &entries[i]
								// t.Logf("found log entry: %+v", *entry)

								if entry.StructuredEnd == 0 {
									t.Errorf("malformed structured message: %q", entry.Message)
								}

								jsonPayload := []byte(entry.Message)
								if entry.Redactable {
									jsonPayload = redact.RedactableBytes(jsonPayload).StripMarkers()
								}
								var info map[string]interface{}
								if err := json.Unmarshal(jsonPayload, &info); err != nil {
									return errors.Wrapf(err, "unable to decode json: %q", jsonPayload)
								}
								// Erase non-deterministic fields.
								info["Timestamp"] = "XXX"
								info["RemoteAddress"] = "XXX"
								info["SessionID"] = "XXX"
								if _, ok := info["Duration"]; ok {
									info["Duration"] = "NNN"
								}
								msg, err := json.Marshal(info)
								if err != nil {
									t.Fatal(err)
								}
								fmt.Fprintf(&buf, "%d %s\n", entry.Counter, msg)
							}
							lastLogMsg := entries[0].Message
							if !re.MatchString(lastLogMsg) {
								return errors.Newf("last entry does not match: %q", lastLogMsg)
							}
							if lastAuthLogCounter == entries[0].Counter {
								return errors.Newf("log counter has not advanced beyond: %d", lastAuthLogCounter)
							}
							lastAuthLogCounter = entries[0].Counter
						}
						return nil
					}); err != nil {
						buf.WriteString("ERROR: unable to find log line matching regexp")
					}
					return buf.String(), nil

				case "connect", "connect_unix":
					if td.Cmd == "connect_unix" && runtime.GOOS == "windows" {
						// Unix sockets not supported; assume the test succeeded.
						return td.Expected, nil
					}

					// We use rmArg to prevent test-only connection parameters to
					// leak to the session var option parser.
					rmArg := func(key string) {
						for i, a := range td.CmdArgs {
							if a.Key == key {
								td.CmdArgs = append(td.CmdArgs[:i], td.CmdArgs[i+1:]...)
								return
							}
						}
					}

					// Prepare a connection string using the server's default.
					// What is the user requested by the test?
					user := username.RootUser
					if td.HasArg("user") {
						td.ScanArgs(t, "user", &user)
					}

					// Allow connections for non-root, non-testuser to force the
					// use of client certificates.
					forceCerts := false
					if td.HasArg("force_certs") {
						rmArg("force_certs")
						forceCerts = true
					}

					// Whether to display the system identity as well (to test remappings).
					showSystemIdentity := false
					if td.HasArg("show_system_identity") {
						rmArg("show_system_identity")
						showSystemIdentity = true
					}

					// Whether to display the authentication_method as well.
					showAuthMethod := false
					if td.HasArg("show_authentication_method") {
						rmArg("show_authentication_method")
						showAuthMethod = true
					}

					certName := ""
					if td.HasArg("cert_name") {
						td.ScanArgs(t, "cert_name", &certName)
						rmArg("cert_name")
					}

					systemIdentity := user
					explicitSystemIdentity := td.HasArg("system_identity")
					if explicitSystemIdentity {
						td.ScanArgs(t, "system_identity", &systemIdentity)
						rmArg("system_identity")
					}

					// We want the certs to be present in the filesystem for this test.
					// However, certs are only generated for users "root" and "testuser" specifically.
					sqlURL, cleanupFn := pgurlutils.PGUrlWithOptionalClientCerts(
						t, s.AdvSQLAddr(), t.Name(), url.User(systemIdentity),
						forceCerts || systemIdentity == username.RootUser ||
							systemIdentity == username.TestUser, certName)
					defer cleanupFn()

					var host, port string
					if td.Cmd == "connect" {
						host, port, err = net.SplitHostPort(s.AdvSQLAddr())
						if err != nil {
							t.Fatal(err)
						}
					} else /* unix */ {
						host = maybeSocketDir
						port = socketConnVirtualPort
					}
					options, err := url.ParseQuery(sqlURL.RawQuery)
					if err != nil {
						t.Fatal(err)
					}

					// Here we make use of the fact that pq accepts connection
					// strings using the alternate postgres configuration format,
					// consisting of k=v pairs separated by spaces.
					// For example this is a valid connection string:
					//  "host=localhost port=5432 user=root"
					// We also make use of the datadriven K/V parsing facility,
					// which always prioritizes the first K instance in the test's
					// argument list. We append the server's config parameters
					// at the end, letting the test override by introducing its
					// own values at the beginning.
					args := append(td.CmdArgs,
						datadriven.CmdArg{Key: "user", Vals: []string{user}},
						datadriven.CmdArg{Key: "host", Vals: []string{host}},
						datadriven.CmdArg{Key: "port", Vals: []string{port}},
					)
					for key := range options {
						args = append(args,
							datadriven.CmdArg{Key: key, Vals: []string{options.Get(key)}})
					}

					if explicitSystemIdentity {
						args = append(args,
							datadriven.CmdArg{Key: "options", Vals: []string{"-csystem_identity=" + systemIdentity}})
					}

					// Now turn the cmdargs into a dsn.
					var dsnBuf strings.Builder
					sp := ""
					seenKeys := map[string]struct{}{}
					for _, a := range args {
						if _, ok := seenKeys[a.Key]; ok {
							continue
						}
						seenKeys[a.Key] = struct{}{}
						val := ""
						if len(a.Vals) > 0 {
							val = a.Vals[0]
						}
						if len(val) == 0 {
							// pgx.Connect requires empty values to be passed as a
							// single-quoted empty string.
							val = "''"
						}
						fmt.Fprintf(&dsnBuf, "%s%s=%s", sp, a.Key, val)
						sp = " "
					}
					dsn := dsnBuf.String()

					// Finally, connect and test the connection.
					dbSQL, err := pgx.Connect(ctx, dsn)
					if dbSQL != nil {
						// Note: pgx.Connect may return a valid db (with an open
						// TCP connection) even if there is an error. We want to
						// ensure this gets closed so that we catch the conn close
						// message in the log.
						defer func() { _ = dbSQL.Close(ctx) }()
					}
					if err != nil {
						// If the error is a PgError, return that directly instead of the
						// wrapped error. The wrapped error includes additional contextual
						// information that complicates checking for the expected error
						// string in tests.
						pgErr := new(pgconn.PgError)
						if errors.As(err, &pgErr) {
							return "", pgErr
						}
						return "", err
					}
					row := dbSQL.QueryRow(ctx, "SELECT current_catalog")
					var result string
					if err := row.Scan(&result); err != nil {
						return "", err
					}
					if showSystemIdentity {
						row := dbSQL.QueryRow(ctx, `SHOW system_identity`)
						var name string
						if err := row.Scan(&name); err != nil {
							t.Fatal(err)
						}
						result += " " + name
					}
					if showAuthMethod {
						var method, methodFromShowSessions string
						row := dbSQL.QueryRow(ctx, `SHOW authentication_method`)
						if err := row.Scan(&method); err != nil {
							t.Fatal(err)
						}
						// Verify that the session variable agrees with the information
						// in SHOW SESSIONS.
						row = dbSQL.QueryRow(ctx, `SELECT authentication_method FROM [SHOW SESSIONS] WHERE session_id = current_setting('session_id');`)
						if err := row.Scan(&methodFromShowSessions); err != nil {
							t.Fatal(err)
						}
						if method != methodFromShowSessions {
							t.Fatalf("SHOW SESSIONS disagrees with SHOW: %s vs %s", method, methodFromShowSessions)
						}
						result += " " + method
					}

					return "ok " + result, nil

				default:
					td.Fatalf(t, "unknown command: %s", td.Cmd)
				}
				return "", nil
			}()
			if err != nil {
				return fmtErr(err)
			}
			return resultString
		})
	})
}

var authLogFileRe = regexp.MustCompile(`"EventType":"client_`)

// fmtErr formats an error into an expected output.
func fmtErr(err error) string {
	if err != nil {
		errStr := ""
		if pgxErr := (*pgconn.PgError)(nil); errors.As(err, &pgxErr) {
			errStr = pgxErr.Message
			if pgcode.MakeCode(pgxErr.Code) != pgcode.Uncategorized {
				errStr += fmt.Sprintf(" (SQLSTATE %s)", pgxErr.Code)
			}
			if pgxErr.Hint != "" {
				hint := strings.Replace(pgxErr.Hint, stdstrings.IssueReferral, "<STANDARD REFERRAL>", 1)
				if strings.Contains(hint, "Supported methods:") {
					// Depending on whether the test is running on linux or not
					// (or, more specifically, whether gss build tag is set),
					// "gss" method might not be included, so we remove it here
					// and not include into the expected output.
					hint = strings.Replace(hint, "gss, ", "", 1)
				}
				errStr += "\nHINT: " + hint
			}
			if pgxErr.Detail != "" {
				errStr += "\nDETAIL: " + pgxErr.Detail
			}
		} else {
			errStr = err.Error()
			// pgx uses an internal type (pgconn.connectError) for "TLS not enabled"
			// errors here. We need to munge the error here to avoid including
			// non-stable information like IP addresses in the output.
			const tlsErr = "tls error: server refused TLS connection"
			if strings.HasSuffix(errStr, tlsErr) {
				errStr = tlsErr
			}
		}
		return "ERROR: " + errStr
	}
	return "ok"
}

// TestClientAddrOverride checks that the crdb:remote_addr parameter
// can override the client address.
func TestClientAddrOverride(t *testing.T) {
	defer leaktest.AfterTest(t)()
	sc := log.ScopeWithoutShowLogs(t)
	defer sc.Close(t)

	// Start a server.
	srv, db, _ := serverutils.StartServer(t, base.TestServerArgs{})
	ctx := context.Background()
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()

	pgURL, cleanupFunc := s.PGUrl(t, serverutils.CertsDirPrefix("testClientAddrOverride"), serverutils.User(username.TestUser))
	defer cleanupFunc()

	// Ensure the test user exists.
	if _, err := db.Exec(fmt.Sprintf(`CREATE USER %s`, username.TestUser)); err != nil {
		t.Fatal(err)
	}

	// Enable conn/auth logging.
	// We can't use the cluster settings to do this, because
	// cluster settings for booleans propagate asynchronously.
	pgServer := s.PGServer().(*pgwire.Server)
	pgServer.TestingEnableAuthLogging()
	pgPreServer := s.PGPreServer().(*pgwire.PreServeConnHandler)

	testCases := []struct {
		specialAddr string
		specialPort string
	}{
		{"11.22.33.44", "5566"},    // IPv4
		{"[11:22:33::44]", "5566"}, // IPv6
	}
	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s:%s", tc.specialAddr, tc.specialPort), func(t *testing.T) {
			// Create a custom HBA rule to refuse connections by the testuser
			// when coming from the special address.
			addr := tc.specialAddr
			mask := "32"
			if addr[0] == '[' {
				// An IPv6 address. The CIDR format in HBA rules does not
				// require the square brackets.
				addr = addr[1 : len(addr)-1]
				mask = "128"
			}
			hbaConf := "host all " + username.TestUser + " " + addr + "/" + mask + " reject\n" +
				"host all all all cert-password\n"
			if _, err := db.Exec(
				`SET CLUSTER SETTING server.host_based_authentication.configuration = $1`,
				hbaConf,
			); err != nil {
				t.Fatal(err)
			}

			// Wait until the configuration has propagated back to the
			// test client. We need to wait because the cluster setting
			// change propagates asynchronously.
			expConf, err := pgwire.ParseAndNormalize(hbaConf)
			if err != nil {
				// The SET above succeeded so we don't expect a problem here.
				t.Fatal(err)
			}
			testutils.SucceedsSoon(t, func() error {
				curConf, _ := pgServer.GetAuthenticationConfiguration()
				if expConf.String() != curConf.String() {
					return errors.Newf(
						"HBA config not yet loaded\ngot:\n%s\nexpected:\n%s",
						curConf, expConf)
				}
				return nil
			})

			// Inject the custom client address.
			options, _ := url.ParseQuery(pgURL.RawQuery)
			options["crdb:remote_addr"] = []string{tc.specialAddr + ":" + tc.specialPort}
			pgURL.RawQuery = options.Encode()

			t.Run("check-server-reject-override", func(t *testing.T) {
				// Connect a first time, with trust override disabled. In that case,
				// the server will complain that the remote override is not supported.
				_ = pgPreServer.TestingSetTrustClientProvidedRemoteAddr(false)

				testDB, err := gosql.Open("postgres", pgURL.String())
				if err != nil {
					t.Fatal(err)
				}
				defer testDB.Close()
				if err := testDB.Ping(); !testutils.IsError(err, "server not configured to accept remote address override") {
					t.Error(err)
				}
			})

			// Wait two full microseconds: we're parsing the log output below, and
			// the logging format has a microsecond precision on timestamps. We need to ensure that this check will not pick up log entries
			// from a previous test.
			time.Sleep(2 * time.Microsecond)
			testStartTime := timeutil.Now()

			t.Run("check-server-hba-uses-override", func(t *testing.T) {
				// Now recognize the override. Now we're expecting the connection
				// to hit the HBA rule and fail with an authentication error.
				_ = pgPreServer.TestingSetTrustClientProvidedRemoteAddr(true)

				testDB, err := gosql.Open("postgres", pgURL.String())
				if err != nil {
					t.Fatal(err)
				}
				defer testDB.Close()
				if err := testDB.Ping(); !testutils.IsError(err, "authentication rejected") {
					t.Error(err)
				}
			})

			t.Run("check-server-log-uses-override", func(t *testing.T) {
				// Wait for the disconnection event in logs.
				testutils.SucceedsSoon(t, func() error {
					log.FlushFiles()
					entries, err := log.FetchEntriesFromFiles(testStartTime.UnixNano(), math.MaxInt64, 10000, sessionTerminatedRe,
						log.WithMarkedSensitiveData)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) == 0 {
						return errors.New("entry not found")
					}
					return nil
				})

				// Now we want to check that the logging tags are also updated.
				log.FlushFiles()
				entries, err := log.FetchEntriesFromFiles(testStartTime.UnixNano(), math.MaxInt64, 10000, authLogFileRe,
					log.WithMarkedSensitiveData)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) == 0 {
					t.Fatal("no entries")
				}
				seenClient := false
				for _, e := range entries {
					t.Log(e.Tags)
					if strings.Contains(e.Tags, "client=") {
						seenClient = true
						if !strings.Contains(e.Tags, "client="+tc.specialAddr+":"+tc.specialPort) {
							t.Fatalf("expected override addr in log tags, got %+v", e)
						}
					}
				}
				if !seenClient {
					t.Fatal("no log entry found with the 'client' tag set")
				}
			})
		})
	}
}

// TestSSLSessionVar checks that the read-only SSL session variable correctly
// reflects the state of the connection.
func TestSSLSessionVar(t *testing.T) {
	defer leaktest.AfterTest(t)()
	sc := log.ScopeWithoutShowLogs(t)
	defer sc.Close(t)

	ctx := context.Background()
	// Start a server.
	srv, db, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()

	// Ensure the test user exists.
	if _, err := db.Exec(fmt.Sprintf(`CREATE USER %s WITH PASSWORD 'abc'`, username.TestUser)); err != nil {
		t.Fatal(err)
	}

	// Set the system layer to accept SQL without TLS in the event of a shared
	// process virtual cluster.
	srv.SystemLayer().SetAcceptSQLWithoutTLS(true)

	// TODO(herko): What effect should this have on a shared process virtual
	// cluster? See: https://github.com/cockroachdb/cockroach/issues/112961
	s.SetAcceptSQLWithoutTLS(true)

	pgURLWithCerts, cleanupFuncCerts := s.PGUrl(t,
		serverutils.CertsDirPrefix("TestSSLSessionVarCerts"),
		serverutils.User(username.TestUser),
		serverutils.ClientCerts(true),
	)
	defer cleanupFuncCerts()

	pgURLWithoutCerts, cleanupFuncWithoutCerts := s.PGUrl(t,
		serverutils.CertsDirPrefix("TestSSLSessionVarNoCerts"),
		serverutils.UserPassword(username.TestUser, "abc"),
		serverutils.ClientCerts(false),
	)
	defer cleanupFuncWithoutCerts()

	q := pgURLWithoutCerts.Query()
	q.Set("sslmode", "disable")
	pgURLWithoutCerts.RawQuery = q.Encode()

	// Connect with certs.
	connWithCerts, err := pgx.Connect(ctx, pgURLWithCerts.String())
	require.NoError(t, err)
	var result string
	err = connWithCerts.QueryRow(ctx, "SHOW ssl").Scan(&result)
	require.NoError(t, err)
	require.Equal(t, "on", result)

	// Connect without certs.
	connWithoutCerts, err := pgx.Connect(ctx, pgURLWithoutCerts.String())
	require.NoError(t, err)
	err = connWithoutCerts.QueryRow(ctx, "SHOW ssl").Scan(&result)
	require.NoError(t, err)
	require.Equal(t, "off", result)
}

var sessionTerminatedRe = regexp.MustCompile("client_session_end")
