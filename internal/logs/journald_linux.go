//go:build linux

package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
)

// journaldTailer ships systemd journal entries by exec'ing journalctl. It is the
// second and last subprocess the agent runs (internal/process execs ps on macOS),
// confined to Linux because journalctl exists only there. The journal has no
// cgo-free read API, so this mirrors the stock Agent's sdjournal tailer over the
// journalctl JSON output instead. Each delivered entry's cursor is stored so a
// restart resumes exactly where it left off (at-least-once, like the file tailer).
type journaldTailer struct {
	source   config.LogSource
	id       string // "journald:" + path or "default"
	out      chan<- Message
	registry *Registry
	proc     *processor    // log_processing_rules, nil when none configured
	pause    time.Duration // between respawns, so a failing journalctl cannot fork-loop
	log      *slog.Logger
}

// startJournald launches a journald tailer for src as a member of wg, writing to
// out. journald has no globbing, so it runs once per source for the agent's life.
func startJournald(ctx context.Context, src config.LogSource, out chan<- Message, reg *Registry, wg *sync.WaitGroup, log *slog.Logger) {
	t := &journaldTailer{
		source:   src,
		id:       "journald:" + journaldID(src),
		out:      out,
		registry: reg,
		pause:    time.Second,
		log:      log,
	}
	if len(src.LogProcessingRules) > 0 {
		t.proc = newProcessor(src.LogProcessingRules, 0, log)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t.run(ctx)
	}()
	log.Info("tailing journald", "id", t.id, "service", src.Service, "source", src.Source)
}

// journaldID names the registry key for a source: its journal path when set, else
// "default", matching the stock Agent's Identifier.
func journaldID(src config.LogSource) string {
	if src.Path != "" {
		return src.Path
	}
	return "default"
}

// run reads journalctl until ctx is done, respawning it when it exits so a
// journal rotation or transient error does not end tailing. The pause between
// respawns keeps a persistently failing journalctl (a bad match argument, an
// unreadable journal) from becoming a fork loop.
func (t *journaldTailer) run(ctx context.Context) {
	for {
		err := t.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		t.log.Warn("journalctl exited, restarting", "id", t.id, "err", err)
		if !sleep(ctx, t.pause) {
			return
		}
	}
}

// stream runs one journalctl process and forwards its entries. It resumes from
// the stored cursor and returns when the process exits or ctx is cancelled. A
// nonzero exit comes back as an error carrying journalctl's last words on
// stderr, so the respawn warning says why.
func (t *journaldTailer) stream(ctx context.Context) error {
	cursor, _ := t.registry.PositionCursor(t.id)
	cmd := exec.CommandContext(ctx, "journalctl", journalctlArgs(t.source, cursor)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	r := bufio.NewReaderSize(stdout, 1<<16)
	var readErr error
	for readErr == nil {
		var line []byte
		line, readErr = r.ReadBytes('\n')
		if len(line) > 0 && !t.handle(ctx, line) {
			break // shutdown mid-stream: no later entry may outrun a dropped one
		}
	}
	switch waitErr := cmd.Wait(); {
	case waitErr != nil:
		if s := stderr.String(); s != "" {
			return fmt.Errorf("%w: %s", waitErr, s)
		}
		return waitErr
	case readErr == nil || errors.Is(readErr, io.EOF):
		return nil
	default:
		return readErr
	}
}

// tailBuffer keeps the tail of what is written to it, enough to carry
// journalctl's complaint into the respawn warning without growing unbounded.
type tailBuffer struct {
	b []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if max := 512; len(t.b) > max {
		t.b = t.b[len(t.b)-max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.b)) }

// handle decodes one journalctl line and sends a Message. A non-JSON line (an
// occasional journalctl notice) or an entry dropped by the source's excludes or
// processing rules is skipped. It reports false only when ctx ended and the
// entry was dropped undelivered, so the caller stops the stream: a later entry's
// cursor must never be stored past a dropped one.
func (t *journaldTailer) handle(ctx context.Context, line []byte) bool {
	fields, cursor, realtime, err := parseEntry(line)
	if err != nil {
		return true
	}
	if shouldDrop(t.source, fields) {
		return true
	}
	body := buildBody(fields)
	if t.proc != nil {
		processed, _, ok := t.proc.finish(body, 0)
		if !ok {
			return true
		}
		body = processed
	}
	app := deriveService(fields)
	service := t.source.Service
	if service == "" {
		service = app
	}
	source := t.source.Source
	if source == "" {
		source = app
	}
	msg := Message{
		Content:  body,
		Status:   priorityStatus(fields["PRIORITY"]),
		Service:  service,
		Source:   source,
		Tags:     t.source.Tags,
		sourceID: t.id,
		cursor:   cursor,
	}
	if realtime > 0 {
		msg.Timestamp = time.UnixMicro(realtime)
	}
	select {
	case t.out <- msg:
		return true
	default:
	}
	select {
	case t.out <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}
