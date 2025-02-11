package output

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	zstd "github.com/valyala/gozstd"

	"github.com/AdRoll/baker"
)

const helpMsg = `This output writes serialized records into compressed files, gzip (.gz) or zstd
(.zst) depending on the file extension in PathString.

Generated files may be rotated if RotateInterval is set. PathString is used to
control the name of the generated files, it may contain placeholders. These
placeholders are evaluated each time a file is created, that is upon creation
of the output or everytime a rotation takes place.

Supported placeholders:
 - {{.Year}}      year at file creation, 4 digits (YYYY)
 - {{.Month}}     month number at file creation, 2 digits (MM)
 - {{.Day}}       day of the month at file creation, 2 digits (DD)
 - {{.Hour}}      hour at file creation in 24h format, 2 digits (HH)
 - {{.Minute}}    minute at file creation, 2 digits (MM)
 - {{.Second}}    second at file creation, 2 digits (SS)
 - {{.Index}}     index of the current output process (see [output.procs]), 4 digits long
 - {{.UUID}}      per-worker random UUID (v4 UUID), 36 chars long
 - {{.Rotation}}  rotation count, 6 digits long
 - {{.Field0}}    value of the first field provided in [output.fields] (only if present).
 
When choosing configuration values for your FileWriter, it's important to keep in mind
the following rules:

 1. A file should only ever be accessed by a single worker at a time.

If you use multiple output processes, you should use {{.Index}} or {{.UUID}} 
so that generated filenames are guaranteed to be different for each workers.

 2. Rotation should never generate the same path twice.
 
To avoid a file to be overwritten by its successor in the rotation, you should ensure
that 2 files generated at a distance of RotateInterval will have different filenames.
To ensure filenames are different, you should set RotateInterval to a duration that 
exceeds that of the time-based placeholder with the shortest span.

For example, the following is correct since it's the generated path is guaranteed to
be unique at each rotation:

    PathString = "/path/to/file-{{.Hour}}-{{.Minute}}.log.gz" 
    RotateInterval = 5m

However, this is not correct, since successive generations may generate the exact same 
path:

    PathString = "/path/to/file-{{.Hour}}-{{.Minute}}.log.gz" 
    RotateInterval = 1s

If you use RotateSize you should also probably include {{.Rotation}}, to generate different 
different paths:

    PathString = "/path/to/file-{{.Rotation}}.log.gz" 
    RotateSize = "128MB"

 3. Only use {{.Field0}} if you trust the records you consume.

By using {{.Field0}} the files produces will have a path containing whatever value
is found. It could contain characters that are not valid to appear in a path. That also
means that the number of files (and workers) depend on the cardinality of that field.`

var FileWriterDesc = baker.OutputDesc{
	Name:   "FileWriter",
	New:    NewFileWriter,
	Config: &FileWriterConfig{},
	Raw:    true,
	Help:   helpMsg,
}

type FileWriterConfig struct {
	PathString           string          `help:"Template describing names of the generated files. See top-level documentation for supported placeholders.."`
	RotateInterval       time.Duration   `help:"Time interval between 2 successive file rotations. -1 disables interval-based rotation." default:"60s"`
	RotateSize           baker.SizeBytes `help:"File size which when reached triggers a file rotation. Can be cumulated with RotateInterval. 0 to disable. Examples: 12000, 12KB, 1MB, 1MiB, etc." default:"0"`
	DiscardEmptyFiles    bool            `help:"By default, if no records have been received at the moment of rotation (see RotateInterval) then created files are empty. If true, then empty files are discarded." default:"false"`
	ZstdCompressionLevel int             `help:"Zstd compression level, ranging from 1 (best speed) to 19 (best compression)." default:"3"`
	ZstdWindowLog        int             `help:"Enable zstd long distance matching. Increase memory usage for both compressor/decompressor. If more than 27 the decompressor requires special treatment. 0:disabled." default:"0"`
}

func (cfg *FileWriterConfig) fillDefaults() {
	if cfg.PathString == "" {
		cfg.PathString = "/tmp/baker/ologs/logs/{{.Year}}/{{.Month}}/{{.Day}}/baker/{{.Year}}{{.Month}}{{.Day}}-{{.Hour}}{{.Minute}}{{.Second}}.{{.Index}}.log.gz"
	}

	switch cfg.RotateInterval {
	case -1:
		// no time-interval-based rotation
		cfg.RotateInterval = 0
	case 0:
		// default value
		cfg.RotateInterval = 60 * time.Second
	}

	if cfg.ZstdCompressionLevel == 0 {
		cfg.ZstdCompressionLevel = 3
	}
}

type FileWriter struct {
	// atomically-accessed, keep on top for 64-bit alignment.
	totaln int64

	Cfg *FileWriterConfig

	Fields []baker.FieldIndex

	workers map[string]*fileWorker
	index   int

	tmpl         *template.Template
	useReplField bool
}

func NewFileWriter(cfg baker.OutputParams) (baker.Output, error) {
	log.WithFields(log.Fields{"fn": "NewFileWriter", "idx": cfg.Index}).Info("Initializing")

	dcfg := cfg.DecodedConfig.(*FileWriterConfig)
	dcfg.fillDefaults()

	fw := &FileWriter{
		Cfg:          dcfg,
		Fields:       cfg.Fields,
		workers:      make(map[string]*fileWorker),
		index:        cfg.Index,
		useReplField: strings.Contains(dcfg.PathString, "{{.Field0}}"),
	}

	if fw.useReplField && len(cfg.Fields) == 0 {
		return nil, errors.New("if {{.Field0}} is given, at least one field must be given in [output.fields]")
	}

	// Compile the template. Trying to check the validity of the path without
	// creating it is futile as this is very much os-dependent, plus, if
	// PathString contains {{.Field0}}, then the generated path can very much
	// contains anything, but we'd only know this at runtime. So it's reasonable
	// to handle such errors in fileWorker.makePath.
	var err error
	if fw.tmpl, err = template.New("fileWorkerType").Parse(dcfg.PathString); err != nil {
		return nil, fmt.Errorf("FileWorker: invalid PathString template: %s", err)
	}

	return fw, nil
}

func (w *FileWriter) Run(input <-chan baker.OutputRecord, upch chan<- string) error {
	ctxlog := log.WithFields(log.Fields{"idx": w.index})
	ctxlog.Info("FileWriter ready to log")

	var err error

	for lldata := range input {
		wname := ""
		if w.useReplField {
			wname = lldata.Fields[0]
		}
		worker, ok := w.workers[wname]
		if !ok {
			// Unique UUID for the output processes
			uid := uuid.New().String()
			worker, err = newWorker(w.Cfg, w.tmpl, wname, w.index, uid, upch)
			if err != nil {
				// This error will be returned, but we'll try to cleanup the
				// potential other workers, not early exit.
				err = fmt.Errorf("FileWriter, can't create new worker: %s", err)
				break
			}
			w.workers[wname] = worker
		}

		worker.write(lldata.Record)

		atomic.AddInt64(&w.totaln, 1)
	}

	ctxlog.Info("FileWriter Terminating")

	// Concurrently close the workers, but with no more than 'NumCPU' goroutines.
	sem := make(chan struct{}, runtime.NumCPU())
	wg := sync.WaitGroup{}
	for i := range w.workers {
		i := i
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			err := w.workers[i].Close()
			if err != nil {
				ctxlog.WithError(err).Error("error when closing worker")
			}
		}()
	}

	wg.Wait()

	return err
}

func (w *FileWriter) Stats() baker.OutputStats {
	return baker.OutputStats{
		NumProcessedLines: atomic.LoadInt64(&w.totaln),
	}
}

func (w *FileWriter) CanShard() bool {
	return false
}

// fileWorker manages writes to a file and its periodic rotation.
type fileWorker struct {
	in   chan []byte
	done chan struct{}

	cfg            *FileWriterConfig
	useZstd        bool
	replFieldValue string
	index          int
	uid            string
	rotateIdx      int64
	writtenOnce    bool
}

const fileWorkerChunkBuffer = 128 * 1024

func newWorker(cfg *FileWriterConfig, tmpl *template.Template, replFieldValue string, index int, uid string, upch chan<- string) (*fileWorker, error) {
	ctxLog := log.WithFields(log.Fields{"output": "FileWriter", "idx": index})

	fw := &fileWorker{
		in:             make(chan []byte, 1),
		done:           make(chan struct{}),
		cfg:            cfg,
		replFieldValue: replFieldValue,
		index:          index,
		uid:            uid,
		rotateIdx:      0,
		useZstd:        strings.HasSuffix(cfg.PathString, ".zst") || strings.HasSuffix(cfg.PathString, ".zstd"),
		writtenOnce:    false,
	}

	curPath, err := fw.makePath(tmpl)
	if err != nil {
		return nil, err
	}
	curw, bytesWritten, err := fw.newFile(curPath)
	if err != nil {
		return nil, fmt.Errorf("can't create file: %v", err)
	}

	// Perform rotation. Close, upload and swap curw with a newly
	// created file, after evaluating the path template.
	rotate := func() {
		if cfg.DiscardEmptyFiles && fw.writtenOnce {
			// Do not rotate if nothing has been written.
			return
		}

		if err := curw.Close(); err != nil {
			ctxLog.WithError(err).WithField("current", curPath).Error("FileWriter worker error closing file")
		}

		upch <- curPath

		fw.rotateIdx++
		newPath, err := fw.makePath(tmpl)
		if err != nil {
			// TODO(arl): when sticky error will be in place, do not
			// log.Fatal here but set the sticky error instead.
			ctxLog.WithError(err).WithField("current", curPath).Fatal("FileWriter worker can't create file")
		}

		ctxLog.WithFields(log.Fields{"current": curPath, "new": newPath}).Info("FileWriter worker file rotation")
		if curw, bytesWritten, err = fw.newFile(newPath); err != nil {
			// TODO(arl): when sticky error will be in place, do not
			// log.Fatal here but set the sticky error instead.
			ctxLog.WithError(err).WithField("current", curPath).Fatal("FileWriter worker can't create file")
		}
		curPath = newPath
	}

	go func() {
		var (
			tick   <-chan time.Time
			ticker *time.Ticker
		)
		restartTicker := func() {
			if cfg.RotateInterval > 0 {
				if ticker != nil {
					// Actual restart? stop previous ticker to avoid resource leak.
					ticker.Stop()
				}
				ticker = time.NewTicker(cfg.RotateInterval)
				tick = ticker.C
			}
		}
		restartTicker()

		defer func() {
			if ticker != nil {
				ticker.Stop()
			}
			ctxLog.WithFields(log.Fields{"current": curPath}).Info("FileWriter worker terminating")

			// Close the last file and upload it.
			if err := curw.Close(); err != nil {
				ctxLog.WithError(err).WithField("current", curPath).Error("FileWriter worker error closing file")
			}
			upch <- curPath
			close(fw.done)
		}()

		for {
			select {
			case <-tick:
				rotate()

			case line, ok := <-fw.in:
				if !ok {
					return
				}
				if _, err := curw.Write(line); err != nil {
					log.WithError(err).Error("FileWriter worker error writing to file")
				}
				fw.writtenOnce = true

				const linesep = '\n'
				if _, err := curw.Write([]byte{linesep}); err != nil {
					log.WithError(err).Error("FileWriter worker error writing to file")
				}

				nwritten := bytesWritten()
				if cfg.RotateSize != 0 && nwritten > int64(cfg.RotateSize) {
					// Max size reached, we can rotate and reset the 'interval timer'.
					rotate()
					restartTicker()
				}
			}
		}
	}()

	return fw, nil
}

func (fw *fileWorker) newFile(path string) (io.WriteCloser, func() int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	fw.writtenOnce = false

	bufw := bufio.NewWriterSize(f, fileWorkerChunkBuffer)
	countw := newCountingWriter(bufw)

	var wc io.WriteCloser
	if fw.useZstd {
		zstdw := zstd.NewWriterParams(countw, &zstd.WriterParams{
			CompressionLevel: fw.cfg.ZstdCompressionLevel,
			WindowLog:        fw.cfg.ZstdWindowLog,
		})
		wc = makeWriteCloser(zstdw, zstdw.Close)
	} else {
		// Only way to for gzip.NewWriterLevel to fail is to pass an
		// incorrect compression level.
		wc, _ = gzip.NewWriterLevel(countw, gzip.BestSpeed)
	}

	// Close the writers (the order is important here):
	// (zstd|gzip).Writer -> bufio.Writer -> os.File
	close := func() error {
		if err := wc.Close(); err != nil {
			return fmt.Errorf("compression error: %s", err)
		}
		if err := bufw.Flush(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return nil
	}

	return makeWriteCloser(wc, close), countw.BytesWritten, nil
}

func (fw *fileWorker) write(req []byte) {
	fw.in <- req
}

func (fw *fileWorker) Close() error {
	close(fw.in)
	<-fw.done
	return nil
}

func (fw *fileWorker) makePath(tmpl *template.Template) (string, error) {
	now := time.Now().UTC()
	var buf bytes.Buffer

	replacementVars := map[string]string{
		"Index":    fmt.Sprintf("%04d", fw.index),
		"Year":     fmt.Sprintf("%04d", now.Year()),
		"Month":    fmt.Sprintf("%02d", now.Month()),
		"Day":      fmt.Sprintf("%02d", now.Day()),
		"Hour":     fmt.Sprintf("%02d", now.Hour()),
		"Minute":   fmt.Sprintf("%02d", now.Minute()),
		"Second":   fmt.Sprintf("%02d", now.Second()),
		"UUID":     fw.uid,
		"Rotation": fmt.Sprintf("%06d", fw.rotateIdx),
		"Field0":   fw.replFieldValue,
	}

	err := tmpl.Execute(&buf, replacementVars)
	if err != nil {
		panic(err.Error())
	}

	dir := filepath.Dir(buf.String())
	if _, err = os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0777); err != nil {
				return "", fmt.Errorf("can't create directory structure: %s", err)
			}
		} else {
			return "", fmt.Errorf("can't stat directory: %s", err)
		}
	}

	return buf.String(), nil
}

// makeWriteCloser converts an io.Writer and a Close function into a
// WriteCloser.
func makeWriteCloser(w io.Writer, close func() error) io.WriteCloser {
	return &writeCloser{Writer: w, close: close}
}

type writeCloser struct {
	io.Writer
	close func() error
}

func (wc *writeCloser) Close() error {
	return wc.close()
}

// countingWriter embeds a WriteCloser and exposes how many bytes have been
// written into it. It's not safe for multiple goroutines to write into a single
// countingWriter.
type countingWriter struct {
	io.Writer
	count int64
}

// newCountingWriter function create new countingWriter
func newCountingWriter(w io.Writer) *countingWriter {
	return &countingWriter{Writer: w}
}

func (cw *countingWriter) Write(buf []byte) (int, error) {
	n, err := cw.Writer.Write(buf)
	cw.count += int64(n)
	return n, err
}

// BytesWritten returns the number of bytes written into the underlying writer.
func (cw *countingWriter) BytesWritten() int64 {
	return cw.count
}
