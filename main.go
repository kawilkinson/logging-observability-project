package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kawilkinson/linko/internal/build"
	"kawilkinson/linko/internal/linkoerr"
	"kawilkinson/linko/internal/store"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closeLogger, err := initializeLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close logger: %v\n", err)
		}
	}()

	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}

	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger.Debug("Linko is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}

type closeFunc func() error

func initializeLogger() (*slog.Logger, closeFunc, error) {
	linkoLogs, exists := os.LookupEnv("LINKO_LOG_FILE")
	if exists {
		logFile, err := os.OpenFile(linkoLogs, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open log file: %w", err)
		}
		bufferedFile := bufio.NewWriterSize(logFile, 8192)

		noColor := true
		if isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()) {
			noColor = false
		}
		
		debugHandler := tint.NewTextHandler(os.Stderr, &tint.Options{
			ReplaceAttr: replaceAttr,
			Level: slog.LevelDebug,
			NoColor: noColor,
		})
		infoHandler := slog.NewJSONHandler(bufferedFile, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
			Level: slog.LevelInfo,
		})

		logger := slog.New(slog.NewMultiHandler(debugHandler, infoHandler))

		close := func() error {
			if err := bufferedFile.Flush(); err != nil {
				return fmt.Errorf("failed to flush log buffer: %w", err)
			}
			if err := logFile.Close(); err != nil {
				return fmt.Errorf("failed to close log file: %w", err)
			}
			return nil
		}
		return logger, close, nil
	}
	close := func() error {
		return nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil)), close, nil
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		if multiErr, ok := errors.AsType[multiError](err); ok {
			errors := multiErr.Unwrap()
			attrs := []slog.Attr{}
			for i, e := range errors {
				attrs = append(attrs, slog.Attr {
					Key: fmt.Sprintf("error_%d", i+1),
					Value: slog.AnyValue(errorAttrs(e)),
				})
			}
			return slog.GroupAttrs("errors", attrs...)
		}
		
		attrs := errorAttrs(err)
		return slog.GroupAttrs("error", attrs...)
	}
	return a
}

func errorAttrs(err error) []slog.Attr {
		attrs := []slog.Attr{
			{
				Key: "message",
				Value: slog.StringValue(err.Error()),
			},
		}

		attrs = append(attrs, linkoerr.Attrs(err)...)

		if stackErr, ok := errors.AsType[stackTracer](err); ok {
			attrs = append(attrs, slog.Attr {
				Key: "stack_trace",
				Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
			})
		}
		return attrs
}
