// Command tgshelf is the main entry point of tgshelf and starts the Telegram bot service.
//
// Startup chain: parse flags -> load config (generate the default config on first run) ->
// validate config -> initialize zap logging -> create the data directory -> acquire the
// single-instance lock (flock) -> open state -> start the bot (tgbot.New -> bot.NewPolling -> Run).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tgbot "github.com/go-telegram/bot"

	"github.com/1368129224/tgshelf/internal/bot"
	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/fs"
	"github.com/1368129224/tgshelf/internal/logging"
	"github.com/1368129224/tgshelf/internal/state"
	"github.com/1368129224/tgshelf/internal/tg"
	"go.uber.org/zap"
)

// run holds the main business logic and returns an error so main can map it to an exit code.
//
// -h/--help returns nil (exit code 0); every other error returns non-nil (exit code 1).
// This function never panics: all errors are wrapped with a prefix and passed upward.
func run(args []string) error {
	fset := flag.NewFlagSet("tgshelf", flag.ContinueOnError)
	fset.Usage = func() {
		_, _ = fmt.Fprintf(fset.Output(), "tgshelf (Telegram Shelf) - a self-hosted Telegram media archiver bot.\n"+
			"Downloads the media behind t.me links to local storage by tag, with a built-in MTProto client, no Python or external download tool needed.\n\nUsage:\n  tgshelf [options]\n  tgshelf -login [-config X]\n  tgshelf -resolve <t.me link> [-resolve <link2> ...] [-config X]\n  tgshelf -download <t.me link> [-download <link2> ...] [-to dir] [-tag name] [-threads N] [-file-concurrency N] [-keep-parts] [-chunk-bytes bytes] [-config X]\n  tgshelf -sessions [-config X]\n  tgshelf -health [-config X]\n\nOptions:\n")
		fset.PrintDefaults()
	}

	configPath := fset.String("config", "tgshelf_config.toml", "path to the config file")
	healthMode := fset.Bool("health", false, "health check for Docker HEALTHCHECK (exit 0 means healthy)")
	fset.BoolVar(healthMode, "healthcheck", false, "alias for -health")
	loginMode := fset.Bool("login", false, "log in to a Telegram account interactively (phone number, login code, 2FA) and write the session file on success")
	keepSession := fset.Bool("keep-session", false, "with -login: keep the existing session file (by default it is backed up to session.json.bak and login starts over)")
	resolveMode := &resolveLinks{}
	fset.Var(resolveMode, "resolve", "resolve t.me links and print a media report; repeat the flag or separate links with commas")
	sessionsMode := fset.Bool("sessions", false, "list the active sessions of the current account (to check whether the PC client was logged out server-side)")
	downloadMode := &downloadArgs{}
	fset.Var(downloadMode, "download", "download the media behind t.me links to disk; repeat the flag or separate links with commas")
	fset.StringVar(&downloadMode.to, "to", "", "download target directory (defaults to config download_path)")
	fset.StringVar(&downloadMode.tag, "tag", "", "download subdirectory tag (must be listed in config tags)")
	fset.IntVar(&downloadMode.threads, "threads", 0, "number of download threads per file (defaults to config download.threads)")
	fset.IntVar(&downloadMode.fileConcurrency, "file-concurrency", 0, "number of concurrent files (defaults to config download.file_concurrency)")
	fset.BoolVar(&downloadMode.keepParts, "keep-parts", false, "keep the .state file after a successful download")
	fset.Int64Var(&downloadMode.chunkBytes, "chunk-bytes", 0, "chunk size in bytes (must divide 1MiB; default 1MiB)")

	if err := fset.Parse(args); err != nil {
		// flag.ContinueOnError returns flag.ErrHelp for -h/--help
		if errors.Is(err, flag.ErrHelp) {
			return nil // exit 0
		}
		return fmt.Errorf("parse command line arguments: %w", err)
	}

	if *healthMode {
		return runHealthCheck(*configPath)
	}

	// Subcommand-style switches: -login / -resolve skip the bot_token validation
	// (they do not need it) and run their own lightweight flows; they are mutually
	// exclusive, and passing several at once is treated as an argument error.
	if *loginMode && (len(resolveMode.urls) > 0 || len(downloadMode.urls) > 0 || *sessionsMode) {
		return errors.New("-login cannot be combined with -resolve/-download/-sessions")
	}
	if len(resolveMode.urls) > 0 && len(downloadMode.urls) > 0 {
		return errors.New("-resolve cannot be combined with -download")
	}
	if *sessionsMode && (len(resolveMode.urls) > 0 || len(downloadMode.urls) > 0) {
		return errors.New("-sessions cannot be combined with -resolve/-download")
	}
	if *sessionsMode {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		return runSessions(cfg, *configPath)
	}
	if *loginMode {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		return runLogin(cfg, *configPath, *keepSession)
	}
	if len(resolveMode.urls) > 0 {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		return runResolve(cfg, *configPath, resolveMode.urls)
	}
	if len(downloadMode.urls) > 0 {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		return runDownload(cfg, *configPath, downloadMode)
	}

	// Signal handling: SIGINT/SIGTERM cancel ctx and drive the graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Step 1: load the config; when the file is missing, generate a commented default
	// config and tell the user to fill it in.
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Step 2: validate the config (all errors are aggregated into one report).
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}

	// Step 3: prepare the data directory (data/ next to the config file).
	dataDir := dataDirOf(*configPath)
	if err := fs.EnsureDir(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	// Step 4: initialize zap logging (console + rotating file + automatic redaction of secrets).
	logger, logCloser, err := initLogging(cfg, dataDir)
	if err != nil {
		return fmt.Errorf("initialize logging: %w", err)
	}
	defer func() {
		_ = logger.Sync()
		if logCloser != nil {
			_ = logCloser.Close()
		}
	}()

	logger.Info("config loaded",
		zap.String("config_path", *configPath),
		zap.String("bot_token", maskToken(cfg.BotToken)),
		zap.String("download_path", cfg.DownloadPath),
	)

	// Step 5: single-instance lock, so two processes never share the same Telegram session.
	lockPath := filepath.Join(dataDir, "session.lock")
	lock, err := fs.AcquireLockWithContext(ctx, lockPath, fs.LockOptions{})
	if err != nil {
		return fmt.Errorf("acquire single-instance lock: %w", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			logger.Warn("release single-instance lock", zap.Error(err))
		}
	}()

	// Step 6: open the state store (data/state.json).
	st, err := state.New(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return fmt.Errorf("open state file: %w", err)
	}
	defer func() { _ = st.Close() }() // Close performs the final Flush internally

	// Step 7: start the bot.
	if err := startBot(ctx, cfg, logger, st, *configPath); err != nil {
		return fmt.Errorf("start bot: %w", err)
	}

	return nil
}

// loadConfig loads the config file; when it does not exist (ErrNotExist) a commented
// default config is generated and an error tells the user to fill in bot_token and retry.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, config.ErrNotExist) {
			if werr := config.WriteDefault(path); werr != nil {
				return nil, fmt.Errorf("write default config: %w", werr)
			}
			return nil, fmt.Errorf("generated default config %s, fill in bot_token and try again", path)
		}
		return nil, err
	}
	return cfg, nil
}

// initLogging initializes the logging stack: console output + rotating file (1MB x 3 files)
// + automatic redaction of sensitive values.
func initLogging(cfg *config.Config, dataDir string) (*zap.Logger, io.Closer, error) {
	opts := logging.Options{
		Debug:       cfg.Debug,
		LogFilePath: filepath.Join(dataDir, "tgshelf.log"),
		MaxBytes:    logging.DefaultMaxBytes,
		MaxBackups:  logging.DefaultMaxBackups,
	}
	if cfg != nil {
		opts.Secrets = append(opts.Secrets, cfg.BotToken, cfg.Telegram.APIHash, cfg.ProxyURL)
	}
	return logging.New(opts)
}

// runHealthCheck is called by the container health check; it verifies the config, the
// free disk space and the session file.
//
// The verdict matches a first deployment:
//   - no session file = /login has not run yet, a legitimate state, the container is
//     healthy (otherwise the first start would always be unhealthy and the user would
//     think the deployment failed);
//   - no download directory = nothing downloaded yet, equally legitimate (the directory
//     is created with the first task);
//   - anything else (unreadable config, existing but unreadable session, low disk space)
//     is unhealthy.
func runHealthCheck(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("health check failed: cannot load config: %w", err)
	}

	// Disk: only checked when the download directory exists (statfs fails on a missing path).
	if _, statErr := os.Stat(cfg.DownloadPath); statErr == nil {
		if err := fs.CheckSpace(cfg.DownloadPath, 10*1024*1024); err != nil {
			return fmt.Errorf("health check failed: not enough disk space: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("health check failed: cannot access download directory %s: %w", cfg.DownloadPath, statErr)
	}

	sessionPath := sessionPathOf(cfg, configPath)
	if _, err := os.Stat(sessionPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("OK (not logged in yet: waiting for /login)")
			return nil
		}
		return fmt.Errorf("health check failed: session file unreadable: %w", err)
	}

	fmt.Println("OK")
	return nil
}

// dataDirOf returns the data directory: the "data" subdirectory of the config file directory.
// Keeping it next to the config file makes it independent of the working directory (deployment
// and tests can run from any cwd).
func dataDirOf(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "data")
}

// maskToken redacts bot_token: any non-empty value returns "***", so the plaintext never
// appears in the logs.
func maskToken(token string) string {
	if token == "" {
		return "(not set)"
	}
	return "***"
}

// startBot starts the Telegram bot: it builds the bot client and the MTProto client, injects
// the download executor plus sync/account lookups, then enters polling. This function only does
// the wiring, the business logic lives in internal/bot.
//
// Why b.Run goes into the client.Run callback: a gotd authorized session is only valid inside
// the Run callback, and downloads and dialog sync must both be started in that scope; the two
// therefore run in parallel under the same ctx and exit together when ctx is cancelled
// (SIGINT/SIGTERM).
func startBot(ctx context.Context, cfg *config.Config, logger *zap.Logger, st *state.Store, configPath string) error {
	// The Bot API HTTP client must use the proxy on its own: go-telegram/bot does not reuse
	// the MTProto dialer. On hosts that can only reach the internet through a proxy, skipping
	// this step makes getMe time out and the bot never starts.
	opts := []tgbot.Option{}
	httpClient, herr := tg.NewProxiedHTTPClient(cfg.ProxyURL)
	if herr != nil {
		return fmt.Errorf("build Bot API proxy client: %w", herr)
	}
	if httpClient != nil {
		// Poll timeout 25s, HTTP timeout 120s (long polling must be well above the poll timeout).
		opts = append(opts, tgbot.WithHTTPClient(25*time.Second, httpClient))
	}
	raw, err := tgbot.New(cfg.BotToken, opts...)
	if err != nil {
		return fmt.Errorf("build Telegram bot client: %w", err)
	}
	b, err := bot.NewPolling(raw, cfg, logger, st)
	if err != nil {
		return fmt.Errorf("assemble bot: %w", err)
	}

	// MTProto session: on a first deployment a missing session file is a **normal state**,
	// not an error. The bot must still start, otherwise "docker compose up -d, then /login in
	// Telegram" - the only first-run path - would not work (the container would exit right
	// away and nobody would answer in the chat). gotd handles a missing session by creating a
	// new auth key and entering the logged-out state, and the /login wizard can then send the
	// code, sign in and do 2FA through the same client.
	sessionPath := sessionPathOf(cfg, configPath)
	if _, serr := os.Stat(sessionPath); serr != nil {
		if !os.IsNotExist(serr) {
			return fmt.Errorf("check session file: %w", serr)
		}
		logger.Warn("not logged in yet: session file missing, starting the bot in the logged-out state; send /login in Telegram to log in",
			zap.String("session_path", sessionPath))
	}
	storage, err := tg.OpenSession(sessionPath)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = storage.Close() }()

	// One-time preparation (clean up ghost tasks + send the startup notification) happens in
	// the client.Run callback: the startup notification shows account information
	// (client.Self), and MTProto is only usable once the connection is ready, so calling it
	// before Run would only report "not wired" - and the user would think login failed.
	// attempt==1 guarantees the notification is not sent again on reconnects.

	// When cache_dialogs_hours <= 0, tg.NewPeerResolver falls back to its built-in default
	// (6 hours); it is not rewritten here, to avoid the implicit "config says 0 but it is
	// actually 1 hour" behaviour.
	ttl := time.Duration(cfg.Download.CacheDialogsHours) * time.Hour

	// Search result cache: built once outside the reconnect loop so a search confirmed right
	// before a reconnect is still downloadable afterwards (results are in-memory only anyway).
	searchStore := bot.NewSearchStore(0)

	// Reconnect loop: gotd's Run returns an error as soon as pingLoop decides the connection
	// is gone (observed as "task pingLoop: disconnect (pong missed)"). The old implementation
	// treated that as fatal and exited the process, so a brief network hiccup took the bot
	// offline for the whole night. Here it reconnects with exponential backoff and only exits
	// for real on SIGINT/SIGTERM (ctx cancellation).
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}

		connectedAt := time.Now()
		// Run one connection attempt inside a closure so defer pool.Close() executes on every
		// reconnect cycle instead of accumulating until startBot returns.
		runErr, fatalErr := func() (error, error) {
			// Rebuild the client on every reconnect: a gotd Client cannot be reused after it is
			// closed (c.ctx is already Done).
			client, cerr := tg.New(cfg, storage)
			if cerr != nil {
				return nil, fmt.Errorf("build Telegram client: %w", cerr)
			}
			api := client.API()
			resolver, rerr := tg.NewPeerResolver(api, st, ttl)
			if rerr != nil {
				return nil, fmt.Errorf("build peer resolver: %w", rerr)
			}
			pipeline, perr := tg.NewPipeline(api, resolver)
			if perr != nil {
				return nil, fmt.Errorf("build resolve pipeline: %w", perr)
			}
			// Multi-DC connection pool for speed (aligned with tdl core/dcpool): every DC keeps its
			// own pool, which gets past the single-connection rate limit and connects straight to
			// the DC that holds the file.
			poolSize := int64(cfg.Download.Threads * cfg.Download.FileConcurrency)
			if poolSize < 8 {
				poolSize = 8
			}
			if poolSize > 32 {
				poolSize = 32
			}
			pool := client.NewPool(poolSize)
			defer func() { _ = pool.Close() }()

			// getFileRPC is satisfied by the *gotd API client (tg.Client in the internal package is
			// an alias of the API client).
			runner, nerr := tg.NewTaskRunner(api, logger, tg.RunnerConfig{
				RootDir:         cfg.DownloadPath,
				Threads:         cfg.Download.Threads,
				FileConcurrency: cfg.Download.FileConcurrency,
				SkipSame:        cfg.Download.SkipSame,
				KeepParts:       cfg.Download.KeepParts,
				Pool:            pool,
				Takeout:         cfg.Download.Takeout,
			})
			if nerr != nil {
				return nil, fmt.Errorf("build download orchestrator: %w", nerr)
			}
			searcher, serr := tg.NewSearcher(api, resolver)
			if serr != nil {
				return nil, fmt.Errorf("build media searcher: %w", serr)
			}

			b.SetDownload(newBotDownloader(cfg, pipeline, runner, searchStore, logger))
			b.SetSearchMedia(newBotSearcher(searcher), searchStore)
			b.SetSync(func(c context.Context, full bool) (int, error) {
				return resolver.SyncDialogs(c, full)
			})
			b.SetAccount(func(c context.Context) (int64, string, int, error) {
				return client.Self(c)
			})
			// Login wizard: phone number, login code, 2FA, all through real MTProto.
			b.SetLoginAPI(func(context.Context) (bot.LoginAPI, error) {
				return tgLoginAPI{client: client}, nil
			})
			// /logout: delete the session file. The current process still holds the connection, so a
			// restart is needed to be truly logged out.
			b.SetLogout(func(context.Context) error {
				if err := os.Remove(sessionPath); err != nil && !os.IsNotExist(err) {
					return err
				}
				return nil
			})

			// Takeout session (enabled by download.takeout=true).
			// init must run inside the client.Run callback (only then is the MTProto connection
			// ready); finish is hooked after the callback returns and uses its own ctx so the exit
			// path can still run it.
			var takeoutID int64

			err := client.Run(ctx, func(cctx context.Context) error {
				// Connection is ready: only now can the startup notification carry real account info.
				if attempt == 1 {
					b.Startup(cctx)
				}

				// takeout session initialization (a failure does not block bot startup; downloads
				// fall back to a regular session for this round).
				if cfg.Download.Takeout {
					id, terr := tg.InitTakeoutSession(cctx, client.API(), 4000)
					if terr != nil {
						logger.Warn("init takeout session failed, downloads fall back to a regular session this round", zap.Error(terr))
					} else {
						takeoutID = id
						logger.Info("takeout session ready", zap.Int64("takeout_id", id))
						defer func() {
							finCtx, finCancel := context.WithTimeout(context.Background(), 15*time.Second)
							defer finCancel()
							if ferr := tg.FinishTakeoutSession(finCtx, client.API(), takeoutID); ferr != nil {
								logger.Warn("finish takeout session", zap.Error(ferr))
							} else {
								logger.Info("takeout session finished", zap.Int64("takeout_id", takeoutID))
							}
						}()
					}
				}

				return b.Run(cctx)
			})
			return err, nil
		}()
		if fatalErr != nil {
			return fatalErr
		}
		if ctx.Err() != nil {
			// Clean shutdown (Ctrl-C / SIGTERM), not a failure.
			return nil
		}
		if time.Since(connectedAt) > time.Minute {
			backoff = time.Second
		}

		logger.Warn("Telegram connection lost, reconnecting",
			zap.Int("attempt", attempt), zap.Duration("backoff", backoff), zap.Error(runErr))

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		if backoff < 60*time.Second {
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
		}
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.SetOutput(os.Stderr)
		log.Println("Error:", err)
		os.Exit(1)
	}
}
