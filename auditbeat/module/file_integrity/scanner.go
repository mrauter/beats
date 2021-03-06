package file_integrity

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/juju/ratelimit"

	"github.com/elastic/beats/libbeat/logp"
)

// scannerID is used as a global monotonically increasing counter for assigning
// a unique name to each scanner instance for logging purposes. Use
// atomic.AddUint32() to get a new value.
var scannerID uint32

type scanner struct {
	fileCount   uint64
	byteCount   uint64
	tokenBucket *ratelimit.Bucket

	done   <-chan struct{}
	eventC chan Event

	log    *logp.Logger
	config Config
}

// NewFileSystemScanner creates a new EventProducer instance that scans the
// configured file paths.
func NewFileSystemScanner(c Config) (EventProducer, error) {
	return &scanner{
		log:    logp.NewLogger(moduleName).With("scanner_id", atomic.AddUint32(&scannerID, 1)),
		config: c,
		eventC: make(chan Event, 1),
	}, nil
}

// Start starts the EventProducer. The provided done channel can be used to stop
// the EventProducer prematurely. The returned Event channel will be closed when
// scanning is complete. The channel must drained otherwise the scanner will
// block.
func (s *scanner) Start(done <-chan struct{}) (<-chan Event, error) {
	s.done = done

	if s.config.ScanRateBytesPerSec > 0 {
		s.log.With(
			"bytes_per_sec", s.config.ScanRateBytesPerSec,
			"capacity_bytes", s.config.MaxFileSizeBytes).
			Debugf("Creating token bucket with rate %v/sec and capacity %v",
				s.config.ScanRatePerSec,
				s.config.MaxFileSize)

		s.tokenBucket = ratelimit.NewBucketWithRate(
			float64(s.config.ScanRateBytesPerSec)/2., // Fill Rate
			int64(s.config.MaxFileSizeBytes))         // Max Capacity
		s.tokenBucket.TakeAvailable(math.MaxInt64)
	}

	go s.scan()
	return s.eventC, nil
}

// scan iterates over the configured paths and generates events for each file.
func (s *scanner) scan() {
	s.log.Debugw("File system scanner is starting", "file_path", s.config.Paths)
	defer s.log.Debug("File system scanner is stopping")
	defer close(s.eventC)
	startTime := time.Now()

	for _, path := range s.config.Paths {
		// Resolve symlinks to ensure we have an absolute path.
		evalPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			s.log.Warnw("Failed to scan", "file_path", path, "error", err)
			continue
		}

		if err = s.walkDir(evalPath); err != nil {
			s.log.Warnw("Failed to scan", "file_path", evalPath, "error", err)
		}
	}

	duration := time.Since(startTime)
	byteCount := atomic.LoadUint64(&s.byteCount)
	fileCount := atomic.LoadUint64(&s.fileCount)
	s.log.Infow("File system scan completed",
		"took", duration,
		"file_count", fileCount,
		"total_bytes", byteCount,
		"bytes_per_sec", float64(byteCount)/float64(duration)*float64(time.Second),
		"files_per_sec", float64(fileCount)/float64(duration)*float64(time.Second),
	)
}

func (s *scanner) walkDir(dir string) error {
	errDone := errors.New("done")
	startTime := time.Now()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if !os.IsNotExist(err) {
				s.log.Warnw("Scanner is skipping a path because of an error",
					"file_path", path, "error", err)
			}
			return nil
		}

		if s.config.IsExcludedPath(path) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		defer func() { startTime = time.Now() }()

		event := s.newScanEvent(path, info, err)
		event.rtt = time.Since(startTime)
		select {
		case s.eventC <- event:
		case <-s.done:
			return errDone
		}

		// Throttle reading and hashing rate.
		if event.Info != nil && len(event.Hashes) > 0 {
			s.throttle(event.Info.Size)
		}

		// Always traverse into the start dir.
		if !info.IsDir() || dir == path {
			return nil
		}

		// Only step into directories if recursion is enabled.
		// Skip symlinks to dirs.
		m := info.Mode()
		if !s.config.Recursive || m&os.ModeSymlink > 0 {
			return filepath.SkipDir
		}

		return nil
	})
	if err == errDone {
		err = nil
	}
	return err
}

func (s *scanner) throttle(fileSize uint64) {
	if s.tokenBucket == nil {
		return
	}

	wait := s.tokenBucket.Take(int64(fileSize))
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-s.done:
		}
	}
}

func (s *scanner) newScanEvent(path string, info os.FileInfo, err error) Event {
	event := NewEventFromFileInfo(path, info, err, None, SourceScan,
		s.config.MaxFileSizeBytes, s.config.HashTypes)

	// Update metrics.
	atomic.AddUint64(&s.fileCount, 1)
	if event.Info != nil {
		atomic.AddUint64(&s.byteCount, event.Info.Size)
	}
	return event
}
