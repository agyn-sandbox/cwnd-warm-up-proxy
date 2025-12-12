package testserver

import (
	"sync/atomic"
	"time"
)

type Stats struct {
	totalUploads atomic.Uint64
	totalBytes   atomic.Uint64
	lastUpload   atomic.Value
}

type uploadSummary struct {
	Size        uint64
	Duration    time.Duration
	CompletedAt time.Time
}

type Snapshot struct {
	TotalUploads uint64
	TotalBytes   uint64
	LastSize     uint64
	LastDuration time.Duration
	LastTime     time.Time
}

func NewStats() *Stats {
	s := &Stats{}
	s.lastUpload.Store(uploadSummary{})
	return s
}

func (s *Stats) RecordUpload(size uint64, duration time.Duration, completedAt time.Time) {
	s.totalUploads.Add(1)
	s.totalBytes.Add(size)
	s.lastUpload.Store(uploadSummary{Size: size, Duration: duration, CompletedAt: completedAt})
}

func (s *Stats) TotalUploads() uint64 {
	return s.totalUploads.Load()
}

func (s *Stats) TotalBytes() uint64 {
	return s.totalBytes.Load()
}

func (s *Stats) Snapshot() Snapshot {
	summary := s.lastUpload.Load().(uploadSummary)
	return Snapshot{
		TotalUploads: s.totalUploads.Load(),
		TotalBytes:   s.totalBytes.Load(),
		LastSize:     summary.Size,
		LastDuration: summary.Duration,
		LastTime:     summary.CompletedAt,
	}
}
