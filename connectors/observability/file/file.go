package file

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is this connector's name in observability.json.
const Driver = "file"

const (
	version      = "1.0.0"
	dayLayout    = "2006-01-02"
	rotationDay  = "daily"
	rotationNone = "none"
	dirMode      = 0o750
	fileMode     = 0o640
	maxLine      = 16 << 20 // a longer line is corrupt: the redactor bounds strings
	defaultPage  = 100
	maxPage      = 1000
)

// Parameters is the sink's `parameters` object (§7.4).
type Parameters struct {
	Path string `json:"path"`
	// Rotation is "daily", the default, or "none".
	Rotation string `json:"rotation,omitempty"`
	// RetentionDays deletes rotated files older than this many days. Zero means
	// config.DefaultLogDays.
	RetentionDays int `json:"retentionDays,omitempty"`
}

// Options are test seams.
type Options struct {
	Now func() time.Time
}

// Register adds the driver.
func Register(r connector.Registrar) {
	r.RegisterLogSink(connector.Info{
		Driver:       Driver,
		Version:      version,
		Available:    true,
		Capabilities: map[string]bool{"query": true, "rotation": true},
	}, Factory)
}

// Factory opens a sink from its configuration entry.
func Factory(_ context.Context, cfg config.Sink, _ connector.SecretResolver) (connector.LogSink, error) {
	var params Parameters
	if len(cfg.Parameters) > 0 {
		if err := json.Unmarshal(cfg.Parameters, &params); err != nil {
			return nil, errs.Wrap(errs.CodeValidationFailed, err, "invalid file sink parameters")
		}
	}
	return New(params, Options{})
}

// Sink writes one file and rotates it. Writes are serialized by the caller's pump
// and by a mutex here; queries read files independently and never block a write.
type Sink struct {
	params Parameters
	now    func() time.Time
	stem   string
	ext    string
	dir    string
	active string
	rotRe  *regexp.Regexp

	mu    sync.Mutex
	f     *os.File
	day   string // UTC day of the active file's records
	dirty bool   // the last write failed, so the next one starts a new line
}

var (
	_ connector.LogSink    = (*Sink)(nil)
	_ connector.LogQuerier = (*Sink)(nil)
)

// New opens or creates the file, rotating leftovers of a previous day.
func New(params Parameters, opts Options) (*Sink, error) {
	if params.Path == "" || !filepath.IsAbs(params.Path) {
		return nil, errs.New(errs.CodeValidationFailed, "file sink needs an absolute path")
	}
	switch params.Rotation {
	case "":
		params.Rotation = rotationDay
	case rotationDay, rotationNone:
	default:
		return nil, errs.Newf(errs.CodeValidationFailed, "unknown file sink rotation %q", params.Rotation)
	}
	if params.RetentionDays < 0 {
		return nil, errs.New(errs.CodeValidationFailed, "retentionDays cannot be negative")
	}
	if params.RetentionDays == 0 {
		params.RetentionDays = config.DefaultLogDays
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	base := filepath.Base(params.Path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	s := &Sink{
		params: params,
		now:    now,
		stem:   stem,
		ext:    ext,
		dir:    filepath.Dir(params.Path),
		active: params.Path,
		rotRe:  regexp.MustCompile("^" + regexp.QuoteMeta(stem) + `-(\d{4}-\d{2}-\d{2})(?:-(\d+))?` + regexp.QuoteMeta(ext) + "$"),
	}
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openActive(); err != nil {
		return nil, err
	}
	return s, nil
}

// Info implements connector.LogSink.
func (s *Sink) Info() connector.Info {
	return connector.Info{
		Family:       connector.FamilyLogSink,
		Driver:       Driver,
		Version:      version,
		Available:    true,
		Capabilities: map[string]bool{"query": true, "rotation": true},
	}
}

func (s *Sink) today() string { return s.now().UTC().Format(dayLayout) }

func (s *Sink) openActive() error {
	today := s.today()
	if info, err := os.Stat(s.active); err == nil && info.Size() > 0 && s.params.Rotation == rotationDay {
		if day := info.ModTime().UTC().Format(dayLayout); day != today {
			if err := s.renameRotated(day); err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(s.active, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	s.f = f
	s.day = today
	s.sweep(today)
	return nil
}

// renameRotated moves the active file aside, under the name of the day its records
// belong to.
func (s *Sink) renameRotated(day string) error {
	target := filepath.Join(s.dir, s.stem+"-"+day+s.ext)
	for n := 1; ; n++ {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		target = filepath.Join(s.dir, s.stem+"-"+day+"-"+strconv.Itoa(n)+s.ext)
	}
	if err := os.Rename(s.active, target); err != nil {
		return fmt.Errorf("rotate log file: %w", err)
	}
	return nil
}

func (s *Sink) rotateIfNeeded() error {
	if s.params.Rotation != rotationDay {
		return nil
	}
	today := s.today()
	if today == s.day {
		return nil
	}
	if s.f != nil {
		_ = s.f.Sync()
		_ = s.f.Close()
		s.f = nil
	}
	if info, err := os.Stat(s.active); err == nil && info.Size() > 0 {
		if err := s.renameRotated(s.day); err != nil {
			return err
		}
	}
	return s.openActive()
}

// sweep deletes rotated files past retention. A failure here must not stop logging,
// and the next rotation tries again.
func (s *Sink) sweep(today string) {
	cutoffDay, err := time.Parse(dayLayout, today)
	if err != nil {
		return
	}
	cutoff := cutoffDay.AddDate(0, 0, -s.params.RetentionDays)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		m := s.rotRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		day, err := time.Parse(dayLayout, m[1])
		if err == nil && day.Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

// Write implements connector.LogSink. A batch is one write call.
func (s *Sink) Write(_ context.Context, batch []connector.LogRecord) error {
	if len(batch) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for i := range batch {
		line, err := json.Marshal(&batch[i])
		if err != nil {
			continue // the redactor already made fields encodable; never fail a batch for one record
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotateIfNeeded(); err != nil {
		return err
	}
	if s.f == nil {
		if err := s.openActive(); err != nil {
			return err
		}
	}
	data := buf.Bytes()
	if s.dirty {
		data = append([]byte{'\n'}, data...)
	}
	if _, err := s.f.Write(data); err != nil {
		s.dirty = true
		return fmt.Errorf("write log file: %w", err)
	}
	s.dirty = false
	return nil
}

// Flush implements connector.LogSink.
func (s *Sink) Flush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	return s.f.Sync()
}

// Close implements connector.LogSink.
func (s *Sink) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	syncErr := s.f.Sync()
	closeErr := s.f.Close()
	s.f = nil
	return errors.Join(syncErr, closeErr)
}

// ---- Query ----

type logFile struct {
	path   string
	day    string
	n      int
	active bool
}

// cursor is a position: records strictly before Offset in that file come next.
// N is -1 for the active file, so a rotation between two pages can still find it.
type cursor struct {
	Day    string `json:"d"`
	N      int    `json:"n"`
	Offset int64  `json:"o"`
}

func encodeCursor(c cursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(text string) (cursor, error) {
	var c cursor
	data, err := base64.RawURLEncoding.DecodeString(text)
	if err == nil {
		err = json.Unmarshal(data, &c)
	}
	if err != nil {
		return c, errs.New(errs.CodeInvalidArgument, "invalid log cursor")
	}
	return c, nil
}

func (s *Sink) listFiles() ([]logFile, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var files []logFile
	for _, e := range entries {
		m := s.rotRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n := 0
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		files = append(files, logFile{path: filepath.Join(s.dir, e.Name()), day: m[1], n: n})
	}
	sort.Slice(files, func(a, b int) bool {
		if files[a].day != files[b].day {
			return files[a].day < files[b].day
		}
		return files[a].n < files[b].n
	})
	s.mu.Lock()
	day := s.day
	s.mu.Unlock()
	if _, err := os.Stat(s.active); err == nil {
		files = append(files, logFile{path: s.active, day: day, n: -1, active: true})
	}
	return files, nil
}

func levelRank(level string) int {
	switch strings.ToLower(level) {
	case "debug":
		return 1
	case "info", "":
		return 2
	case "warn", "warning":
		return 3
	case "error":
		return 4
	}
	return 2
}

func matches(rec *connector.LogRecord, q *connector.LogQuery, minRank int) bool {
	if q.TenantID != "" && rec.TenantID != q.TenantID {
		return false
	}
	if q.JobID != "" && rec.JobID != q.JobID {
		return false
	}
	if minRank > 0 && levelRank(rec.Level) < minRank {
		return false
	}
	if !q.Since.IsZero() && rec.Timestamp.Before(q.Since) {
		return false
	}
	return true
}

type positioned struct {
	rec    connector.LogRecord
	file   logFile
	offset int64 // where this record's line starts
}

// QueryLogs implements connector.LogQuerier: newest first, with memory bounded by the
// page size and time linear in the size of the files read (I275).
func (s *Sink) QueryLogs(ctx context.Context, q connector.LogQuery) (connector.LogPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	minRank := 0
	if q.MinLevel != "" {
		minRank = levelRank(q.MinLevel)
	}
	var start *cursor
	if q.Cursor != "" {
		c, err := decodeCursor(q.Cursor)
		if err != nil {
			return connector.LogPage{}, err
		}
		start = &c
	}
	files, err := s.listFiles()
	if err != nil {
		return connector.LogPage{}, fmt.Errorf("list log files: %w", err)
	}
	sinceDay := ""
	if !q.Since.IsZero() {
		sinceDay = q.Since.UTC().Format(dayLayout)
	}
	collected := make([]positioned, 0, limit+1)
	for i := len(files) - 1; i >= 0 && len(collected) <= limit; i-- {
		file := files[i]
		upTo := int64(-1)
		if start != nil {
			switch cursorPlace(file, *start, files, i) {
			case placeAfter:
				continue
			case placeSame:
				upTo = start.Offset
			}
		}
		if sinceDay != "" && file.day < sinceDay {
			break
		}
		if err := ctx.Err(); err != nil {
			return connector.LogPage{}, err
		}
		found, err := scanFile(ctx, file, upTo, limit+1-len(collected), &q, minRank)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // rotated or swept while listing
			}
			return connector.LogPage{}, fmt.Errorf("read log file: %w", err)
		}
		collected = append(collected, found...)
	}
	page := connector.LogPage{}
	n := min(len(collected), limit)
	page.Records = make([]connector.LogRecord, n)
	for i := range n {
		page.Records[i] = collected[i].rec
	}
	if len(collected) > limit {
		last := collected[limit-1]
		page.NextCursor = encodeCursor(cursor{Day: last.file.day, N: last.file.n, Offset: last.offset})
	}
	return page, nil
}

type cursorPosition int

const (
	placeBefore cursorPosition = iota // this file is older than the cursor: read it whole
	placeSame
	placeAfter // newer than the cursor: skip it
)

func cursorPlace(file logFile, c cursor, files []logFile, index int) cursorPosition {
	cursorActive := c.N == -1
	if cursorActive {
		if file.active {
			if file.day == c.Day {
				return placeSame
			}
			return placeAfter // the file the cursor meant has rotated since
		}
		if file.day > c.Day {
			return placeAfter
		}
		if file.day < c.Day {
			return placeBefore
		}
		// Same day, rotated: the cursor's file became the last one of that day.
		if index+1 < len(files) && files[index+1].day == c.Day && !files[index+1].active {
			return placeAfter
		}
		return placeSame
	}
	if file.active {
		return placeAfter
	}
	switch {
	case file.day > c.Day, file.day == c.Day && file.n > c.N:
		return placeAfter
	case file.day == c.Day && file.n == c.N:
		return placeSame
	}
	return placeBefore
}

// scanFile returns up to keep matching records before offset upTo, newest first.
func scanFile(ctx context.Context, file logFile, upTo int64, keep int, q *connector.LogQuery, minRank int) ([]positioned, error) {
	f, err := os.Open(file.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64<<10)
	ring := make([]positioned, 0, keep)
	head := 0
	var offset int64
	for lineNo := 0; ; lineNo++ {
		if lineNo%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if upTo >= 0 && offset >= upTo {
			break
		}
		line, err := readLine(reader)
		if len(line) > 0 {
			start := offset
			offset += int64(len(line))
			var rec connector.LogRecord
			if json.Unmarshal(line, &rec) == nil && matches(&rec, q, minRank) {
				item := positioned{rec: rec, file: file, offset: start}
				if len(ring) < keep {
					ring = append(ring, item)
				} else {
					ring[head] = item
					head = (head + 1) % keep
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	out := make([]positioned, len(ring))
	for i := range ring {
		out[len(ring)-1-i] = ring[(head+i)%len(ring)]
	}
	return out, nil
}

// readLine returns one line with its newline. An oversized line comes back cut, which
// fails to decode and is skipped like any corrupt line.
func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(out)+len(chunk) <= maxLine {
			out = append(out, chunk...)
		}
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return out, err
		}
	}
}
