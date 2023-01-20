package log

// This file implements a sampling logger where the sampling can be turned off.
// It is copied from go.uber.org/zap@v1.24.0/zapcore/sampler.go and then modified.
//
// The reasoning behind maintaining a fork is so that we can use a sampling logger as the root
// logger in the worker, but never sample the log messages generated by user code.  This requires us
// to clone the underlying core with fields, since the user code log messages have datumID, jobID,
// pipelineID fields computed well away from the point where we run user code.
//
// In context.go, we have a log option WithoutRatelimit() that knows how to clone this core and
// disable the rate limit.  The rate limit can be turned back on with that implementation, but I
// hesitate to expose that API surface area.  (jonathan@ 12/14/2022)
//
// Copyright (c) 2016-2022 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

import (
	"time"

	"go.uber.org/atomic"
	"go.uber.org/zap/zapcore"
)

const (
	_minLevel         = zapcore.DebugLevel // Added in our copy.
	_maxLevel         = zapcore.FatalLevel // Added in our copy.
	_numLevels        = _maxLevel - _minLevel + 1
	_countersPerLevel = 4096
)

type counter struct {
	resetAt atomic.Int64
	counter atomic.Uint64
}

type counters [_numLevels][_countersPerLevel]counter

func newCounters() *counters {
	return &counters{}
}

func (cs *counters) get(lvl zapcore.Level, key string) *counter {
	i := lvl - _minLevel
	j := fnv32a(key) % _countersPerLevel
	return &cs[i][j]
}

// fnv32a, adapted from "hash/fnv", but without a []byte(string) alloc
func fnv32a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	hash := uint32(offset32)
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= prime32
	}
	return hash
}

func (c *counter) IncCheckReset(t time.Time, tick time.Duration) uint64 {
	tn := t.UnixNano()
	resetAfter := c.resetAt.Load()
	if resetAfter > tn {
		return c.counter.Inc()
	}

	c.counter.Store(1)

	newResetAfter := tn + tick.Nanoseconds()
	if !c.resetAt.CAS(resetAfter, newResetAfter) {
		// We raced with another goroutine trying to reset, and it also reset
		// the counter to 1, so we need to reincrement the counter.
		return c.counter.Inc()
	}

	return 1
}

// optionFunc wraps a func so it satisfies the SamplerOption interface.
type optionFunc func(*sampler)

func (f optionFunc) apply(s *sampler) {
	f(s)
}

// samplerOption configures a Sampler.
type samplerOption interface {
	apply(*sampler)
}

// nopSamplingHook is the default hook used by sampler.
func nopSamplingHook(zapcore.Entry, zapcore.SamplingDecision) {}

// samplerHook registers a function  which will be called when Sampler makes a
// decision.
//
// This hook may be used to get visibility into the performance of the sampler.
// For example, use it to track metrics of dropped versus sampled logs.
//
//	var dropped atomic.Int64
//	samplerHook(func(ent zapcore.Entry, dec zapcore.SamplingDecision) {
//	  if dec&zapcore.LogDropped > 0 {
//	    dropped.Inc()
//	  }
//	})
func samplerHook(hook func(entry zapcore.Entry, dec zapcore.SamplingDecision)) samplerOption {
	return optionFunc(func(s *sampler) {
		s.hook = hook
	})
}

// newSamplerWithOptions creates a Core that samples incoming entries, which
// caps the CPU and I/O load of logging while attempting to preserve a
// representative subset of your logs.
//
// Zap samples by logging the first N entries with a given level and message
// each tick. If more Entries with the same level and message are seen during
// the same interval, every Mth message is logged and the rest are dropped.
//
// For example,
//
//	core = NewSamplerWithOptions(core, time.Second, 10, 5)
//
// This will log the first 10 log entries with the same level and message
// in a one second interval as-is. Following that, it will allow through
// every 5th log entry with the same level and message in that interval.
//
// If thereafter is zero, the Core will drop all log entries after the first N
// in that interval.
//
// Sampler can be configured to report sampling decisions with the SamplerHook
// option.
//
// Keep in mind that Zap's sampling implementation is optimized for speed over
// absolute precision; under load, each tick may be slightly over- or
// under-sampled.
//
// NOTE(jonathan): This fork has a samplingEnabled option; if sampling is disabled, then no rate
// limiting occurs.  We also don't call the sampling hook if sampling is disabled, even though we
// technically made the decision not to sample the log entry.
func newSamplerWithOptions(core zapcore.Core, samplingEnabled bool, tick time.Duration, first, thereafter int, opts ...samplerOption) zapcore.Core {
	s := &sampler{
		Core:            core,
		tick:            tick,
		counts:          newCounters(),
		first:           uint64(first),
		thereafter:      uint64(thereafter),
		hook:            nopSamplingHook,
		samplingEnabled: samplingEnabled,
	}
	for _, opt := range opts {
		opt.apply(s)
	}

	return s
}

type sampler struct {
	zapcore.Core

	counts            *counters
	tick              time.Duration
	first, thereafter uint64
	hook              func(zapcore.Entry, zapcore.SamplingDecision)

	samplingEnabled bool // Added to this copy.
}

var (
	_ zapcore.Core         = (*sampler)(nil)
	_ zapcore.LevelEnabler = (*sampler)(nil)
)

func (s *sampler) Level() zapcore.Level {
	return zapcore.LevelOf(s.Core)
}

func (s *sampler) With(fields []Field) zapcore.Core {
	return &sampler{
		samplingEnabled: s.samplingEnabled,
		Core:            s.Core.With(fields),
		tick:            s.tick,
		counts:          s.counts,
		first:           s.first,
		thereafter:      s.thereafter,
		hook:            s.hook,
	}
}

func (s *sampler) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !s.Enabled(ent.Level) {
		return ce
	}

	if s.samplingEnabled {
		if ent.Level >= _minLevel && ent.Level <= _maxLevel {
			counter := s.counts.get(ent.Level, ent.Message)
			n := counter.IncCheckReset(ent.Time, s.tick)
			if n > s.first && (s.thereafter == 0 || (n-s.first)%s.thereafter != 0) {
				s.hook(ent, zapcore.LogDropped)
				return ce
			}
			s.hook(ent, zapcore.LogSampled)
		}
	}
	return s.Core.Check(ent, ce)
}

// cloneWithSampling allows you to clone a zap core and change whether or not it is a sampling core.
// If the provided zapcore.Core is not a *sampler, then the core is returned unmodified, with ok set
// to false.
func cloneWithSampling(core zapcore.Core, samplingEnabled bool) (_ zapcore.Core, ok bool) {
	if s, ok := core.(*sampler); ok {
		return &sampler{
			samplingEnabled: samplingEnabled,
			Core:            s.Core,
			tick:            s.tick,
			counts:          s.counts,
			first:           s.first,
			thereafter:      s.thereafter,
			hook:            s.hook,
		}, true
	}
	return core, false
}