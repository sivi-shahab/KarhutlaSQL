// Command karhutlasql-client adalah klien CLI KarhutlaSQL (psql-like): REPL
// interaktif, --command, atau SQL dari stdin.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"golang.org/x/term"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/report"
	"github.com/bankmega/karhutlasql/internal/version"
	"github.com/bankmega/karhutlasql/pkg/client"
	"github.com/bankmega/karhutlasql/pkg/client/oauth"
)

func main() {
	// SIGINT tidak di sini: di REPL Ctrl+C hanya membatalkan statement aktif.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type options struct {
	cfg         client.Config
	host        string
	port        int
	passwordEnv string
	command     string
	format      string
	historyFile string
	timing      bool

	oidc      oauth.Config
	oidcLogin bool
	browser   bool
	tokenFile string

	verify    string // manifest.json report (G-10)
	publicKey string
}

func run(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) int {
	var o options
	cmd := &cobra.Command{
		Use:           "karhutlasql-client",
		Short:         "Klien CLI KarhutlaSQL (Arrow Flight SQL)",
		Version:       version.Version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			return o.run(ctx, stdin, stdout, stderr)
		},
	}
	home, _ := os.UserHomeDir()
	fs := cmd.Flags()
	fs.StringVar(&o.host, "host", "localhost", "host server")
	fs.IntVar(&o.port, "port", 31337, "port server")
	fs.StringVarP(&o.cfg.Username, "username", "U", "admin", "user")
	fs.StringVar(&o.passwordEnv, "password-env", "KARHUTLASQL_PASSWORD", "nama env var berisi password; kosong di terminal = prompt")
	fs.BoolVar(&o.cfg.TLS, "tls", false, "pakai TLS (otomatis bila flag --tls-* lain diset)")
	fs.BoolVar(&o.cfg.SkipVerify, "tls-skip-verify", false, "TLS tanpa verifikasi sertifikat server (dev saja)")
	fs.StringVar(&o.cfg.CAFile, "tls-ca", "", "CA PEM untuk verifikasi server")
	fs.StringVar(&o.cfg.CertFile, "tls-cert", "", "client cert PEM (mTLS)")
	fs.StringVar(&o.cfg.KeyFile, "tls-key", "", "client key PEM (mTLS)")
	fs.StringVarP(&o.command, "command", "c", "", "eksekusi SQL lalu keluar")
	fs.StringVar(&o.format, "format", "table", "format output: "+strings.Join(formats, "|"))
	fs.StringVar(&o.historyFile, "history-file", filepath.Join(home, ".karhutlasql_history"), "file history REPL (0600); kosong = mati")
	fs.BoolVar(&o.timing, "timing", false, "tampilkan durasi tiap statement")
	fs.StringVar(&o.oidc.Issuer, "oidc-issuer", "", "issuer OIDC; diset = SSO (token tersimpan) menggantikan password")
	fs.StringVar(&o.oidc.ClientID, "oidc-client-id", "karhutlasql", "client ID OIDC (public client)")
	fs.BoolVar(&o.oidcLogin, "oidc-login", false, "login SSO baru (Device Code; --browser = Auth Code + PKCE) lalu simpan token")
	fs.BoolVar(&o.browser, "browser", false, "dengan --oidc-login: login lewat browser (PKCE) alih-alih device code")
	fs.StringVar(&o.verify, "verify", "", "verifikasi report: path manifest.json (tanda tangan + hash result di direktori yang sama), tanpa koneksi server")
	fs.StringVar(&o.publicKey, "public-key", "", "dengan --verify: kunci publik Ed25519 PEM server")
	fs.StringVar(&o.tokenFile, "token-file", filepath.Join(home, ".karhutlasql", "token.json"), "file token SSO (0600)")
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, errText(err))
		return 1
	}
	return 0
}

func (o *options) run(ctx context.Context, stdin *os.File, stdout, stderr io.Writer) error {
	if o.verify != "" {
		return verifyReport(o.verify, o.publicKey, stdout)
	}
	if !slices.Contains(formats, o.format) {
		return fmt.Errorf("--format %q: want one of %v", o.format, formats)
	}
	if (o.cfg.CertFile == "") != (o.cfg.KeyFile == "") {
		return errors.New("--tls-cert and --tls-key must be set together")
	}
	interactive := isTerminal(stdin)
	o.cfg.Addr = net.JoinHostPort(o.host, strconv.Itoa(o.port))
	if o.oidcLogin && o.oidc.Issuer == "" {
		return errors.New("--oidc-login requires --oidc-issuer")
	}
	if o.oidc.Issuer != "" {
		if err := o.sso(ctx, stderr); err != nil {
			return err
		}
	} else if o.cfg.Password = os.Getenv(o.passwordEnv); o.cfg.Password == "" {
		if !interactive {
			return fmt.Errorf("env %s is empty", o.passwordEnv)
		}
		_, _ = fmt.Fprintf(stderr, "Password for %s: ", o.cfg.Username)
		pw, err := term.ReadPassword(int(stdin.Fd())) //nolint:gosec // fd kecil, tidak overflow
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			return err
		}
		o.cfg.Password = string(pw)
	}

	c, err := client.Dial(ctx, o.cfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	s := &session{c: c, out: stdout, errOut: stderr, format: o.format, timing: o.timing}

	switch {
	case o.command != "":
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		return s.run(ctx, lines(strings.NewReader(o.command)), true)
	case !interactive:
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		return s.run(ctx, lines(stdin), true)
	default:
		return s.repl(ctx, stdin, o.historyFile)
	}
}

// sso mengisi cfg.TokenSource dari login baru atau token tersimpan.
func (o *options) sso(ctx context.Context, stderr io.Writer) error {
	store := oauth.FileStore{Path: o.tokenFile}
	var (
		tok *oauth2.Token
		err error
	)
	switch {
	case o.oidcLogin && o.browser:
		tok, err = oauth.BrowserLogin(ctx, o.oidc, stderr)
	case o.oidcLogin:
		tok, err = oauth.DeviceLogin(ctx, o.oidc, stderr)
	default:
		tok, err = store.Load(o.oidc)
	}
	if err != nil {
		return err
	}
	if o.oidcLogin {
		if err := store.Save(o.oidc, tok); err != nil {
			return err
		}
	}
	o.cfg.TokenSource, err = oauth.TokenSource(ctx, o.oidc, tok, store)
	return err
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) } //nolint:gosec // fd kecil

// lines membaca r baris per baris (tanpa '\n').
func lines(r io.Reader) func(bool) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 16<<20)
	return func(bool) (string, error) {
		if sc.Scan() {
			return sc.Text(), nil
		}
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
}

type session struct {
	c           *client.Client
	out, errOut io.Writer
	format      string
	timing      bool
	interactive bool // Ctrl+C membatalkan statement aktif, error tidak menghentikan sesi
}

// run membaca baris dari next (argumen: true bila sedang di tengah statement)
// sampai io.EOF atau \q. Statement diakhiri `;`; sisa tanpa `;` di akhir input
// tetap dieksekusi. stopOnErr: kembalikan error pertama (mode skrip).
func (s *session) run(ctx context.Context, next func(cont bool) (string, error), stopOnErr bool) error {
	var buf string
	do := func(fn func() error) error {
		err := fn()
		if err == nil || stopOnErr {
			return err
		}
		_, _ = fmt.Fprintln(s.errOut, errText(err))
		return nil
	}
	for {
		pending := strings.TrimSpace(buf) != ""
		line, err := next(pending)
		if errors.Is(err, io.EOF) {
			if pending {
				return do(func() error { return s.exec(ctx, buf) })
			}
			return nil
		}
		if err != nil {
			return err
		}
		if cmd := strings.TrimSpace(line); !pending && strings.HasPrefix(cmd, `\`) {
			if cmd == `\q` {
				return nil
			}
			if err := do(func() error { return s.meta(ctx, cmd) }); err != nil {
				return err
			}
			continue
		}
		var stmts []string
		stmts, buf = split(buf + line + "\n")
		for _, st := range stmts {
			if err := do(func() error { return s.exec(ctx, st) }); err != nil {
				return err
			}
		}
	}
}

func errText(err error) string {
	// errors.As, bukan status.FromError: pesan tanpa prefix wrapper pkg/client.
	var se interface{ GRPCStatus() *status.Status }
	if errors.As(err, &se) {
		st := se.GRPCStatus()
		return fmt.Sprintf("ERROR: %s (%s)", st.Message(), st.Code())
	}
	return "ERROR: " + err.Error()
}

func (s *session) exec(ctx context.Context, sql string) error {
	return s.timed(ctx, func(ctx context.Context) error {
		rdr, err := s.c.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rdr.Release()
		_, err = write(s.out, s.format, rdr)
		return err
	})
}

func (s *session) timed(ctx context.Context, fn func(context.Context) error) error {
	if s.interactive {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
	}
	start := time.Now()
	err := fn(ctx)
	if s.timing {
		_, _ = fmt.Fprintf(s.out, "Time: %.3f ms\n", float64(time.Since(start).Microseconds())/1000)
	}
	return err
}

const help = `\dt        daftar tabel
\timing    toggle durasi statement
\q         keluar
\?         bantuan ini
SQL diakhiri ';'. Ctrl+C membatalkan statement yang sedang berjalan.
`

func (s *session) meta(ctx context.Context, cmd string) error {
	switch cmd {
	case `\dt`:
		return s.timed(ctx, func(ctx context.Context) error {
			rdr, err := s.c.Tables(ctx)
			if err != nil {
				return err
			}
			defer rdr.Release()
			_, err = write(s.out, s.format, rdr)
			return err
		})
	case `\timing`:
		s.timing = !s.timing
		_, err := fmt.Fprintf(s.out, "Timing is %s.\n", map[bool]string{true: "on", false: "off"}[s.timing])
		return err
	case `\?`:
		_, err := io.WriteString(s.out, help)
		return err
	}
	return fmt.Errorf("unknown command %s; try \\?", cmd)
}

// repl: mode raw hanya selama membaca baris (line editing + history), mode
// normal selama eksekusi agar Ctrl+C menjadi SIGINT pembatal statement.
func (s *session) repl(ctx context.Context, stdin *os.File, historyFile string) error {
	s.interactive = true
	fd := int(stdin.Fd()) //nolint:gosec // fd kecil
	t := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{stdin, s.out}, "")
	hist, err := openHistory(t, historyFile)
	if err != nil {
		_, _ = fmt.Fprintln(s.errOut, "history disabled:", err)
	}
	if hist != nil {
		defer func() { _ = hist.Close() }()
	}
	_, _ = fmt.Fprintf(s.out, "karhutlasql-client %s — \\? untuk bantuan, \\q untuk keluar\n", version.Version)
	return s.run(ctx, func(cont bool) (string, error) {
		t.SetPrompt(map[bool]string{false: "karhutlasql=> ", true: "karhutlasql-> "}[cont])
		old, err := term.MakeRaw(fd)
		if err != nil {
			return "", err
		}
		line, err := t.ReadLine()
		_ = term.Restore(fd, old)
		if err == nil && hist != nil && strings.TrimSpace(line) != "" {
			_, _ = fmt.Fprintln(hist, line)
		}
		return line, err
	}, false)
}

// ponytail: file history tumbuh tanpa batas (hanya 100 baris terakhir yang
// dimuat); rotasi bila jadi masalah.
func openHistory(t *term.Terminal, path string) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	prev := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for _, l := range prev[max(0, len(prev)-100):] {
		if l != "" {
			t.History.Add(l)
		}
	}
	return f, nil
}

// verifyReport memeriksa manifest.json(.sig) dan file hasil di sebelahnya.
func verifyReport(manifestPath, pubPath string, out io.Writer) error {
	if pubPath == "" {
		return errors.New("--verify requires --public-key")
	}
	read := func(p string) ([]byte, error) { return os.ReadFile(filepath.Clean(p)) }
	pemBytes, err := read(pubPath)
	if err != nil {
		return err
	}
	pub, err := report.ParsePublicKey(pemBytes)
	if err != nil {
		return err
	}
	manifest, err := read(manifestPath)
	if err != nil {
		return err
	}
	sig, err := read(manifestPath + ".sig")
	if err != nil {
		return err
	}
	var hdr report.Manifest
	if err := json.Unmarshal(manifest, &hdr); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if hdr.Result.File == "" || filepath.Base(hdr.Result.File) != hdr.Result.File {
		return fmt.Errorf("manifest: invalid result file %q", hdr.Result.File)
	}
	result, err := read(filepath.Join(filepath.Dir(manifestPath), hdr.Result.File))
	if err != nil {
		return err
	}
	m, err := report.Verify(manifest, sig, result, pub)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "OK report %s (%s) by %s at %s\nsnapshot %s, %d rows, result sha256 %s, key %s\n",
		m.ReportID, m.Name, m.User, m.FinishedAt.Format(time.RFC3339), m.SnapshotID, m.Result.Rows, m.Result.SHA256, m.KeyID)
	return nil
}
