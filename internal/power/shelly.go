package power

import (
	"context"
)

// FansState is the rack-fan plug's reported state.
type FansState struct {
	On bool   `json:"on"`
	IP string `json:"ip"`
}

// ACState is the f-host mains AC plug's reported state (shelly2).
type ACState struct {
	On bool   `json:"on"`
	IP string `json:"ip"`
}

// FansStatus reads the rack-fan Shelly plug's current state.
//
// This is a thin delegate to the FansBackend seam (see backends.go): the
// mechanism that actually reaches the plug -- the HTTP digest-auth RPC --
// lives on the infra.ShellyClient the adapter holds, not on the Engine, so a
// change to how the plug is talked to is an edit there rather than here.
// Engine carries the fan-guard and shutdown policy; the adapter carries the
// I/O.
func (e *Engine) FansStatus(ctx context.Context) (FansState, error) {
	return e.fansBackend().Status(ctx)
}

// FansSet switches the rack-fan plug and returns its state read back from the
// device.
//
// The read-back (and the bounded settle wait behind it) is the adapter's job,
// not the Engine's: Shelly's relay does not flip the instant Switch.Set
// returns, so the adapter polls Status until the plug reports the requested
// state. That settle loop is mechanism-adjacent -- it exists because of how
// the Shelly plug behaves -- so it lives with the ShellyClient in the
// FansBackend adapter (execFans, in backends_exec.go) rather than here. The
// engine calls the seam and gets back a confirmed state.
//
// See FansSet's former doc (now on execFans.Set in backends_exec.go) for why
// the read-back is not belt-and-braces and why a single eager read once
// turned a good switch-on into a failed job on 2026-08-09.
func (e *Engine) FansSet(ctx context.Context, on bool) (FansState, error) {
	return e.fansBackend().Set(ctx, on)
}

// ACStatus reads the f-host mains AC Shelly plug's current state.
//
// Same seam shape as FansStatus; a different adapter and IP so fan and AC
// never share a client. Unlike the fans, AC is never flipped by power on/off
// or boot -- only by an explicit `f3sctl ac` / POST /ac call.
func (e *Engine) ACStatus(ctx context.Context) (ACState, error) {
	return e.acBackend().Status(ctx)
}

// ACSet switches the f-host mains AC plug and returns its state read back
// from the device. Same settle semantics as FansSet (see execAC.Set).
func (e *Engine) ACSet(ctx context.Context, on bool) (ACState, error) {
	return e.acBackend().Set(ctx, on)
}
