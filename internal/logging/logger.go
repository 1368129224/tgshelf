package logging

import (
	"fmt"
	"io"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Options holds the configuration options of the Logger.
type Options struct {
	Debug       bool
	LogFilePath string   // log file path; empty means no file output
	MaxBytes    int64    // maximum bytes per log file
	MaxBackups  int      // maximum number of backup files
	Secrets     []string // sensitive strings to redact exactly (e.g. token/api_hash)
}

// New creates a *zap.Logger wired up with redaction, rotation and console output.
// The returned io.Closer closes the log file at process exit (may be nil).
func New(opts Options) (*zap.Logger, io.Closer, error) {
	redacter := NewRedacter(opts.Secrets...)

	// 1. console core (writes to stderr)
	var consoleEncoder zapcore.Encoder
	if opts.Debug {
		devCfg := zap.NewDevelopmentEncoderConfig()
		devCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		consoleEncoder = zapcore.NewConsoleEncoder(devCfg)
	} else {
		prodCfg := zap.NewProductionEncoderConfig()
		prodCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		consoleEncoder = zapcore.NewJSONEncoder(prodCfg)
	}

	level := zapcore.InfoLevel
	if opts.Debug {
		level = zapcore.DebugLevel
	}

	consoleCore := zapcore.NewCore(consoleEncoder, zapcore.Lock(os.Stderr), level)

	var (
		cores  = []zapcore.Core{consoleCore}
		closer io.Closer
	)

	// 2. file rotation core (when a log path is set)
	if opts.LogFilePath != "" {
		rotator, err := NewRotateWriter(opts.LogFilePath, opts.MaxBytes, opts.MaxBackups)
		if err != nil {
			return nil, nil, fmt.Errorf("init log rotation: %w", err)
		}
		closer = rotator

		fileCfg := zap.NewProductionEncoderConfig()
		fileCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		fileEncoder := zapcore.NewJSONEncoder(fileCfg)

		fileCore := zapcore.NewCore(fileEncoder, zapcore.AddSync(rotator), level)
		cores = append(cores, fileCore)
	}

	// 3. wrap every core with the redacting filter
	tee := zapcore.NewTee(cores...)
	redactingCore := WrapCore(tee, redacter)

	logger := zap.New(redactingCore, zap.AddCaller())
	return logger, closer, nil
}

// TaskFields builds the standard structured log fields associated with a download task.
func TaskFields(taskID, link, tag string) []zap.Field {
	return []zap.Field{
		zap.String("task_id", taskID),
		zap.String("link", link),
		zap.String("tag", tag),
	}
}
