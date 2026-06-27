package logs

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/0intro/dd-micro-agent/internal/config"
)

const (
	readChunk        = 32 * 1024
	defaultPollDelay = time.Second

	// maxLineBytes caps how much of a single line is buffered while waiting for
	// its newline, the stock Agent's limit. A line that outgrows it ships as-is,
	// so a file that never writes a newline cannot grow memory without bound.
	maxLineBytes = 256 * 1024
)

// tailer follows one file: it reads complete lines, emits them as Messages
// carrying their end offset, and handles rotation (the path now points at a new
// inode) and truncation (the file shrank below our offset) by reopening from the
// start. It reads to EOF, then polls. A file with no registry entry is started at
// the end (like the stock Agent's default start_position), so the agent never
// backfills pre-existing history. Known files resume from their stored offset.
type tailer struct {
	source   config.LogSource
	id       string // "file:" + absolute path
	path     string // absolute path
	out      chan<- Message
	registry *Registry
	poll     time.Duration
	log      *slog.Logger
	proc     *processor // log_processing_rules (multiline + mask/exclude/include)
}

func (t *tailer) run(ctx context.Context) {
	t.proc = newProcessor(t.source.LogProcessingRules, t.poll, t.log)
	var (
		f              *os.File
		offset, resume = t.registry.Position(t.id) // resume=false: new file, tail from end
		partial        []byte
		buf            = make([]byte, readChunk)
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	for {
		if f == nil {
			file, err := os.Open(t.path)
			if err != nil {
				if !sleep(ctx, t.poll) { // file not present yet, wait and retry
					return
				}
				continue
			}
			f = file
			if !resume {
				// New to the registry: start at the end so pre-existing history isn't
				// shipped. From here on we resume by offset (a rotation resets to 0).
				offset = fileSize(f)
				resume = true
			} else if size := fileSize(f); offset > size {
				offset = 0 // truncated while we weren't watching
			}
			f.Seek(offset, io.SeekStart)
			partial = partial[:0]
			t.log.Debug("tailing from", "path", t.path, "offset", offset)
		}

		// Read everything currently available, emitting complete lines.
		for {
			n, err := f.Read(buf)
			if n > 0 {
				var ok bool
				if offset, ok = t.emit(ctx, buf[:n], offset, &partial); !ok {
					return // shutdown mid-emit: stop so no later line outruns a dropped one
				}
			}
			if err != nil { // io.EOF (caught up) or a read error
				break
			}
		}

		// Ship a buffered multiline message once it has been idle for the flush timeout
		// (no-op unless a multi_line rule is active and a message is pending).
		if content, off, ok := t.proc.flush(time.Now(), false); ok {
			if !t.send(ctx, content, off) {
				return
			}
		}

		if t.rotated(f, offset) {
			// The path now points at a new file: force out the old one's unterminated
			// last line and buffered multiline message before its bytes are gone.
			if len(partial) > 0 {
				offset += int64(len(partial))
				if content, off, ok := t.proc.line(partial, offset, time.Now()); ok {
					if !t.send(ctx, content, off) {
						return
					}
				}
				partial = partial[:0]
			}
			if content, off, ok := t.proc.flush(time.Now(), true); ok {
				if !t.send(ctx, content, off) {
					return
				}
			}
			f.Close()
			f = nil
			offset = 0
			continue
		}

		if !sleep(ctx, t.poll) {
			// Shutdown: a still-buffered multiline message is left unsent on purpose.
			// Its bytes were read but the offset isn't advanced until delivery, so a
			// restart re-reads and re-aggregates it (at-least-once holds).
			return
		}
	}
}

// emit appends data to partial, sends each complete line, and returns the new
// offset, which lands on a newline boundary so a restart resumes cleanly. The
// lone exception is a line past maxLineBytes, shipped unterminated so the buffer
// stays bounded. Empty lines are skipped, except under a multi_line rule, where
// they belong to the aggregate. ok=false means ctx ended and a line was dropped,
// so the caller must stop reading.
func (t *tailer) emit(ctx context.Context, data []byte, offset int64, partial *[]byte) (_ int64, ok bool) {
	*partial = append(*partial, data...)
	for {
		i := bytes.IndexByte(*partial, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimRight((*partial)[:i], "\r")
		offset += int64(i + 1)
		*partial = (*partial)[i+1:]
		if len(line) == 0 && !t.proc.aggregating() {
			continue
		}
		if content, off, ok := t.proc.line(line, offset, time.Now()); ok {
			if !t.send(ctx, content, off) {
				return offset, false
			}
		}
	}
	if len(*partial) > maxLineBytes {
		offset += int64(len(*partial))
		if content, off, ok := t.proc.line(*partial, offset, time.Now()); ok {
			if !t.send(ctx, content, off) {
				return offset, false
			}
		}
		*partial = (*partial)[:0]
	}
	return offset, true
}

// send delivers one line. The content is copied because partial's backing array
// is reused. offset is the position just past this line. Delivery is preferred
// over shutdown while the batcher has room, and ok=false reports a drop (ctx
// ended, channel full) so the caller stops: a later line must never outrun a
// dropped one, or its delivery would advance the registry past undelivered bytes.
func (t *tailer) send(ctx context.Context, line []byte, offset int64) bool {
	msg := Message{
		Content:   append([]byte(nil), line...),
		Timestamp: time.Now(),
		Service:   t.source.Service,
		Source:    t.source.Source,
		Tags:      t.source.Tags,
		sourceID:  t.id,
		offset:    offset,
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

// rotated reports whether the file should be reopened: the path is gone, now
// resolves to a different inode, or was truncated below our read offset.
func (t *tailer) rotated(f *os.File, offset int64) bool {
	pathInfo, err := os.Stat(t.path)
	if err != nil {
		return true // rotated away or removed
	}
	fdInfo, err := f.Stat()
	if err != nil {
		return true
	}
	return !os.SameFile(pathInfo, fdInfo) || pathInfo.Size() < offset
}

func fileSize(f *os.File) int64 {
	if info, err := f.Stat(); err == nil {
		return info.Size()
	}
	return 0
}

// sleep waits for d or until ctx is done, reporting false if ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
