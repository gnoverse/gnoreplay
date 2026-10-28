package main

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// reportsMaxSize bounds the space reports take on disk: past it, the oldest
// are deleted.
var reportsMaxSize int64 = 1 << 30

// pruneReports deletes the oldest reports past reportsMaxSize, except each
// branch's latest push replay: new replays are classified against it. Their
// jobs' pages, rendered when they finished, remain. Reports saved before
// reports were gzipped are gzipped here, those that are kept.
func (s *Server) pruneReports(ctx context.Context) {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	jobs, err := s.queue.Reports()
	if err != nil {
		s.logger.Error("list reports", "err", err)
		return
	}
	var total int64
	branches := map[string]bool{}
	for _, j := range jobs { // newest first
		if ctx.Err() != nil {
			return
		}
		baseline := false
		if key := j.Repo + " " + j.Branch; j.Event == eventPush && !branches[key] {
			branches[key], baseline = true, true
		}
		fi, err := os.Stat(j.ReportPath)
		if err != nil {
			continue // pruned already
		}
		size := fi.Size()
		if !strings.HasSuffix(j.ReportPath, ".gz") && (baseline || total < reportsMaxSize) {
			if gz, err := s.compressReport(j); err != nil {
				s.logger.Error("compress report", "job", j.ID, "err", err)
			} else {
				s.logger.Info("report compressed", "job", j.ID, "size", size, "compressed", gz)
				size = gz
			}
		}
		total += size
		if total <= reportsMaxSize || baseline {
			continue
		}
		if err := os.Remove(j.ReportPath); err != nil {
			s.logger.Error("prune report", "job", j.ID, "err", err)
			continue
		}
		s.logger.Info("report pruned", "job", j.ID, "size", size)
	}
}

// compressReport gzips a report saved before reports were, and returns its
// new size.
func (s *Server) compressReport(j *Job) (int64, error) {
	path := j.ReportPath + ".gz"
	if err := compressFile(j.ReportPath, path); err != nil {
		return 0, err
	}
	fi, err := os.Stat(path)
	if err == nil {
		err = s.queue.SetReportPath(j.ID, path)
	}
	if err != nil {
		os.Remove(path)
		return 0, err
	}
	os.Remove(j.ReportPath)
	j.ReportPath = path
	return fi.Size(), nil
}

// compressFile writes src gzipped to dst, which appears whole or not at all.
func compressFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(out.Name(), dst)
	}
	if err != nil {
		os.Remove(out.Name())
	}
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
