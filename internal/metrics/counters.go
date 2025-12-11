package metrics

import "sync/atomic"

type Counters struct {
	DummyTx atomic.Uint64
	DummyRx atomic.Uint64
	RealTx  atomic.Uint64
	RealRx  atomic.Uint64
}

func (c *Counters) AddDummyTx(n uint64) {
	c.DummyTx.Add(n)
}

func (c *Counters) AddDummyRx(n uint64) {
	c.DummyRx.Add(n)
}

func (c *Counters) AddRealTx(n uint64) {
	c.RealTx.Add(n)
}

func (c *Counters) AddRealRx(n uint64) {
	c.RealRx.Add(n)
}

type Snapshot struct {
	DummyTx uint64
	DummyRx uint64
	RealTx  uint64
	RealRx  uint64
}

func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		DummyTx: c.DummyTx.Load(),
		DummyRx: c.DummyRx.Load(),
		RealTx:  c.RealTx.Load(),
		RealRx:  c.RealRx.Load(),
	}
}
