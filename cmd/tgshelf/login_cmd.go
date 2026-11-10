package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/tg"
)

// loginReader is the input source for the login interaction; tests can inject a replacement for
// os.Stdin.
var loginReader io.Reader = os.Stdin

// stdinBuf is the **only** buffered stdin reader.
//
// It must be shared globally: if every input created a new bufio.Reader, the first read would
// pull all the buffered lines in the kernel buffer (for example a pasted "phone number\nlogin
// code\n") into that private buffer and the second read would never see the remaining lines -
// showing up as "it failed without asking for the login code". Reusing the same reader is what
// makes consecutive ReadString calls return the lines in order.
var stdinBuf *bufio.Reader

// bufReader returns the shared buffered stdin reader (created on the first call).
func bufReader() *bufio.Reader {
	if stdinBuf == nil {
		stdinBuf = bufio.NewReader(loginReader)
	}
	return stdinBuf
}

// readMasked reads one line from the terminal and echoes it as '*' (used for the 2FA password).
//
// The only difference from readSecret is the visual feedback: the input is not shown, yet the
// user does not think the program is stuck the way fully disabled echo makes them feel. The
// implementation switches the terminal to "no ECHO" and reads byte by byte itself, printing one
// '*' per printable character read. Backspace erases one '*', Enter finishes the line and breaks
// it. When f is not a terminal (pipe/test) it degrades to a plain line read and prints no asterisk.
func readMasked(f *os.File) (string, error) {
	fd := int(f.Fd())
	saved, terr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if terr != nil {
		line, err := secretReader(f).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	noecho := *saved
	noecho.Lflag &^= unix.ECHO
	if serr := unix.IoctlSetTermios(fd, unix.TCSETS, &noecho); serr != nil {
		return "", fmt.Errorf("disable terminal echo: %w", serr)
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, saved) }()

	var buf []byte
	one := make([]byte, 1)
	for {
		n, rerr := f.Read(one)
		if n > 0 {
			switch c := one[0]; {
			case c == '\r' || c == '\n':
				// End of input: restore echo, then add a newline (the Enter key is not echoed,
				// so the terminal does not wrap the line itself).
				_ = unix.IoctlSetTermios(fd, unix.TCSETS, saved)
				fmt.Println()
				return strings.TrimSpace(string(buf)), nil
			case c == 0x7f || c == 0x08: // Backspace / DEL
				if len(buf) > 0 {
					buf = buf[:len(buf)-1]
					fmt.Print("\b \b")
				}
			case c == 0x03: // Ctrl-C
				_ = unix.IoctlSetTermios(fd, unix.TCSETS, saved)
				fmt.Println()
				return "", errors.New("input cancelled by the user (Ctrl-C)")
			case c >= 0x20:
				buf = append(buf, c)
				fmt.Print("*")
			}
		}
		if rerr != nil {
			_ = unix.IoctlSetTermios(fd, unix.TCSETS, saved)
			fmt.Println()
			if errors.Is(rerr, io.EOF) {
				return strings.TrimSpace(string(buf)), nil
			}
			return "", rerr
		}
	}
}

// secretReader picks the reader for f: when f is stdin (the same object loginReader points at)
// it uses the shared reader so its buffering does not diverge from promptLine and lose lines;
// otherwise it uses f's own reader (a pipe injected by a test).
func secretReader(f *os.File) *bufio.Reader {
	if r, ok := loginReader.(*os.File); ok && r == f {
		return bufReader()
	}
	return bufio.NewReader(f)
}

// runLogin runs the interactive login: phone number -> login code -> (optional) 2FA password.
// On success the session file is written out by gotd's SessionStorage.
//
// This command does not need a bot_token, so cfg.Validate() is skipped and only api_id/api_hash
// are checked.
func runLogin(cfg *config.Config, configPath string, keepSession bool) error {
	if err := preflightTelegram(cfg); err != nil {
		return err
	}

	sessionPath := sessionPathOf(cfg, configPath)
	if err := ensureSessionParent(sessionPath); err != nil {
		return err
	}

	// Strategy for an existing session:
	//   A stale session (a previous login aborted halfway, a non-empty auth key left behind
	//   that the server has already dropped) makes auth.sendCode return 500 AUTH_RESTART, and
	//   retrying is pointless. Since this is the "log in again" path anyway, the safest move
	//   is to move the old session to .bak and start over instead of gambling on an auth key
	//   that is known to be possibly invalid.
	//   The user can keep the old file with -keep-session (for debugging or to keep an account
	//   switch).
	if _, statErr := os.Stat(sessionPath); statErr == nil {
		if keepSession {
			fmt.Println("A session already exists; keeping it because of -keep-session and logging in on top of it")
		} else {
			backupPath := sessionPath + ".bak"
			_ = os.Remove(backupPath)
			if mvErr := os.Rename(sessionPath, backupPath); mvErr != nil {
				return fmt.Errorf("back up old session: %w", mvErr)
			}
			fmt.Printf("A session already exists and was backed up to %s (pass -keep-session to keep the old session)\n", backupPath)
		}
	}

	storage, err := tg.OpenSession(sessionPath)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = storage.Close() }()

	client, err := tg.New(cfg, storage)
	if err != nil {
		return fmt.Errorf("build Telegram client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// gotd convention: once fn returns nil, Run shuts down and exits by itself, so no extra
	// cancellation is needed.
	err = client.Run(ctx, func(ctx context.Context) error {
		return doLogin(ctx, client)
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	return nil
}

// doLogin runs the interactive login flow once the gotd connection is ready.
//
// Echo policy (as requested by the user):
//   - login code: **echoed in plaintext** so the user can check that what they typed is right;
//   - 2FA password: stays hidden (it is a long-lived account secret, not a one-time number);
//   - phone number: only the last 4 digits are shown in errors.
//
// Note: echoing the login code means it stays in the terminal scrollback and possibly in the
// shell history; the code is valid for a few minutes only and single-use, so the risk is acceptable.
func doLogin(ctx context.Context, client *tg.Client) error {
	phone, err := promptLine("Enter your phone number (example format +8613800138000): ")
	if err != nil {
		return fmt.Errorf("read phone number: %w", err)
	}

	hash, err := client.SendCode(ctx, phone)
	if err != nil {
		// A failed SendCode must not echo the phone number; only the last 4 digits are shown.
		return fmt.Errorf("send login code (phone number ending in %s): %w", maskPhone(phone), err)
	}

	fmt.Print("Enter the login code (the 5-6 digit number Telegram sent): ")
	code, err := promptLine("")
	if err != nil {
		return fmt.Errorf("read login code: %w", err)
	}
	if code == "" {
		return errors.New("login code is empty, login cancelled")
	}

	needPassword, err := client.SignIn(ctx, phone, hash, code)
	if err != nil {
		return fmt.Errorf("verify login: %w", err)
	}

	if needPassword {
		fmt.Print("Enter your 2FA password (shown as * while typing): ")
		password, perr := readMasked(os.Stdin)
		if perr != nil {
			return fmt.Errorf("read 2FA password: %w", perr)
		}
		if perr := client.CheckPassword(ctx, strings.TrimSpace(password)); perr != nil {
			return fmt.Errorf("2FA verification: %w", perr)
		}
	}

	id, username, dc, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("fetch login identity: %w", err)
	}
	fmt.Printf("Logged in: id=%d username=%s dc=%d\n", id, username, dc)
	return nil
}

// promptLine reads one line from loginReader (surrounding whitespace trimmed) after printing the
// prompt.
func promptLine(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := bufReader().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// maskPhone keeps only the last 4 digits of the phone number for messages and replaces the rest
// with *.
func maskPhone(phone string) string {
	p := strings.TrimSpace(phone)
	if len(p) <= 4 {
		return "****"
	}
	return "****" + p[len(p)-4:]
}

// readSecret reads one line from the terminal without echoing it (used for the login code and the
// 2FA password).
//
// When f is a terminal (TCGETS succeeds) it clears the ECHO bit for the read and restores the
// original termios right afterwards (defer covers every path, including a read error); when f is
// not a terminal (a pipe or a redirect, for example) it degrades to a plain line read with the
// same behaviour as ordinary input.
func readSecret(f *os.File) (string, error) {
	fd := int(f.Fd())
	saved, terr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if terr != nil {
		// Not a terminal: read one line directly, no error.
		// When f is the stdin that loginReader points at, reuse the shared reader (so the
		// buffer is shared with promptLine); otherwise (a pipe injected by a test, etc.) use
		// f's own reader.
		line, err := secretReader(f).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	// Clear the ECHO bit (keeping the other flags); the ioctl takes effect immediately.
	noecho := *saved
	noecho.Lflag &^= unix.ECHO
	if serr := unix.IoctlSetTermios(fd, unix.TCSETS, &noecho); serr != nil {
		return "", fmt.Errorf("disable terminal echo: %w", serr)
	}
	defer func() {
		// The restore must also happen on the error paths: read failures, EOF and panics all
		// end up here.
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, saved)
	}()

	line, err := secretReader(f).ReadString('\n')
	// Restore echo first, whether reading succeeded or not, then handle the result.
	if rerr := unix.IoctlSetTermios(fd, unix.TCSETS, saved); rerr != nil {
		return "", fmt.Errorf("restore terminal echo: %w", rerr)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
